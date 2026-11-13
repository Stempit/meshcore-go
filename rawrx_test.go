package meshcore

import (
	"testing"
)

// logRxFrame wraps a raw RF packet body into a 0x88 LOG_RX_DATA companion
// frame the way the firmware does: [0x88][snr*4][rssi][raw…].
func logRxFrame(raw []byte) []byte {
	return append([]byte{0x88, 0x20, 0xd8}, raw...)
}

// TestOnRawDataFiresPerReception pins the OnRawData contract: the callback
// fires for every 0x88 frame — pre-dedup, pre-correlation — and each copy's
// raw bytes reach the callback unchanged. Registering the observer must not
// disturb the decoded-message path correlation (takeDmRaw / pendingRaw).
func TestOnRawDataFiresPerReception(t *testing.T) {
	c := NewMeshCoreClient(nil)

	var seen []RawRx
	c.onRawData = func(rx RawRx) { seen = append(seen, rx) }

	p1 := rawTXTMSG([]byte{0x11}, []byte{0xAA})
	p2 := rawTXTMSG([]byte{0x11}, []byte{0xAA, 0xBB}) // same sender, longer path
	p3 := rawGRPXTX(make([]byte, 16), []byte{0xCC})
	c.parseLogRxData(logRxFrame(p1))
	c.parseLogRxData(logRxFrame(p2))
	c.parseLogRxData(logRxFrame(p3))

	if len(seen) != 3 {
		t.Fatalf("callback fired %d times, want 3", len(seen))
	}
	for i, want := range [][]byte{p1, p2, p3} {
		if string(seen[i].Data) != string(want) {
			t.Errorf("copy %d: data=%x want %x", i, seen[i].Data, want)
		}
		if seen[i].At.IsZero() {
			t.Errorf("copy %d: At not set", i)
		}
	}

	// Correlation is untouched: the decoded DM still takes its path from its
	// own TXT_MSG raw packet even with an OnRawData observer registered.
	var got *Message
	c.onMessage = func(m *Message) { got = m }
	c.parseContactMessageV3(v3DMFrame([]byte{0x11, 0x11, 0x11, 0x11, 0x11, 0x11}, 1700000001, "hello"))
	if got == nil || got.PathLength != 1 || len(got.PathBytes) != 1 || got.PathBytes[0] != 0xAA {
		t.Fatalf("DM correlation broken with OnRawData set: %+v", got)
	}
}

// TestParseRawPacket pins the header walk: flood vs transport-coded routes,
// hash sizes 1–4 bytes per hop, the zero-path case, and the malformed inputs
// that must be rejected rather than guessed at.
func TestParseRawPacket(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	tests := []struct {
		name    string
		data    []byte
		ptype   byte
		path    []byte
		hops    int
		payload []byte
		ok      bool
	}{
		{
			name:    "TXT_MSG direct, two 1-byte hops",
			data:    append([]byte{0x0A, 0x02, 0xAA, 0xBB}, payload...),
			ptype:   payloadTypeTxtMsg,
			path:    []byte{0xAA, 0xBB},
			hops:    2,
			payload: payload,
			ok:      true,
		},
		{
			name:    "GRP_TXT flood, three 2-byte hops (path_len 0x43)",
			data:    append([]byte{0x15, 0x43, 0xA1, 0xA2, 0xB1, 0xB2, 0xC1, 0xC2}, payload...),
			ptype:   payloadTypeGrpTxt,
			path:    []byte{0xA1, 0xA2, 0xB1, 0xB2, 0xC1, 0xC2},
			hops:    3,
			payload: payload,
			ok:      true,
		},
		{
			name:    "transport-coded route, one 3-byte hop",
			data:    append([]byte{0x0B, 0x01, 0x02, 0x03, 0x04, byte(1 | (2 << 6)), 0xA1, 0xA2, 0xA3}, payload...),
			ptype:   payloadTypeTxtMsg,
			path:    []byte{0xA1, 0xA2, 0xA3},
			hops:    1,
			payload: payload,
			ok:      true,
		},
		{
			name:    "zero hops: no path, payload intact",
			data:    append([]byte{0x15, 0x00}, payload...),
			ptype:   payloadTypeGrpTxt,
			path:    nil,
			hops:    0,
			payload: payload,
			ok:      true,
		},
		{
			name: "unsupported payload version rejected",
			// header 0x4A: version bits (6–7) = 01, otherwise a valid TXT_MSG direct.
			data: append([]byte{0x4A, 0x02, 0xAA, 0xBB}, payload...),
			ok:   false,
		},
		{
			name: "reserved path mode 3 rejected",
			// path_len 0xC3: mode 3, 3 hops × 4 bytes; well-formed otherwise.
			data: append(append([]byte{0x0A, 0xC3}, make([]byte, 12)...), payload...),
			ok:   false,
		},
		{
			name: "path longer than MAX_PATH_SIZE rejected",
			// path_len 0x61: 33 hops × 2 bytes = 66 path bytes > 64, all present.
			data: append(append([]byte{0x15, 0x61}, make([]byte, 66)...), payload...),
			ok:   false,
		},
		{
			name: "truncated path rejected",
			data: []byte{0x0A, 0x03, 0xAA, 0xBB},
			ok:   false,
		},
		{
			name: "transport header cut short rejected",
			data: []byte{0x0B, 0x01, 0x02},
			ok:   false,
		},
		{
			name: "too short rejected",
			data: []byte{0x0A},
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ptype, path, hops, payload, ok := ParseRawPacket(tc.data)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if ptype != tc.ptype || hops != tc.hops {
				t.Fatalf("ptype = %#x (want %#x), hops = %d (want %d)", ptype, tc.ptype, hops, tc.hops)
			}
			// nil-ness is part of the contract: zero hops must give nil, not an
			// empty slice — string() comparison alone can't tell them apart.
			if (path == nil) != (tc.path == nil) {
				t.Fatalf("path = %x (nil: %v), want nil: %v", path, path == nil, tc.path == nil)
			}
			if string(path) != string(tc.path) {
				t.Fatalf("path = %x, want %x", path, tc.path)
			}
			if string(payload) != string(tc.payload) {
				t.Fatalf("payload = %x, want %x", payload, tc.payload)
			}
		})
	}
}

// TestParseRawPacketPayloadStableAcrossPathGrowth pins the property the
// repeat watch and MQTT dedup rely on: the same packet retransmitted with a
// longer accumulated path yields the identical payload — and therefore the
// same packetHash.
func TestParseRawPacketPayloadStableAcrossPathGrowth(t *testing.T) {
	body := []byte{0x42, 0x11, 0x01, 0x02, 0xC3} // dest, src, MAC, ciphertext placeholder
	one := append([]byte{0x0A, 0x01, 0xAA}, body...)
	two := append([]byte{0x0A, 0x02, 0xAA, 0xBB}, body...)

	_, _, _, p1, ok1 := ParseRawPacket(one)
	_, _, _, p2, ok2 := ParseRawPacket(two)
	if !ok1 || !ok2 || string(p1) != string(p2) {
		t.Fatalf("payload changed with path growth: %x vs %x (ok %v/%v)", p1, p2, ok1, ok2)
	}
}

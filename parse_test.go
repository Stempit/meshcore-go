package meshcore

import (
	"encoding/binary"
	"testing"
	"time"
)

func pad(b string, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

// statsFrame builds a respCodeStats frame body of the given type.
func statsFrame(statsType byte, payload ...byte) []byte {
	return append([]byte{respCodeStats, statsType}, payload...)
}

// TestStatsSnapshotNotAliased pins the ownership rule: onStats and GetStats
// must hand out snapshots — a callback that keeps its argument must not see
// later frames mutate it, and the two readers must never alias the live struct.
func TestStatsSnapshotNotAliased(t *testing.T) {
	c := NewMeshCoreClient(nil)
	var cb, first *CompanionRadioStats
	c.onStats = func(s *CompanionRadioStats) {
		if first == nil {
			first = s // remember the first delivery; later frames must not touch it
		}
		cb = s
	}

	c.parseStats(statsFrame(STATS_TYPE_CORE, 0x39, 0x05, 1, 0, 0, 0, 0, 0, 3))
	if cb == nil {
		t.Fatal("onStats not called")
	}
	if got := c.GetStats(); got == nil || got.BatteryMv != 0x0539 {
		t.Fatalf("GetStats: %+v", got)
	}
	if got := c.GetStats(); got == cb {
		t.Fatal("GetStats and onStats alias the same pointer")
	}

	// A second frame must not reach into previously delivered snapshots.
	c.parseStats(statsFrame(STATS_TYPE_CORE, 0x11, 0x02, 1, 0, 0, 0, 0, 0, 4))
	if first.BatteryMv != 0x0539 {
		t.Fatalf("first callback snapshot mutated by a later frame: %+v", first)
	}
}

// TestStatsConcurrentReaders is meant to run under -race: parseStats writes
// the shared stats while GetStats/FormatStats read them.
func TestStatsConcurrentReaders(t *testing.T) {
	c := NewMeshCoreClient(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		frame := statsFrame(STATS_TYPE_RADIO, make([]byte, 12)...)
		for i := 0; i < 1000; i++ {
			c.parseStats(frame)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = c.GetStats()
		_ = c.FormatStats()
		select {
		case <-done:
			return
		case <-time.After(50 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("writer goroutine did not finish")
			}
		}
	}
}

// ─── Golden frames ────────────────────────────────────────────────────────────
// Each test feeds one wire-format frame documented from the firmware and
// asserts the decoded state and callback, so a framing change cannot pass
// silently.

func selfInfoFrame(name string) []byte {
	f := []byte{respCodeSelfInfo, 0x01, 10, 22} // code, adv_type, tx, max_tx
	f = append(f, pad("", 32)...)               // public key
	f = binary.LittleEndian.AppendUint32(f, 0)  // lat
	f = binary.LittleEndian.AppendUint32(f, 0)  // lon
	f = append(f, 0, 0, 0x03, 1)                // multi_acks, adv_loc, tele_flags, manual_add
	f = binary.LittleEndian.AppendUint32(f, 869525000)
	f = binary.LittleEndian.AppendUint32(f, 250000)
	f = append(f, 9, 5) // sf, cr
	return append(append(f, name...), 0)
}

func TestParseSelfInfoGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	var got *SelfInfo
	c.onSelfInfo = func(i *SelfInfo) { got = i }

	c.parseSelfInfo(selfInfoFrame("self"))

	if got == nil {
		t.Fatal("onSelfInfo not called")
	}
	if got.TxPower != 10 || got.MaxTxPower != 22 {
		t.Errorf("tx power: %+v", got)
	}
	if got.FreqHz != 869525000 || got.BwHz != 250000 || got.Sf != 9 || got.Cr != 5 {
		t.Errorf("radio params: %+v", got)
	}
	if got.Name != "self" || !got.ManualAdd || got.TeleModeBase != 3 {
		t.Errorf("misc fields: %+v", got)
	}
	if c.selfInfo != got {
		t.Error("selfInfo not stored")
	}
}

func TestParseChannelInfoGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	psk := pad("0123456789abcdef", 16)

	f := append([]byte{respCodeChannelInfo, 2}, pad("chat", 32)...)
	f = append(f, psk...)
	c.parseChannelInfo(f)

	ch := c.channels[2]
	if ch == nil || ch.Name != "chat" || ch.IsEmpty {
		t.Fatalf("channel: %+v", ch)
	}
	if string(ch.PSK) != "0123456789abcdef" {
		t.Fatalf("psk: %x", ch.PSK)
	}

	// An empty name marks an empty slot even though the PSK field is present.
	f = append([]byte{respCodeChannelInfo, 3}, pad("", 32)...)
	f = append(f, psk...)
	c.parseChannelInfo(f)
	if ch := c.channels[3]; ch == nil || !ch.IsEmpty || ch.Name != "" {
		t.Fatalf("empty slot: %+v", ch)
	}
}

func TestParseContactGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	var got *Contact
	c.onContact = func(ct *Contact) { got = ct }

	// path_len 0x42 = 2 hops × 2 bytes per hop; only the first 4 bytes count.
	c.parseContact(contactFrame(key, "node", 0x42, []byte{1, 2, 3, 4, 0xFF, 0xFF}))

	if got == nil {
		t.Fatal("onContact not called")
	}
	if got.Name != "node" || got.PathLength != 0x42 {
		t.Fatalf("contact: %+v", got)
	}
	if len(got.Path) != 4 || got.Path[0] != 1 || got.Path[3] != 4 {
		t.Fatalf("path: %x", got.Path)
	}
	if got.LastMod != 1700000001 {
		t.Fatalf("lastmod: %d", got.LastMod)
	}
	if c.syncLastmod != 1700000001 {
		t.Fatalf("syncLastmod: %d", c.syncLastmod)
	}
}

func deviceInfoFrame() []byte {
	f := []byte{respCodeDeviceInfo, 14, 150, 8}
	f = binary.LittleEndian.AppendUint32(f, 123456)
	f = append(f, pad("build1234567", 12)...)
	f = append(f, pad("Heltec V3", 40)...)
	f = append(f, pad("1.7.2", 20)...)
	return f
}

func TestParseDeviceInfoGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	c.selfInfo = &SelfInfo{}

	c.parseDeviceInfo(deviceInfoFrame())

	if c.selfInfo.FirmwareVersion != "1.7.2" || c.selfInfo.Model != "Heltec V3" {
		t.Fatalf("selfInfo not updated: %+v", c.selfInfo)
	}
}

func TestParseTraceDataGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	var got *TraceData
	c.onTraceData = func(td *TraceData) { got = td }

	f := []byte{pushCodeTraceData, 0, 3, 0}
	f = binary.LittleEndian.AppendUint32(f, 0x11223344)
	f = binary.LittleEndian.AppendUint32(f, 0x55667788)
	f = append(f, 1, 2, 3)       // path hashes
	f = append(f, 40, 0xF8, 100) // per-hop SNR×4 (0xF8 = int8 -8)
	f = append(f, 20)            // final SNR×4

	c.parseTraceData(f)

	if got == nil {
		t.Fatal("onTraceData not called")
	}
	if got.Tag != 0x11223344 || got.AuthCode != 0x55667788 {
		t.Fatalf("tag/auth: %+v", got)
	}
	if len(got.PathHashes) != 3 || got.PathHashes[2] != 3 {
		t.Fatalf("hashes: %x", got.PathHashes)
	}
	if got.PathSNRs[0] != 10 || got.PathSNRs[1] != -2 || got.PathSNRs[2] != 25 || got.FinalSNR != 5 {
		t.Fatalf("snrs: %v final %v", got.PathSNRs, got.FinalSNR)
	}
}

func TestParseChannelDataGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	var got *ChannelDataMessage
	c.onChannelData = func(m *ChannelDataMessage) { got = m }

	f := []byte{respCodeChannelDataRecv, 0x28, 0, 0, 5, 0x01, 0x34, 0x12, 2}
	f = append(f, 0xDE, 0xAD)

	c.parseChannelData(f)

	if got == nil {
		t.Fatal("onChannelData not called")
	}
	if got.ChannelIndex != 5 || got.DataType != 0x1234 || got.PathLength != 1 {
		t.Fatalf("header fields: %+v", got)
	}
	if len(got.Data) != 2 || got.Data[0] != 0xDE || got.SNR != 10 {
		t.Fatalf("payload/snr: %+v", got)
	}
}

func TestParseBatteryAndStorageGolden(t *testing.T) {
	c := NewMeshCoreClient(nil)
	var mv, used, total int
	c.onBattery = func(m int, u, tt int) { mv, used, total = m, u, tt }

	f := []byte{respCodeBattAndStorage}
	f = binary.LittleEndian.AppendUint16(f, 4200)
	f = binary.LittleEndian.AppendUint32(f, 100)
	f = binary.LittleEndian.AppendUint32(f, 8000)

	c.parseBatteryAndStorage(f)

	if mv != 4200 || used != 100 || total != 8000 {
		t.Fatalf("battery: %d %d %d", mv, used, total)
	}
}

// TestHandleFrameContactDispatch pins the V2/V3 contact dispatch: a full
// 148-byte frame parses as V2, a short frame refreshes via V3 — which must
// keep the full entry (see TestContactV3RefreshKeepsFullContact).
func TestHandleFrameContactDispatch(t *testing.T) {
	c := NewMeshCoreClient(nil)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	c.handleFrame(contactFrame(key, "full", 0x00, nil))
	if got := c.contacts[pubKeyHex(key)]; got == nil || got.Name != "full" {
		t.Fatalf("full frame mishandled: %+v", got)
	}

	c.handleFrame(append(append([]byte{respCodeContact}, key[:6]...), "short"...))
	if got := c.contacts[pubKeyHex(key)]; got == nil || got.Name != "short" || got.PathLength != 0x00 || got.LastMod != 1700000001 {
		t.Fatalf("V3 refresh clobbered the full contact: %+v", got)
	}
}

// ─── Raw-packet and helper units ─────────────────────────────────────────────

func TestExtractPathFromRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		path []byte
		hops int
	}{
		{"flood, 1-byte hops", []byte{0x15, 0x02, 0xAA, 0xBB, 0x01}, []byte{0xAA, 0xBB}, 2},
		{"direct route", []byte{0x0A, 0x01, 0xEE, 0x42, 0x11}, []byte{0xEE}, 1},
		{"transport codes", []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x02, 0xAA, 0xBB}, []byte{0xAA, 0xBB}, 2},
		{"multi-byte hops", []byte{0x15, 0x42, 1, 2, 3, 4}, []byte{1, 2, 3, 4}, 2},
		{"zero hops", []byte{0x15, 0x00, 0x01}, nil, 0},
		{"truncated path", []byte{0x15, 0x04, 0xAA}, nil, 0},
		{"too short", []byte{0x15}, nil, 0},
	}
	for _, tc := range cases {
		path, hops := extractPathFromRaw(tc.raw)
		if hops != tc.hops || string(path) != string(tc.path) {
			t.Errorf("%s: got (%x, %d), want (%x, %d)", tc.name, path, hops, tc.path, tc.hops)
		}
	}
}

func TestPathRenderHelpers(t *testing.T) {
	if got := decodeMsgHops(0xFF); got != pathLengthFlood {
		t.Errorf("decodeMsgHops(0xFF) = %d", got)
	}
	if got := decodeMsgHops(0x42); got != 2 {
		t.Errorf("decodeMsgHops(0x42) = %d", got)
	}
	if got := displayHops(0xFF); got != 0 {
		t.Errorf("displayHops(0xFF) = %d", got)
	}
	if got := pathHex([]byte{0xAA, 0xBB}, 2); got != "aa:bb" {
		t.Errorf("pathHex 1-byte hops = %q", got)
	}
	if got := pathHex([]byte{1, 2, 3, 4}, 2); got != "0102:0304" {
		t.Errorf("pathHex 2-byte hops = %q", got)
	}
	if got := pathHex(nil, 2); got != "n/a" {
		t.Errorf("pathHex empty = %q", got)
	}
	if got := formatPath(pathLengthFlood, nil); got != "flood" {
		t.Errorf("formatPath flood = %q", got)
	}
	if got := formatPath(0, nil); got != "direct" {
		t.Errorf("formatPath direct = %q", got)
	}
	if got := formatPath(0x42, []byte{1, 2, 3, 4}); got != "0102:0304" {
		t.Errorf("formatPath 2-byte hops = %q", got)
	}
}

func TestIsDuplicateMessageTTL(t *testing.T) {
	c := NewMeshCoreClient(nil)
	c.seenMsgs = map[string]time.Time{
		"expired": time.Now().Add(-2 * dedupTTL),
	}
	if c.isDuplicateMessage("expired") {
		t.Fatal("expired entry treated as duplicate")
	}
	if c.isDuplicateMessage("fresh") {
		t.Fatal("first sighting treated as duplicate")
	}
	if !c.isDuplicateMessage("fresh") {
		t.Fatal("second sighting not deduplicated within TTL")
	}
}

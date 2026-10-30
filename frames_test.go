package meshcore

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// TestHandleFrameShortRawDataDoesNotPanic pins the robustness rule: a frame
// handler must survive malformed input, because the stream parser accepts any
// frame length ≥ 1 and one panic kills the whole host process.
func TestHandleFrameShortRawDataDoesNotPanic(t *testing.T) {
	c := NewMeshCoreClient(nil)
	for _, frame := range [][]byte{
		{pushCodeRawData},
		{pushCodeRawData, 0x10},
		{pushCodeRawData, 0x10, 0xd0}, // well-formed length: goes the full path
	} {
		c.handleFrame(frame) // must not panic
	}
}

// v3ChannelFrame builds a respCodeChannelMsgRecvV3 frame body.
func v3ChannelFrame(channelIndex int, timestamp uint32, text string) []byte {
	data := []byte{respCodeChannelMsgRecvV3, 0x28, 0, 0, byte(channelIndex), 0x02, txtTypePlain}
	data = binary.LittleEndian.AppendUint32(data, timestamp)
	return append(data, text...)
}

// rawGRPXTX builds a raw 0x88 RF packet body for a GRP_TXT channel message:
// [header=flood|GRP_TXT][path_len][path…][channel_hash][mac(2)][ciphertext…].
func rawGRPXTX(psk []byte, path []byte) []byte {
	sum := sha256.Sum256(psk[:16])
	raw := []byte{0x15, byte(len(path))}
	raw = append(raw, path...)
	raw = append(raw, sum[0], 0x01, 0x02) // channel hash + MAC placeholder
	return append(raw, 0xC3, 0xC3)        // ciphertext placeholder
}

// TestChannelMessageCallbackSeesFinalMessage pins the ordering rule: the
// onChannelMessage callback must observe the same complete message the store
// and IncomingMessages consumers see — path fields included — and nothing may
// mutate the struct after the callback fired.
func TestChannelMessageCallbackSeesFinalMessage(t *testing.T) {
	c := NewMeshCoreClient(nil)
	psk := make([]byte, 16)
	c.channels[3] = &Channel{Index: 3, Name: "chat", PSK: psk}

	c.parseLogRxData(append([]byte{0x88, 0x20, 0xd8}, rawGRPXTX(psk, []byte{0xAA, 0xBB})...))

	// Assert INSIDE the callback: a post-callback mutation of the same pointer
	// would be invisible after parseChannelMessageV3 returns.
	var got *ChannelMessage
	c.onChannelMessage = func(m *ChannelMessage) {
		if len(m.PathBytes) != 2 || m.PathBytes[0] != 0xAA || m.PathBytes[1] != 0xBB || m.PathLength != 2 {
			t.Errorf("callback saw incomplete message: path=%x pathLen=%d", m.PathBytes, m.PathLength)
		}
		got = m
	}
	c.parseChannelMessageV3(v3ChannelFrame(3, 1700000000, "Bot: hello"))

	if got == nil {
		t.Fatal("onChannelMessage not called")
	}
	if len(got.PathBytes) != 2 || got.PathBytes[0] != 0xAA || got.PathBytes[1] != 0xBB || got.PathLength != 2 {
		t.Fatalf("callback saw incomplete message: path=%x pathLen=%d", got.PathBytes, got.PathLength)
	}
	stored := c.channelMsgs[3][0]
	if stored != got || len(stored.PathBytes) != 2 {
		t.Fatalf("stored message diverges from the callback's view: %+v", stored)
	}
}

// The V1 parser must satisfy the same ordering rule.
func TestChannelMessageV1CallbackSeesFinalMessage(t *testing.T) {
	c := NewMeshCoreClient(nil)
	psk := make([]byte, 16)
	c.channels[1] = &Channel{Index: 1, Name: "chat", PSK: psk}

	c.parseLogRxData(append([]byte{0x88, 0x20, 0xd8}, rawGRPXTX(psk, []byte{0xCC})...))

	var got *ChannelMessage
	c.onChannelMessage = func(m *ChannelMessage) {
		if len(m.PathBytes) != 1 || m.PathBytes[0] != 0xCC || m.PathLength != 1 {
			t.Errorf("callback saw incomplete message: path=%x pathLen=%d", m.PathBytes, m.PathLength)
		}
		got = m
	}
	frame := append([]byte{respCodeChannelMsgRecv, 1, 0x01, txtTypePlain}, 0, 0, 0, 0)
	frame = append(frame, "Bot: hi"...)
	c.parseChannelMessage(frame)

	if got == nil || len(got.PathBytes) != 1 || got.PathBytes[0] != 0xCC || got.PathLength != 1 {
		t.Fatalf("callback saw incomplete message: %+v", got)
	}
}

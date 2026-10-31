package meshcore

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
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

// rawTXTMSG builds a raw 0x88 RF packet body for a TXT_MSG direct message:
// [header=direct|TXT_MSG][path_len][path…][dest_hash][src_hash][mac(2)][ct…].
func rawTXTMSG(senderPrefix []byte, path []byte) []byte {
	raw := []byte{0x0A, byte(len(path))}
	raw = append(raw, path...)
	raw = append(raw, 0x42, senderPrefix[0], 0x01, 0x02) // dest hash, src hash, MAC
	return append(raw, 0xC3)                             // ciphertext placeholder
}

// v3DMFrame builds a respCodeContactMsgRecvV3 frame body.
func v3DMFrame(prefix []byte, timestamp uint32, text string) []byte {
	data := []byte{respCodeContactMsgRecvV3, 0x20, 0, 0}
	data = append(data, prefix...)
	data = append(data, 0x02, txtTypePlain) // path_len metadata, txt_type
	data = binary.LittleEndian.AppendUint32(data, timestamp)
	return append(data, text...)
}

// TestDMCorrelationMatchesTxtMsgRaw pins the DM-side correlation rule: a DM
// must take its path from its own TXT_MSG raw packet, not from an unrelated
// packet that merely arrived earlier — the DM counterpart of the channel
// hash-matched correlation.
func TestDMCorrelationMatchesTxtMsgRaw(t *testing.T) {
	c := NewMeshCoreClient(nil)
	prefix := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}

	// An unrelated channel packet arrives first — order-only matching would
	// lend it (and its path) to the DM.
	c.parseLogRxData(append([]byte{0x88, 0x20, 0xd8}, rawGRPXTX(make([]byte, 16), []byte{0xDD})...))
	c.parseLogRxData(append([]byte{0x88, 0x20, 0xd8}, rawTXTMSG(prefix, []byte{0xEE})...))

	var got *Message
	c.onMessage = func(m *Message) {
		if len(m.PathBytes) != 1 || m.PathBytes[0] != 0xEE || m.PathLength != 1 {
			t.Errorf("DM took the wrong raw packet's path: path=%x pathLen=%d", m.PathBytes, m.PathLength)
		}
		got = m
	}
	c.parseContactMessageV3(v3DMFrame(prefix, 1700000001, "hello"))

	if got == nil {
		t.Fatal("onMessage not called")
	}
}

// TestChannelV1CorrelationMatchesPSK pins the V1 channel correlation rule: a
// channel-A message must take the raw packet of channel A, not the first raw
// in the pending queue.
func TestChannelV1CorrelationMatchesPSK(t *testing.T) {
	c := NewMeshCoreClient(nil)
	pskA := make([]byte, 16)
	pskA[0] = 0x0A
	pskB := make([]byte, 16)
	pskB[0] = 0x0B
	c.channels[0] = &Channel{Index: 0, Name: "A", PSK: pskA}
	c.channels[1] = &Channel{Index: 1, Name: "B", PSK: pskB}

	// Channel B's raw arrives first; the channel-A message must not take it.
	c.parseLogRxData(append([]byte{0x88, 0x20, 0xd8}, rawGRPXTX(pskB, []byte{0x0B, 0x0B})...))
	c.parseLogRxData(append([]byte{0x88, 0x20, 0xd8}, rawGRPXTX(pskA, []byte{0x0A, 0x0A, 0x0A})...))

	var got *ChannelMessage
	c.onChannelMessage = func(m *ChannelMessage) {
		if len(m.PathBytes) != 3 || m.PathLength != 3 {
			t.Errorf("channel A message took the wrong raw packet's path: path=%x pathLen=%d", m.PathBytes, m.PathLength)
		}
		got = m
	}
	frame := append([]byte{respCodeChannelMsgRecv, 0, 0x03, txtTypePlain}, 0, 0, 0, 0)
	frame = append(frame, "Bot: hi"...)
	c.parseChannelMessage(frame)

	if got == nil {
		t.Fatal("onChannelMessage not called")
	}
}

// contactFrame builds a full 148-byte RESP_CODE_CONTACT frame body.
func contactFrame(pubKey []byte, name string, pathLen byte, path []byte) []byte {
	f := append([]byte{respCodeContact}, pubKey...)
	f = append(f, 0, 0, pathLen) // type, flags, encoded out_path_len
	p := make([]byte, 64)
	copy(p, path)
	f = append(f, p...)
	n := make([]byte, 32)
	copy(n, name)
	f = append(f, n...)
	f = binary.LittleEndian.AppendUint32(f, 1700000000) // last advert seen
	f = binary.LittleEndian.AppendUint32(f, 0)          // lat
	f = binary.LittleEndian.AppendUint32(f, 0)          // lon
	f = binary.LittleEndian.AppendUint32(f, 1700000001) // lastmod
	return f
}

// TestContactV3RefreshKeepsFullContact pins the merge rule: a short V3 contact
// frame refreshes the name of an already-known contact without dropping its
// path, type or GPS — a full frame must not be required to keep them.
func TestContactV3RefreshKeepsFullContact(t *testing.T) {
	c := NewMeshCoreClient(nil)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c.parseContact(contactFrame(key, "old", 0x02, []byte{0xAA, 0xBB}))

	c.parseContactV3(append(append([]byte{respCodeContact}, key[:6]...), "new"...))

	ct := c.contacts[pubKeyHex(key)]
	if ct == nil {
		t.Fatal("contact lost")
	}
	if ct.Name != "new" {
		t.Fatalf("name not refreshed: %q", ct.Name)
	}
	if len(ct.Path) != 2 || ct.Path[0] != 0xAA || ct.PathLength != 0x02 {
		t.Fatalf("full contact data dropped by the V3 refresh: %+v", ct)
	}
}

// TestV1MessageDedup pins the dedup rule for pre-V3 firmware: a sender retry
// (same prefix, timestamp and text) must be processed once.
func TestV1MessageDedup(t *testing.T) {
	c := NewMeshCoreClient(nil)

	dmFrame := append([]byte{respCodeContactMsgRecv, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x02, txtTypePlain}, 0, 0, 0, 0)
	dmFrame = append(dmFrame, "hi"...)
	dmCalls := 0
	c.onMessage = func(*Message) { dmCalls++ }
	c.parseContactMessage(dmFrame)
	c.parseContactMessage(dmFrame) // sender retry: same timestamp+text
	if dmCalls != 1 {
		t.Fatalf("V1 DM not deduplicated: %d callbacks", dmCalls)
	}

	chFrame := append([]byte{respCodeChannelMsgRecv, 0, 0x02, txtTypePlain}, 0, 0, 0, 0)
	chFrame = append(chFrame, "Bot: hi"...)
	chCalls := 0
	c.onChannelMessage = func(*ChannelMessage) { chCalls++ }
	c.parseChannelMessage(chFrame)
	c.parseChannelMessage(chFrame)
	if chCalls != 1 {
		t.Fatalf("V1 channel message not deduplicated: %d callbacks", chCalls)
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

// TestOnNoMoreMessagesSetRaceWithFrameHandler pins the synchronization rule:
// the callback is typically set after Connect, when the frame handler is
// already reading it, so both sides must go through c.mu. Run with -race.
func TestOnNoMoreMessagesSetRaceWithFrameHandler(t *testing.T) {
	c := NewMeshCoreClient(nil)
	frame := []byte{respCodeNoMoreMessages}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			c.OnNoMoreMessages(func() {})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			c.handleFrame(frame)
		}
	}()
	wg.Wait()
}

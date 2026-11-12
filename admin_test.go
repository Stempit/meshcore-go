package meshcore

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ─── Golden frames ────────────────────────────────────────────────────────────
// Байтовые эталоны собраны по прошивке: CMD_ADD_UPDATE_CONTACT разбирает
// updateContactFromFrame, CMD_SET_CHANNEL — handleCmdFrame (MyMesh.cpp).

// TestBuildUpdateContactFrame pins the full 148-byte CMD_ADD_UPDATE_CONTACT
// layout: [09][pubkey32][type][flags][path_len][path64][name32][ts4][lat4]
// [lon4][lastmod4].
func TestBuildUpdateContactFrame(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	lat, lon := 55.75, 37.61
	frame, err := buildUpdateContactFrame(&Contact{
		PublicKey:  key,
		Name:       "node",
		Type:       AdvTypeRepeater,
		Flags:      ContactFlagFavorite,
		PathLength: 0xFF,
		Path:       []byte{0xAA, 0xBB},
		Latitude:   &lat,
		Longitude:  &lon,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(frame) != 148 {
		t.Fatalf("frame len = %d, want 148", len(frame))
	}
	if frame[0] != cmdAddUpdateContact {
		t.Fatalf("code = %#02x", frame[0])
	}
	if !bytes.Equal(frame[1:33], key) {
		t.Fatal("public key mismatch")
	}
	if frame[33] != AdvTypeRepeater || frame[34] != ContactFlagFavorite || frame[35] != 0xFF {
		t.Fatalf("type/flags/path_len: %d %d %#02x", frame[33], frame[34], frame[35])
	}
	path := make([]byte, 64)
	path[0], path[1] = 0xAA, 0xBB
	if !bytes.Equal(frame[36:100], path) {
		t.Fatalf("path field: %x", frame[36:100])
	}
	if !bytes.Equal(frame[100:132], pad("node", 32)) {
		t.Fatalf("name field: %q", frame[100:132])
	}
	ts := time.Unix(int64(binary.LittleEndian.Uint32(frame[132:136])), 0)
	if d := time.Since(ts); d < -10*time.Second || d > 10*time.Second {
		t.Fatalf("last_advert_timestamp not ~now: %v", ts)
	}
	if got := int32(binary.LittleEndian.Uint32(frame[136:140])); got != int32(55.75e6) {
		t.Fatalf("lat = %d", got)
	}
	if got := int32(binary.LittleEndian.Uint32(frame[140:144])); got != int32(37.61e6) {
		t.Fatalf("lon = %d", got)
	}
	if got := binary.LittleEndian.Uint32(frame[144:148]); got != 0 {
		t.Fatalf("lastmod = %d, want 0 (device stamps its RTC)", got)
	}

	// Short public keys are rejected on the client: the firmware stores any
	// 32 bytes blindly, so a truncated key would brick the contact.
	if _, err := buildUpdateContactFrame(&Contact{PublicKey: key[:16]}); err == nil {
		t.Fatal("short public key accepted")
	}
}

// TestBuildSetChannelFrame pins the 50-byte CMD_SET_CHANNEL layout
// ([20][idx][name32][psk16]) and the RemoveChannel variant (empty name, zero
// PSK).
func TestBuildSetChannelFrame(t *testing.T) {
	psk := pad("0123456789abcdef", 16)
	frame, err := buildSetChannelFrame(2, "chat", psk)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := append([]byte{cmdSetChannel, 2}, pad("chat", 32)...)
	want = append(want, psk...)
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = %x, want %x", frame, want)
	}

	// "Deleting" a channel = overwriting the slot with an empty name and a
	// zero PSK; the name must be cleared too or the slot still matches
	// null-key traffic.
	empty, err := buildSetChannelFrame(3, "", make([]byte, 16))
	if err != nil {
		t.Fatalf("build empty: %v", err)
	}
	wantEmpty := append([]byte{cmdSetChannel, 3}, make([]byte, 48)...)
	if !bytes.Equal(empty, wantEmpty) {
		t.Fatalf("empty frame = %x, want %x", empty, wantEmpty)
	}

	// Firmware rejects the 32-byte-secret variant with unsupported_cmd and
	// does not recognize short frames, so the client requires exactly 16.
	for _, bad := range [][]byte{make([]byte, 15), make([]byte, 17), nil, make([]byte, 32)} {
		if _, err := buildSetChannelFrame(0, "x", bad); err == nil {
			t.Fatalf("psk len %d accepted", len(bad))
		}
	}
}

// TestJoinHashtagChannelPSK pins the hashtag key derivation against the
// firmware docs vector: the PSK is the first 16 bytes of SHA256("#name")
// (companion_protocol.md «Hashtag channels»: #test → 9cd8fcf2...).
func TestJoinHashtagChannelPSK(t *testing.T) {
	want, _ := hex.DecodeString("9cd8fcf22a47333b591d96a2b848b73f")
	got := hashtagChannelPSK("test")
	if !bytes.Equal(got, want) {
		t.Fatalf("psk(#test) = %x, want %x", got, want)
	}
	if len(got) != 16 {
		t.Fatalf("len = %d, want 16", len(got))
	}
}

// TestPublicChannelPSKMatchesFirmware cross-checks the exported public-channel
// key against the firmware source string PUBLIC_GROUP_PSK (MyMesh.cpp).
func TestPublicChannelPSKMatchesFirmware(t *testing.T) {
	decoded, err := base64.StdEncoding.DecodeString("izOH6cXN6mrJ5e26oRXNcg==")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(PublicChannelPSK, decoded) {
		t.Fatalf("PublicChannelPSK = %x, want %x", PublicChannelPSK, decoded)
	}
	if len(PublicChannelPSK) != 16 {
		t.Fatalf("len = %d, want 16", len(PublicChannelPSK))
	}
}

// connectAdminTCP connects a client to a fake device for admin tests.
func connectAdminTCP(t *testing.T, addr string) *MeshCoreClient {
	t.Helper()
	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	return client
}

// channelInfoFrame builds a RESP_CODE_CHANNEL_INFO frame body.
func channelInfoFrame(index byte, name string, psk []byte) []byte {
	f := append([]byte{respCodeChannelInfo, index}, pad(name, 32)...)
	p := make([]byte, 16)
	copy(p, psk)
	return append(f, p...)
}

func TestAddContactOK(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdAddUpdateContact, func(_ int, payload []byte) [][]byte {
		if payload[0] != cmdAddUpdateContact || len(payload) != 148 {
			t.Errorf("device got %d-byte frame, code %#02x", len(payload), payload[0])
		}
		return [][]byte{{respCodeOk}}
	})

	client := connectAdminTCP(t, addr)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	ct := &Contact{PublicKey: key, Name: "rep", Type: AdvTypeRepeater}
	if err := client.AddContact(ct); err != nil {
		t.Fatalf("AddContact: %v", err)
	}

	stored := client.contacts[pubKeyHex(key)]
	if stored == nil || stored.Name != "rep" || stored.Type != AdvTypeRepeater {
		t.Fatalf("cache after AddContact: %+v", stored)
	}
}

func TestAddContactTableFull(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdAddUpdateContact, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeTableFull}}
	})

	client := connectAdminTCP(t, addr)
	key := make([]byte, 32)
	ct := &Contact{PublicKey: key, Name: "x", Type: AdvTypeChat}
	err := client.AddContact(ct)
	var devErr *DeviceError
	if !errors.As(err, &devErr) || devErr.Code != errCodeTableFull {
		t.Fatalf("want TABLE_FULL DeviceError, got %v", err)
	}
	if client.contacts[pubKeyHex(key)] != nil {
		t.Fatal("rejected contact leaked into the cache")
	}
}

func TestAddContactValidatesLocally(t *testing.T) {
	dev, addr := startFakeDevice(t) // silent: nothing must reach the device
	client := connectAdminTCP(t, addr)

	if err := client.AddContact(&Contact{PublicKey: []byte{1, 2, 3}, Name: "x"}); err == nil {
		t.Fatal("short public key accepted")
	}
	if err := client.AddContact(nil); err == nil {
		t.Fatal("nil contact accepted")
	}
	// Drop the handshake commands, then verify no ADD_UPDATE was sent.
drainCmds:
	for {
		select {
		case <-dev.cmds:
		default:
			break drainCmds
		}
	}
	select {
	case cmd := <-dev.cmds:
		t.Fatalf("frame reached the device: %x", cmd)
	default:
	}
}

func TestRemoveContactOK(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdRemoveContact, func(_ int, payload []byte) [][]byte {
		if len(payload) != 33 || payload[0] != cmdRemoveContact {
			t.Errorf("device got %d-byte frame, code %#02x", len(payload), payload[0])
		}
		return [][]byte{{respCodeOk}}
	})

	client := connectAdminTCP(t, addr)
	key := make([]byte, 32)
	client.parseContact(contactFrame(key, "gone", 0xFF, nil))
	if client.contacts[pubKeyHex(key)] == nil {
		t.Fatal("contact not in cache before removal")
	}

	if err := client.RemoveContact(&Contact{PublicKey: key}); err != nil {
		t.Fatalf("RemoveContact: %v", err)
	}
	if client.contacts[pubKeyHex(key)] != nil {
		t.Fatal("contact still in cache after removal")
	}
}

func TestRemoveContactNotFound(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdRemoveContact, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeNotFound}}
	})

	client := connectAdminTCP(t, addr)
	err := client.RemoveContact(&Contact{PublicKey: make([]byte, 32)})
	var devErr *DeviceError
	if !errors.As(err, &devErr) || devErr.Code != errCodeNotFound {
		t.Fatalf("want NOT_FOUND DeviceError, got %v", err)
	}
}

func TestGetContactByKey(t *testing.T) {
	dev, addr := startFakeDevice(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	dev.setResponder(cmdGetContactByKey, func(_ int, payload []byte) [][]byte {
		if len(payload) != 33 || payload[0] != cmdGetContactByKey || !bytes.Equal(payload[1:33], key) {
			t.Errorf("device got frame %x", payload)
		}
		return [][]byte{contactFrame(key, "found", 0xFF, nil)}
	})

	client := connectAdminTCP(t, addr)
	ct, err := client.GetContactByKey(key)
	if err != nil {
		t.Fatalf("GetContactByKey: %v", err)
	}
	if ct == nil || ct.Name != "found" {
		t.Fatalf("contact: %+v", ct)
	}
	if client.contacts[pubKeyHex(key)] == nil {
		t.Fatal("reply did not refresh the cache")
	}

	if _, err := client.GetContactByKey(key[:16]); err == nil {
		t.Fatal("short public key accepted")
	}
}

func TestSetChannelWaitsForDevice(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSetChannel, func(_ int, payload []byte) [][]byte {
		if len(payload) != 50 || payload[0] != cmdSetChannel {
			t.Errorf("device got %d-byte frame", len(payload))
		}
		return [][]byte{{respCodeOk}}
	})

	client := connectAdminTCP(t, addr)
	psk := pad("0123456789abcdef", 16)
	if err := client.SetChannel(2, "chat", psk); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	ch := client.channels[2]
	if ch == nil || ch.Name != "chat" || ch.IsEmpty {
		t.Fatalf("cache after SetChannel: %+v", ch)
	}

	// The short-PSK rejection happens on the client: nothing reaches the
	// device and the cache keeps the previous slot state.
	if err := client.SetChannel(3, "bad", psk[:8]); err == nil {
		t.Fatal("short PSK accepted")
	}
	if client.channels[3] != nil {
		t.Fatal("rejected SetChannel wrote the cache")
	}
}

// TestAddChannelPicksFirstEmptySlot scripts slots 0/1 as occupied and slot 2
// as free: AddChannel must query the unknown slots (GET_CHANNEL), then write
// the new channel into slot 2.
func TestAddChannelPicksFirstEmptySlot(t *testing.T) {
	dev, addr := startFakeDevice(t)
	pskA := pad("AAAAAAAAAAAAAAAA", 16)
	pskB := pad("BBBBBBBBBBBBBBBB", 16)
	dev.setResponder(cmdGetChannel, func(_ int, payload []byte) [][]byte {
		idx := payload[1]
		switch idx {
		case 0:
			return [][]byte{channelInfoFrame(0, "Public", pskA)}
		case 1:
			return [][]byte{channelInfoFrame(1, "chat", pskB)}
		default:
			return [][]byte{channelInfoFrame(idx, "", nil)}
		}
	})
	var setPayload []byte
	dev.setResponder(cmdSetChannel, func(_ int, payload []byte) [][]byte {
		setPayload = append([]byte(nil), payload...)
		return [][]byte{{respCodeOk}}
	})

	client := connectAdminTCP(t, addr)
	newPSK := pad("0123456789abcdef", 16)
	ch, err := client.AddChannel("new", newPSK)
	if err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	if ch.Index != 2 || ch.Name != "new" || ch.IsEmpty {
		t.Fatalf("channel: %+v", ch)
	}
	wantSet := append([]byte{cmdSetChannel, 2}, pad("new", 32)...)
	wantSet = append(wantSet, newPSK...)
	if !bytes.Equal(setPayload, wantSet) {
		t.Fatalf("SET_CHANNEL payload: %x", setPayload)
	}
	if ch := client.channels[0]; ch == nil || ch.Name != "Public" {
		t.Fatal("queried slots did not land in the cache")
	}
}

// TestAddChannelUsesCachedSlots pins that a complete cache skips the GET
// round entirely.
func TestAddChannelUsesCachedSlots(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdGetChannel, func(_ int, _ []byte) [][]byte {
		t.Error("GET_CHANNEL sent although the cache was complete")
		return nil
	})
	dev.setResponder(cmdSetChannel, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeOk}}
	})

	client := connectAdminTCP(t, addr)
	client.parseChannelInfo(channelInfoFrame(0, "Public", pad("AAAAAAAAAAAAAAAA", 16)))
	client.parseChannelInfo(channelInfoFrame(1, "chat", pad("BBBBBBBBBBBBBBBB", 16)))
	client.parseChannelInfo(channelInfoFrame(2, "", nil))
	// A 3-slot device (MAX_GROUP_CHANNELS=3): the parsed cache now covers
	// every slot, so the query loop must not fire at all.
	client.maxChannels = 3

	ch, err := client.AddChannel("third", pad("CCCCCCCCCCCCCCCC", 16))
	if err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	if ch.Index != 2 {
		t.Fatalf("slot = %d, want 2", ch.Index)
	}
}

func TestAddChannelNoEmptySlot(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdGetChannel, func(_ int, payload []byte) [][]byte {
		return [][]byte{channelInfoFrame(payload[1], "full", pad("AAAAAAAAAAAAAAAA", 16))}
	})

	client := connectAdminTCP(t, addr)
	if _, err := client.AddChannel("x", pad("0123456789abcdef", 16)); !errors.Is(err, ErrNoEmptyChannelSlot) {
		t.Fatalf("want ErrNoEmptyChannelSlot, got %v", err)
	}
}

// TestAddChannelStopsOnNotFound pins the defensive path: the device reports
// fewer slots than DeviceInfo claimed, the query loop must stop at the first
// NOT_FOUND instead of asking all eight default slots.
func TestAddChannelStopsOnNotFound(t *testing.T) {
	dev, addr := startFakeDevice(t)
	queries := 0
	dev.setResponder(cmdGetChannel, func(_ int, _ []byte) [][]byte {
		queries++
		return [][]byte{{respCodeErr, errCodeNotFound}}
	})

	client := connectAdminTCP(t, addr)
	if _, err := client.AddChannel("x", pad("0123456789abcdef", 16)); !errors.Is(err, ErrNoEmptyChannelSlot) {
		t.Fatalf("want ErrNoEmptyChannelSlot, got %v", err)
	}
	if queries != 1 {
		t.Fatalf("GET_CHANNEL sent %d times, want 1 (loop must break on NOT_FOUND)", queries)
	}
}

// TestAddChannelPropagatesDeviceErrors pins that only NOT_FOUND ends the slot
// query early: any other device error must surface to the caller, not
// masquerade as ErrNoEmptyChannelSlot.
func TestAddChannelPropagatesDeviceErrors(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdGetChannel, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeTableFull}}
	})

	client := connectAdminTCP(t, addr)
	_, err := client.AddChannel("x", pad("0123456789abcdef", 16))
	var devErr *DeviceError
	if !errors.As(err, &devErr) || devErr.Code != errCodeTableFull {
		t.Fatalf("want TABLE_FULL DeviceError, got %v", err)
	}
}

func TestCreatePrivateChannel(t *testing.T) {
	dev, addr := startFakeDevice(t)
	var setPSK []byte
	dev.setResponder(cmdSetChannel, func(_ int, payload []byte) [][]byte {
		setPSK = append([]byte(nil), payload[34:50]...)
		return [][]byte{{respCodeOk}}
	})
	dev.setResponder(cmdGetChannel, func(_ int, payload []byte) [][]byte {
		return [][]byte{channelInfoFrame(payload[1], "", nil)}
	})

	client := connectAdminTCP(t, addr)
	ch, err := client.CreatePrivateChannel("secret")
	if err != nil {
		t.Fatalf("CreatePrivateChannel: %v", err)
	}
	if ch.Index != 0 || len(ch.PSK) != 16 {
		t.Fatalf("channel: %+v", ch)
	}
	if bytes.Equal(setPSK, make([]byte, 16)) {
		t.Fatal("private channel got an all-zero PSK")
	}
}

func TestJoinPublicChannel(t *testing.T) {
	dev, addr := startFakeDevice(t)
	var gotName string
	var gotPSK []byte
	dev.setResponder(cmdSetChannel, func(_ int, payload []byte) [][]byte {
		gotName = string(bytes.TrimRight(payload[2:34], "\x00"))
		gotPSK = append([]byte(nil), payload[34:50]...)
		return [][]byte{{respCodeOk}}
	})
	dev.setResponder(cmdGetChannel, func(_ int, payload []byte) [][]byte {
		return [][]byte{channelInfoFrame(payload[1], "", nil)}
	})

	client := connectAdminTCP(t, addr)
	if _, err := client.JoinPublicChannel(""); err != nil {
		t.Fatalf("JoinPublicChannel: %v", err)
	}
	if gotName != "Public" {
		t.Fatalf("default name = %q, want Public", gotName)
	}
	if !bytes.Equal(gotPSK, PublicChannelPSK) {
		t.Fatalf("psk = %x, want %x", gotPSK, PublicChannelPSK)
	}
}

func TestJoinHashtagChannel(t *testing.T) {
	dev, addr := startFakeDevice(t)
	var gotName string
	var gotPSK []byte
	dev.setResponder(cmdSetChannel, func(_ int, payload []byte) [][]byte {
		gotName = string(bytes.TrimRight(payload[2:34], "\x00"))
		gotPSK = append([]byte(nil), payload[34:50]...)
		return [][]byte{{respCodeOk}}
	})
	dev.setResponder(cmdGetChannel, func(_ int, payload []byte) [][]byte {
		return [][]byte{channelInfoFrame(payload[1], "", nil)}
	})

	client := connectAdminTCP(t, addr)
	ch, err := client.JoinHashtagChannel("#test")
	if err != nil {
		t.Fatalf("JoinHashtagChannel: %v", err)
	}
	if gotName != "#test" {
		t.Fatalf("name = %q, want #test", gotName)
	}
	wantPSK, _ := hex.DecodeString("9cd8fcf22a47333b591d96a2b848b73f")
	if !bytes.Equal(gotPSK, wantPSK) {
		t.Fatalf("psk = %x", gotPSK)
	}
	if ch.Index != 0 {
		t.Fatalf("slot = %d", ch.Index)
	}

	// The leading # is optional; the stored name still becomes "#test".
	if _, err := client.JoinHashtagChannel("test"); err != nil {
		t.Fatalf("JoinHashtagChannel without #: %v", err)
	}
	if gotName != "#test" || !bytes.Equal(gotPSK, wantPSK) {
		t.Fatalf("normalized: name %q psk %x", gotName, gotPSK)
	}

	if _, err := client.JoinHashtagChannel("  "); err == nil {
		t.Fatal("empty tag accepted")
	}
}

func TestRemoveChannel(t *testing.T) {
	dev, addr := startFakeDevice(t)
	var payload []byte
	dev.setResponder(cmdSetChannel, func(_ int, p []byte) [][]byte {
		payload = append([]byte(nil), p...)
		return [][]byte{{respCodeOk}}
	})

	client := connectAdminTCP(t, addr)
	client.parseChannelInfo(channelInfoFrame(1, "chat", pad("BBBBBBBBBBBBBBBB", 16)))

	if err := client.RemoveChannel(1); err != nil {
		t.Fatalf("RemoveChannel: %v", err)
	}
	want := append([]byte{cmdSetChannel, 1}, make([]byte, 48)...)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = %x, want empty-name+zero-psk %x", payload, want)
	}
	ch := client.channels[1]
	if ch == nil || !ch.IsEmpty || ch.Name != "" {
		t.Fatalf("slot after removal: %+v", ch)
	}
}

// TestOnContactDeletedPush pins the 0x8F path: the device-side eviction
// removes the contact from the cache and fires OnContactDeleted with the full
// key.
func TestOnContactDeletedPush(t *testing.T) {
	c := NewMeshCoreClient(nil)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 5)
	}
	c.parseContact(contactFrame(key, "evicted", 0xFF, nil))

	var got []byte
	c.onContactDeleted = func(pubKey []byte) { got = append([]byte(nil), pubKey...) }

	c.handleFrame(append([]byte{pushCodeContactDeleted}, key...))

	if !bytes.Equal(got, key) {
		t.Fatalf("callback got %x, want %x", got, key)
	}
	if c.contacts[pubKeyHex(key)] != nil {
		t.Fatal("evicted contact still in the cache")
	}
}

// TestMaxChannelsFromDeviceInfo pins that MAX_GROUP_CHANNELS from the device
// query reaches findEmptyChannelSlot (frame byte 3).
func TestMaxChannelsFromDeviceInfo(t *testing.T) {
	c := NewMeshCoreClient(nil)
	c.selfInfo = &SelfInfo{}
	c.parseDeviceInfo(deviceInfoFrame())
	if c.maxChannels != 8 {
		t.Fatalf("maxChannels = %d, want 8", c.maxChannels)
	}
}

// ─── ListContacts / ListChannels ──────────────────────────────────────────────

// TestListContacts scripts a full GET_CONTACTS iteration (START, two CONTACT
// frames, END) and pins the listing contract: the result is the device's list,
// sorted by name, deep-copied, and the cache/onContact path runs as usual.
func TestListContacts(t *testing.T) {
	dev, addr := startFakeDevice(t)
	keyA := make([]byte, 32)
	keyB := make([]byte, 32)
	for i := range keyA {
		keyA[i] = byte(i + 1)
		keyB[i] = byte(i + 2)
	}
	dev.setResponder(cmdGetContacts, func(_ int, _ []byte) [][]byte {
		return [][]byte{
			{respCodeContactsStart, 2, 0, 0, 0},
			contactFrame(keyB, "zeta", 0xFF, nil), // not sorted on the wire
			contactFrame(keyA, "alpha", 0xFF, nil),
			{respCodeEndOfContacts, 0x11, 0x22, 0x33, 0x44},
		}
	})

	client := connectAdminTCP(t, addr)
	var cb *Contact
	client.onContact = func(ct *Contact) { cb = ct }

	list, err := client.ListContacts()
	if err != nil {
		t.Fatalf("ListContacts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Fatalf("not sorted by name: %q, %q", list[0].Name, list[1].Name)
	}
	if list[0].PublicKeyHex() != pubKeyHex(keyA) {
		t.Fatalf("key mismatch: %s", list[0].PublicKeyHex())
	}
	if cb == nil || cb.Name != "alpha" {
		t.Fatalf("onContact did not fire for streamed contacts: %+v", cb)
	}

	// Deep copies: mutating the result must not touch the driver's cache.
	list[0].Name = "mutated"
	if client.contacts[pubKeyHex(keyA)].Name != "alpha" {
		t.Fatal("list entry aliases the cached contact")
	}
	list[1].PublicKey[0] ^= 0xFF
	if got := client.contacts[pubKeyHex(keyB)].PublicKey[0]; got != keyB[0] {
		t.Fatalf("list entry aliases the cached public key: cache byte %#02x", got)
	}

	// A second listing returns the same list without duplicating anything.
	list2, err := client.ListContacts()
	if err != nil || len(list2) != 2 {
		t.Fatalf("second ListContacts: %v, len %d", err, len(list2))
	}
}

// TestListContactsDrainsBufferedFrames pins the post-END drain: END arrives as
// the last frame, but the main select gives no ordering, so buffered CONTACT
// frames may still be unread when END is picked. With enough contacts the
// handler always outruns the parsing loop, so END lands while the buffer still
// holds contacts — the listing must return all of them (a removed drain shows
// up as a short list).
func TestListContactsDrainsBufferedFrames(t *testing.T) {
	dev, addr := startFakeDevice(t)
	const total = 40
	keys := make([][]byte, total)
	frames := make([][]byte, 0, total+2)
	frames = append(frames, []byte{respCodeContactsStart, total, 0, 0, 0})
	for i := range keys {
		keys[i] = make([]byte, 32)
		for j := range keys[i] {
			keys[i][j] = byte(i + 1)
		}
		frames = append(frames, contactFrame(keys[i], fmt.Sprintf("n%02d", i), 0xFF, nil))
	}
	frames = append(frames, []byte{respCodeEndOfContacts, 0, 0, 0, 0})
	dev.setResponder(cmdGetContacts, func(_ int, _ []byte) [][]byte { return frames })

	client := connectAdminTCP(t, addr)
	list, err := client.ListContacts()
	if err != nil {
		t.Fatalf("ListContacts: %v", err)
	}
	if len(list) != total {
		t.Fatalf("len = %d, want %d (buffered contacts must be drained after END)", len(list), total)
	}
	for i, ct := range list {
		want := fmt.Sprintf("n%02d", i) // sorted by name
		if ct.Name != want {
			t.Fatalf("list[%d].Name = %q, want %q", i, ct.Name, want)
		}
	}
}

func TestListContactsEmpty(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdGetContacts, func(_ int, _ []byte) [][]byte {
		return [][]byte{
			{respCodeContactsStart, 0, 0, 0, 0},
			{respCodeEndOfContacts, 0, 0, 0, 0},
		}
	})

	client := connectAdminTCP(t, addr)
	list, err := client.ListContacts()
	if err != nil {
		t.Fatalf("ListContacts: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("len = %d, want 0", len(list))
	}
}

func TestListContactsBadState(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdGetContacts, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeBadState}}
	})

	client := connectAdminTCP(t, addr)
	_, err := client.ListContacts()
	var devErr *DeviceError
	if !errors.As(err, &devErr) || devErr.Code != errCodeBadState {
		t.Fatalf("want bad_state DeviceError, got %v", err)
	}
}

// TestListChannels pins the full-slot listing: ordered by index, empty slots
// included, deep copies, and the derived hash matching the raw-packet helper.
func TestListChannels(t *testing.T) {
	dev, addr := startFakeDevice(t)
	pskA := pad("AAAAAAAAAAAAAAAA", 16)
	pskC := pad("CCCCCCCCCCCCCCCC", 16)
	dev.setResponder(cmdGetChannel, func(_ int, payload []byte) [][]byte {
		switch payload[1] {
		case 0:
			return [][]byte{channelInfoFrame(0, "Public", pskA)}
		case 2:
			return [][]byte{channelInfoFrame(2, "chat", pskC)}
		default:
			return [][]byte{channelInfoFrame(payload[1], "", nil)}
		}
	})

	client := connectAdminTCP(t, addr)
	list, err := client.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(list) != 8 {
		t.Fatalf("len = %d, want 8 (all slots, empty included)", len(list))
	}
	if list[0].Name != "Public" || list[1].Name != "" || !list[1].IsEmpty || list[2].Name != "chat" {
		t.Fatalf("slots: %q %q %q", list[0].Name, list[1].Name, list[2].Name)
	}
	if list[0].PSKHex() != pubKeyHex(pskA) {
		t.Fatalf("psk hex: %s", list[0].PSKHex())
	}

	// Deep copies: mutating the result must not touch the cache.
	list[2].PSK[0] ^= 0xFF
	if client.channels[2].PSK[0] == list[2].PSK[0] {
		t.Fatal("list entry aliases the cached channel PSK")
	}
	if list[2].HashByte() == client.channels[2].HashByte() {
		t.Fatal("mutated copy did not rehash")
	}
}

// TestContactAndChannelHelpers pins the derived-info accessors: type names
// (including unknown values), the channel hash matching the raw-packet
// helper, and the golden #test hash byte.
func TestContactAndChannelHelpers(t *testing.T) {
	ct := &Contact{Type: AdvTypeRoom}
	if ct.TypeName() != "room" {
		t.Fatalf("TypeName: %q", ct.TypeName())
	}
	ct.Type = 7
	if ct.TypeName() != "type7" {
		t.Fatalf("unknown TypeName: %q", ct.TypeName())
	}
	if (*Contact)(nil).TypeName() != "" || (*Contact)(nil).PublicKeyHex() != "" {
		t.Fatal("nil receivers must be safe")
	}

	ch := &Channel{PSK: PublicChannelPSK}
	wantHash, _ := channelHashByte(PublicChannelPSK)
	if ch.HashByte() != wantHash {
		t.Fatal("HashByte diverges from the raw-packet helper")
	}
	// Golden pin for the documented #test channel: its PSK is 9cd8fcf2...73f,
	// and the channel hash byte is the first byte of SHA256 over those PSK
	// bytes (0xd9, verified against the firmware docs).
	if ch2 := (&Channel{PSK: hashtagChannelPSK("test")}); ch2.HashByte() != 0xd9 {
		t.Fatalf("#test hash byte: %#02x, want 0xd9", ch2.HashByte())
	}
	var empty *Channel
	if empty.HashByte() != 0 || empty.PSKHex() != "" {
		t.Fatal("nil receiver must be safe")
	}
}

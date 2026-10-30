package meshcore

// Synthetic RF packet construction and raw-packet helpers: everything that
// builds or interprets MeshCore RF packets rather than companion frames —
// GRP_TXT/TXT_MSG packet builders for the outgoing-packet MQTT mirror, the
// firmware's dedup packet hash, LoRa airtime estimation, raw payload/path
// parsing, and the repeat watch that lets a channel send stop early.

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/curve25519"
)

// buildGRPTXTPacket constructs a valid MeshCore GRP_TXT RF packet from plaintext
// fields. This lets us publish outgoing channel messages to MQTT as TX packets so
// the analyzer server can decode and display them like any other received packet.
//
// Encryption: AES-128-ECB with the 16-byte channel PSK.
// MAC:        HMAC-SHA256(psk‖zeros16, ciphertext)[:2].
// Wire:       [header=0x15][path_len=0x00][channel_hash(1)][mac(2)][ciphertext…]
func buildGRPTXTPacket(psk []byte, senderName, text string, timestamp uint32) []byte {
	if len(psk) < 16 {
		return nil
	}
	key := psk[:16]

	// Plaintext: timestamp(4 LE) + flags(1) + "sender: text"
	ts := timestamp
	msg := senderName + ": " + text
	plain := make([]byte, 5+len(msg))
	binary.LittleEndian.PutUint32(plain[0:4], ts)
	plain[4] = 0x00
	copy(plain[5:], msg)

	// Pad to AES block boundary
	if pad := aes.BlockSize - (len(plain) % aes.BlockSize); pad != aes.BlockSize {
		plain = append(plain, make([]byte, pad)...)
	}

	// AES-128-ECB encrypt (block by block, no IV)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	ct := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(ct[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}

	// HMAC-SHA256 MAC: secret = key(16) + zeros(16)
	secret := make([]byte, 32)
	copy(secret, key)
	h := hmac.New(sha256.New, secret)
	h.Write(ct)
	mac := h.Sum(nil)[:2]

	// Channel hash = SHA256(PSK)[0]
	hashSum := sha256.Sum256(key)

	// Wire: [0x15 header][0x00 path_len][channel_hash][mac(2)][ciphertext…]
	// header = ROUTE_TYPE_FLOOD(0x01) | PAYLOAD_TYPE_GRP_TXT(0x05)<<2 = 0x15
	pkt := make([]byte, 0, 3+len(mac)+len(ct))
	pkt = append(pkt, 0x15, 0x00, hashSum[0])
	pkt = append(pkt, mac...)
	pkt = append(pkt, ct...)
	return pkt
}

// MeshCore packet/header constants (mirrors firmware src/MeshCore.h, src/Packet.h).
const (
	payloadTypeGrpTxt = 0x05 // PAYLOAD_TYPE_GRP_TXT
	payloadTypeTxtMsg = 0x02 // PAYLOAD_TYPE_TXT_MSG
	maxHashSize       = 8    // MAX_HASH_SIZE
)

// packetHash reproduces Packet::calculatePacketHash for non-TRACE packets:
// SHA256(payloadType ‖ payload) truncated to MAX_HASH_SIZE. This is the value
// the firmware uses for flood deduplication, so it is identical for our own
// transmission and for any repeater's rebroadcast of the same packet.
func packetHash(payloadType byte, payload []byte) []byte {
	h := sha256.New()
	h.Write([]byte{payloadType})
	h.Write(payload)
	return h.Sum(nil)[:maxHashSize]
}

// loraAirtimeMs estimates a LoRa frame's time-on-air in milliseconds, matching
// RadioLib's getTimeOnAir (Semtech AN1200.13) with MeshCore's radio settings:
// explicit header, CRC on, and preamble length 32 for SF≤8 else 16
// (RadioLibWrapper::preambleLengthForSF). cr is MeshCore's coding-rate value
// (5..8 == 4/5..4/8), which equals the (CR+4) symbol-formula multiplier.
// Returns 0 when parameters are unusable.
func loraAirtimeMs(sf int, bwHz uint32, cr, plBytes int) float64 {
	if sf < 6 || bwHz == 0 || cr < 5 {
		return 0
	}
	tSym := math.Exp2(float64(sf)) / float64(bwHz) // symbol time, seconds
	de := 0
	if tSym > 0.016 { // low-data-rate optimize kicks in above 16ms/symbol
		de = 1
	}
	nPreamble := 16.0
	if sf <= 8 {
		nPreamble = 32.0
	}
	tPreamble := (nPreamble + 4.25) * tSym

	// CRC=1, IH=0 (explicit header). Multiplier (CR+4) == MeshCore cr value.
	num := float64(8*plBytes - 4*sf + 28 + 16)
	den := float64(4 * (sf - 2*de))
	symbols := math.Ceil(num/den) * float64(cr)
	if symbols < 0 {
		symbols = 0
	}
	tPayload := (8 + symbols) * tSym
	return (tPreamble + tPayload) * 1000
}

// repeatWindowFor sizes how long to wait for the mesh to rebroadcast our packet
// before resending. A repeater waits a random flood backoff of up to ~2.6×airtime
// (Mesh::getRetransmitDelay: nextInt(0,5) × airtime×0.52) before its rebroadcast,
// which itself takes ~1×airtime; add our own TX and some slack. Clamped so very
// fast or very slow presets stay sane. Falls back to a default if airtime is
// unknown.
func repeatWindowFor(selfInfo *SelfInfo, pktLen int) time.Duration {
	if selfInfo == nil {
		return defaultRepeatWindow
	}
	air := loraAirtimeMs(selfInfo.Sf, selfInfo.BwHz, selfInfo.Cr, pktLen)
	if air <= 0 {
		return defaultRepeatWindow
	}
	w := time.Duration((4*air + 1500) * float64(time.Millisecond))
	if w < 3*time.Second {
		w = 3 * time.Second
	}
	if w > 12*time.Second {
		w = 12 * time.Second
	}
	return w
}

// rawFramePayload extracts the payload type and payload bytes from a raw RF
// frame (as carried in a 0x88 LOG_RX_DATA frame), mirroring
// Dispatcher::tryParsePacket. The path is skipped so repeats with extra path
// hashes still yield the same payload — and therefore the same packetHash.
func rawFramePayload(raw []byte) (byte, []byte, bool) {
	if len(raw) < 2 {
		return 0, nil, false
	}
	i := 0
	header := raw[i]
	i++
	payloadType := (header >> 2) & 0x0F // PH_TYPE_SHIFT / PH_TYPE_MASK
	routeType := header & 0x03          // PH_ROUTE_MASK
	// TRANSPORT_FLOOD (0x00) and TRANSPORT_DIRECT (0x03) carry 2×uint16 codes.
	if routeType == 0x00 || routeType == 0x03 {
		i += 4
	}
	if i >= len(raw) {
		return 0, nil, false
	}
	pathLen := raw[i]
	i++
	pathHashSize := int(pathLen>>6) + 1 // getPathHashSize
	pathByteLen := int(pathLen&0x3F) * pathHashSize
	i += pathByteLen
	if i > len(raw) {
		return 0, nil, false
	}
	return payloadType, raw[i:], true
}

// channelHashByte returns the 1-byte channel hash (SHA256(PSK)[0]) the firmware
// writes at the start of a GRP_TXT payload. ok is false when the channel has no
// usable PSK, in which case the hash cannot be checked.
func channelHashByte(psk []byte) (hash byte, ok bool) {
	if len(psk) < 16 {
		return 0, false
	}
	sum := sha256.Sum256(psk[:16])
	return sum[0], true
}

// rawIsChannelMsg reports whether a raw RF frame is a GRP_TXT channel message
// and, when wantHash is supplied, whether its channel hash matches. It is used
// to correlate a decoded channel-message frame with the right 0x88 raw frame
// instead of trusting arrival order alone: adverts, ACKs, DMs and other
// channels' traffic all share the pending-raw queue and would otherwise lend
// their path to an unrelated message.
func rawIsChannelMsg(raw []byte, wantHash byte, haveHash bool) bool {
	ptype, payload, ok := rawFramePayload(raw)
	if !ok || ptype != payloadTypeGrpTxt {
		return false
	}
	if !haveHash {
		return true
	}
	// GRP_TXT payload wire format: [channel_hash(1)][mac(2)][ciphertext…].
	return len(payload) > 0 && payload[0] == wantHash
}

// rawIsTxtMsgFrom reports whether a raw RF frame is a TXT_MSG direct message
// whose source hash matches the given sender key prefix — payload[1] is the
// sender's first key byte on both TRANSPORT and flood routes. It is used to
// correlate a decoded DM frame with the right 0x88 raw frame instead of
// trusting arrival order alone: the DM-side counterpart of rawIsChannelMsg.
func rawIsTxtMsgFrom(raw []byte, senderHash byte) bool {
	ptype, payload, ok := rawFramePayload(raw)
	if !ok || ptype != payloadTypeTxtMsg {
		return false
	}
	// TXT_MSG payload wire format: [dest_hash(1)][src_hash(1)][mac(2)][ciphertext…].
	return len(payload) >= 2 && payload[1] == senderHash
}

// armRepeatWatch starts watching incoming raw frames for a rebroadcast of the
// packet identified by hash, returning a channel that receives once a match is
// heard. Call disarmRepeatWatch when done.
func (c *MeshCoreClient) armRepeatWatch(hash []byte) chan struct{} {
	ch := make(chan struct{}, 1)
	c.repeatMu.Lock()
	c.repeatHash = hash
	c.repeatCh = ch
	c.repeatMu.Unlock()
	return ch
}

func (c *MeshCoreClient) disarmRepeatWatch() {
	c.repeatMu.Lock()
	c.repeatHash = nil
	c.repeatCh = nil
	c.repeatMu.Unlock()
}

// noteRepeatCandidate is called for every incoming raw RF frame. If a repeat
// watch is armed and the frame's packet hash matches, it signals the waiter.
func (c *MeshCoreClient) noteRepeatCandidate(raw []byte) {
	c.repeatMu.Lock()
	want := c.repeatHash
	ch := c.repeatCh
	c.repeatMu.Unlock()
	if want == nil || ch == nil {
		return
	}
	ptype, payload, ok := rawFramePayload(raw)
	if !ok || ptype != payloadTypeGrpTxt {
		return
	}
	if bytes.Equal(packetHash(ptype, payload), want) {
		select {
		case ch <- struct{}{}:
		default: // already signalled
		}
	}
}

// publishTXChannelPacket builds a synthetic outgoing GRP_TXT RF packet and queues
// it to MQTT as an emulated RX packet (zero hop, SNR/RSSI = 0) so the operator can
// see their own outgoing channel messages in the feed. Gated by the bridge's
// TxEnabled setting.
func (c *MeshCoreClient) publishTXChannelPacket(channel *Channel, text string, timestamp uint32) {
	c.mu.RLock()
	publisher := c.publisher
	selfInfo := c.selfInfo
	c.mu.RUnlock()

	if publisher == nil || selfInfo == nil {
		return
	}
	if !publisher.TxEnabled() {
		return
	}
	if len(channel.PSK) == 0 {
		return
	}

	senderName := selfInfo.Name
	rawPkt := buildGRPTXTPacket(channel.PSK, senderName, text, timestamp)
	if rawPkt == nil {
		c.logger.Warn("publishTXChannelPacket: failed to build packet")
		return
	}

	channelName := fmt.Sprintf("ch%d", channel.Index)
	if channel.Name != "" {
		channelName = channel.Name
	}
	channelKeyHex := hex.EncodeToString(channel.PSK[:16])

	publisher.QueueChannelPacket(rawPkt, false, 0, 0, channel.Index, channelName, senderName, text, channelKeyHex)
}

// buildTXTMsgPacket constructs a synthetic MeshCore TXT_MSG RF packet for an
// outgoing direct message, mirroring BaseChatMesh::composeMsgPacket →
// Mesh::createDatagram → Utils::encryptThenMAC from the firmware.
//
// Wire format:
//
//	[header=0x0A][path_len=0x00][dest_hash(1)][src_hash(1)][mac(2)][aes_ecb_ciphertext…]
//
// header = ROUTE_TYPE_DIRECT(0x02) | PAYLOAD_TYPE_TXT_MSG(0x02)<<2 = 0x0A
// path_len = 0x00 → zero hops (emulated: observer at source)
//
// Encryption:
//
//	shared_secret = X25519(our_x25519_scalar, ed25519_pub_to_x25519(recipient_pub))
//	plaintext     = [timestamp(4 LE)][flags=0x00][text…]
//	ciphertext    = AES-128-ECB(shared_secret[:16], pad(plaintext))
//	mac           = HMAC-SHA256(shared_secret, ciphertext)[:2]
//
// privKeyBytes supports all three MeshCore key export sizes:
//
//	32 bytes — standard Ed25519 seed (SHA-512 expanded internally)
//	64 bytes — orlp pre-expanded [scalar(32)|nonce(32)]
//	96 bytes — full MeshCore export [prv(64)|pub(32)]
func buildTXTMsgPacket(privKeyBytes, ourPubKey, recipientPubKey []byte, text string, timestamp uint32) []byte {
	if len(ourPubKey) < 32 || len(recipientPubKey) < 32 {
		return nil
	}

	// Derive X25519 scalar from whichever private key format we have.
	var x25519Scalar [32]byte
	switch len(privKeyBytes) {
	case 32: // seed: expand via SHA-512
		h := sha512.New()
		h.Write(privKeyBytes)
		expanded := h.Sum(nil)
		copy(x25519Scalar[:], expanded[:32])
	case 64, 96: // orlp expanded or full MeshCore export: first 32 bytes are the clamped scalar
		copy(x25519Scalar[:], privKeyBytes[:32])
	default:
		return nil
	}

	// Convert recipient Ed25519 public key (compressed Edwards point) to X25519.
	p, err := new(edwards25519.Point).SetBytes(recipientPubKey[:32])
	if err != nil {
		return nil
	}
	recipientX25519 := p.BytesMontgomery()

	sharedSecret, err := curve25519.X25519(x25519Scalar[:], recipientX25519)
	if err != nil {
		return nil
	}

	// Plaintext: [timestamp(4 LE)][flags=0x00][text…]
	plain := make([]byte, 5+len(text))
	binary.LittleEndian.PutUint32(plain[0:4], timestamp)
	plain[4] = 0x00
	copy(plain[5:], text)

	// Pad to AES block boundary.
	if pad := aes.BlockSize - (len(plain) % aes.BlockSize); pad != aes.BlockSize {
		plain = append(plain, make([]byte, pad)...)
	}

	block, err := aes.NewCipher(sharedSecret[:16])
	if err != nil {
		return nil
	}
	ct := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(ct[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}

	// MAC = HMAC-SHA256(sharedSecret, ciphertext)[:2]
	h := hmac.New(sha256.New, sharedSecret)
	h.Write(ct)
	mac := h.Sum(nil)[:2]

	// RF packet: [0x0A][0x00][dest_hash][src_hash][mac(2)][ciphertext…]
	pkt := make([]byte, 0, 2+1+1+2+len(ct))
	pkt = append(pkt, 0x0A, 0x00)
	pkt = append(pkt, recipientPubKey[0])
	pkt = append(pkt, ourPubKey[0])
	pkt = append(pkt, mac...)
	pkt = append(pkt, ct...)
	return pkt
}

// publishTXDirectMessage builds a synthetic TXT_MSG RF packet for an outgoing DM
// and queues it to MQTT as an emulated RX packet (zero hop, SNR/RSSI = 0) so the
// operator can see their own outgoing DMs in the feed. Gated by the bridge's
// TxEnabled setting.
func (c *MeshCoreClient) publishTXDirectMessage(contact *Contact, text string, timestamp uint32) {
	c.mu.RLock()
	publisher := c.publisher
	selfInfo := c.selfInfo
	c.mu.RUnlock()

	if publisher == nil || selfInfo == nil {
		return
	}
	if !publisher.TxEnabled() {
		return
	}
	if len(contact.PublicKey) < 32 {
		return
	}

	privKeyHex := publisher.DevicePrivKey()
	if privKeyHex == "" {
		c.logger.Debug("publishTXDirectMessage: DevicePrivKey not configured, skipping DM TX publish")
		return
	}
	privKeyBytes, err := hex.DecodeString(privKeyHex)
	if err != nil {
		c.logger.Warn("publishTXDirectMessage: invalid DevicePrivKey", "error", err)
		return
	}

	rawPkt := buildTXTMsgPacket(privKeyBytes, selfInfo.PublicKey, contact.PublicKey, text, timestamp)
	if rawPkt == nil {
		c.logger.Warn("publishTXDirectMessage: failed to build packet")
		return
	}

	publisher.QueuePacket(rawPkt, false, 0, 0)
}

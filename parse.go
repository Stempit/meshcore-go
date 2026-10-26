package meshcore

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

// ============================================================================
// Парсеры ответов
// ============================================================================

// parseSelfInfo разбирает RESP_CODE_SELF_INFO (0x05).
//
// Формат (из MyMesh.cpp, handleCmdFrame, CMD_APP_START):
//
//	[code(1)][adv_type(1)][tx_power(1)][max_tx_power(1)][pub_key(32)]
//	[lat(4)][lon(4)][multi_acks(1)][adv_loc_policy(1)][tele_flags(1)]
//	[manual_add(1)][freq_hz(4)][bw_hz(4)][sf(1)][cr(1)][name...]
func (c *MeshCoreClient) parseSelfInfo(data []byte) {
	c.logger.Debug("parseSelfInfo", "bytes", len(data))
	// минимум до имени: 1+1+1+1+32+4+4+1+1+1+1+4+4+1+1 = 58 байт
	if len(data) < 58 {
		c.logger.Warn("parseSelfInfo: data too short", "len", len(data))
		return
	}
	info := &SelfInfo{}
	r := bytes.NewReader(data)
	_, _ = r.Seek(2, 0) // пропускаем code + adv_type

	var tx, maxTx int8
	_ = binary.Read(r, binary.LittleEndian, &tx)
	_ = binary.Read(r, binary.LittleEndian, &maxTx)
	info.TxPower = tx
	info.MaxTxPower = maxTx

	// public key
	info.PublicKey = make([]byte, 32)
	_, _ = r.Read(info.PublicKey)

	// lat, lon
	var latInt, lonInt int32
	_ = binary.Read(r, binary.LittleEndian, &latInt)
	_ = binary.Read(r, binary.LittleEndian, &lonInt)
	info.Latitude = float64(latInt) / 1e6
	info.Longitude = float64(lonInt) / 1e6

	// multi_acks, advert_loc_policy, telemetry
	var multiAcks, advLocPolicy, teleFlags byte
	_ = binary.Read(r, binary.LittleEndian, &multiAcks)
	_ = binary.Read(r, binary.LittleEndian, &advLocPolicy)
	_ = binary.Read(r, binary.LittleEndian, &teleFlags)
	info.MultiAcks = int(multiAcks)
	info.AdvertLocPolicy = int(advLocPolicy)
	info.TeleModeBase = int(teleFlags & 0x03)
	info.TeleModeLoc = int((teleFlags >> 2) & 0x03)
	info.TeleModeEnv = int((teleFlags >> 4) & 0x03)

	// manual_add_contacts: 0 = auto-add enabled (ManualAdd = false),
	// non-zero = manual add required (ManualAdd = true).
	var manualAdd byte
	_ = binary.Read(r, binary.LittleEndian, &manualAdd)
	info.ManualAdd = manualAdd != 0

	// Частота передаётся как freq*1000 (Гц*1000 = кГц)
	var freqHz uint32
	_ = binary.Read(r, binary.LittleEndian, &freqHz)
	info.FreqHz = freqHz

	var bwHz uint32
	_ = binary.Read(r, binary.LittleEndian, &bwHz)
	info.BwHz = bwHz

	var sf, cr byte
	_ = binary.Read(r, binary.LittleEndian, &sf)
	_ = binary.Read(r, binary.LittleEndian, &cr)
	info.Sf = int(sf)
	info.Cr = int(cr)

	// Имя — до конца фрейма, null-terminated (или без нуля)
	nameBytes := make([]byte, 0)
	for {
		b, err := r.ReadByte()
		if err != nil || b == 0 {
			break
		}
		nameBytes = append(nameBytes, b)
	}
	info.Name = string(nameBytes)

	// client_repeat (firmware v9+) — 1 байт после имени, если есть
	if r.Len() >= 1 {
		var clientRepeat byte
		_ = binary.Read(r, binary.LittleEndian, &clientRepeat)
		info.ClientRepeat = clientRepeat != 0
	}

	c.mu.Lock()
	c.selfInfo = info
	c.mu.Unlock()
	c.logger.Debug("SelfInfo updated", "name", info.Name, "public_key", fmt.Sprintf("%x", info.PublicKey))
	if c.onSelfInfo != nil {
		c.onSelfInfo(info)
	}
}

// parseDeviceInfo разбирает RESP_CODE_DEVICE_INFO (0x0D).
//
// Формат (из MyMesh.cpp, CMD_DEVICE_QUERY):
//
//	[code(1)][fw_ver(1)][max_contacts_raw(1)][max_channels(1)][ble_pin(4)]
//	[fw_build(12)][model(40)][version(20)][client_repeat(1, v9+)][path_hash_mode(1, v10+)]
func (c *MeshCoreClient) parseDeviceInfo(data []byte) {
	if len(data) < 2 {
		c.logger.Warn("DeviceInfo too short", "len", len(data))
		return
	}
	c.logger.Debug("DeviceInfo received", "data", fmt.Sprintf("%x", data))
	fwVer := data[1]
	c.logger.Info("Device info", "firmware_version", fwVer)

	if fwVer >= 3 && len(data) >= 80 {
		maxContacts := int(data[2]) * 2
		maxChannels := int(data[3])
		blePin := binary.LittleEndian.Uint32(data[4:8])
		fwBuild := string(bytes.TrimRight(data[8:20], "\x00"))
		model := string(bytes.TrimRight(data[20:60], "\x00"))
		version := string(bytes.TrimRight(data[60:80], "\x00"))

		c.logger.Info("Device details",
			"max_contacts", maxContacts,
			"max_channels", maxChannels,
			"ble_pin", blePin,
			"firmware_build", fwBuild,
			"model", model,
			"version", version)

		if len(data) > 81 {
			c.logger.Info("Additional device info",
				"client_repeat_enabled", data[80] != 0,
				"path_hash_mode", int(data[81]))
		}

		c.mu.Lock()
		if c.selfInfo != nil {
			c.selfInfo.FirmwareVersion = version
			c.selfInfo.Model = model
		}
		c.mu.Unlock()
	}
}

// parseContact разбирает полный RESP_CODE_CONTACT (0x03).
//
// Формат (из MyMesh.cpp, writeContactRespFrame):
//
//	[code(1)][pub_key(32)][type(1)][flags(1)][path_len(1)][path(64)]
//	[name(32)][last_advert_timestamp(4)][gps_lat(4)][gps_lon(4)][lastmod(4)]
//
// Итого: 1+32+1+1+1+64+32+4+4+4+4 = 148 байт (contactFrameSize).
func (c *MeshCoreClient) parseContact(data []byte) {
	const minLen = 1 + 32 + 1 + 1 + 1 + 64 + 32 + 4 + 4 + 4
	if len(data) < minLen {
		c.parseContactV3(data)
		return
	}
	pubKey := data[1:33]
	contactType := int(data[33])
	flags := int(data[34])
	// data[35] — кодированный байт out_path_len прошивки: младшие 6 бит — число
	// прыжков, старшие 2 бита — (байт-на-прыжок − 1). Реальная длина пути в байтах
	// = hash_count × hash_size. Для 1-байтных хопов это совпадает с числом прыжков
	// (старое поведение); для многобайтных — нет. 0xFF (OUT_PATH_UNKNOWN) = flood.
	pathLenByte := data[35]
	byteLen := 0
	if pathLenByte != pathLengthFlood {
		byteLen = pathByteLen(pathLenByte)
	}
	if byteLen > 64 {
		byteLen = 64
	}
	// путь всегда занимает ровно 64 байта в фрейме; копируем только значимые
	pathField := data[36:100]
	actualPath := make([]byte, byteLen)
	copy(actualPath, pathField[:byteLen])

	nameBytes := data[100:132]
	name := string(bytes.TrimRight(nameBytes, "\x00"))

	lastSeen := time.Unix(int64(binary.LittleEndian.Uint32(data[132:136])), 0)
	latInt := int32(binary.LittleEndian.Uint32(data[136:140]))
	lonInt := int32(binary.LittleEndian.Uint32(data[140:144]))
	var lat, lon *float64
	if latInt != 0 || lonInt != 0 {
		latVal := float64(latInt) / 1e6
		lonVal := float64(lonInt) / 1e6
		lat = &latVal
		lon = &lonVal
	}
	lastMod := binary.LittleEndian.Uint32(data[144:148])

	contact := &Contact{
		PublicKey:  pubKey,
		Name:       name,
		Type:       contactType,
		Flags:      flags,
		PathLength: int(pathLenByte), // кодированный байт out_path_len (0xFF = flood)
		Path:       actualPath,
		Latitude:   lat,
		Longitude:  lon,
		LastSeen:   lastSeen,
		LastMod:    lastMod,
	}

	c.updateMessagesWithFullKey(contact.PublicKey)

	c.mu.Lock()
	c.contacts[pubKeyHex(pubKey)] = contact
	if lastMod > c.syncLastmod {
		c.syncLastmod = lastMod
	}
	c.mu.Unlock()
	c.logger.Debug("Contact parsed", "name", name, "public_key", fmt.Sprintf("%x", pubKey))
	if c.onContact != nil {
		c.onContact(contact)
	}

}

func (c *MeshCoreClient) parseContactV3(data []byte) {
	// Минимальная длина: code(1) + 6-байтный префикс + хотя бы 1 байт имени + нуль
	if len(data) < 1+6+2 {
		c.logger.Warn("ContactV3 too short", "len", len(data))
		return
	}
	prefix := data[1:7]
	nameStart := 7
	nameBytes := data[nameStart:]
	nullIdx := bytes.IndexByte(nameBytes, 0)
	var name string
	if nullIdx >= 0 {
		name = string(nameBytes[:nullIdx])
	} else {
		name = string(nameBytes)
	}

	// Пытаемся найти полный ключ по префиксу
	var fullKey []byte
	c.mu.RLock()
	for _, contact := range c.contacts {
		if len(contact.PublicKey) >= 6 && bytes.Equal(contact.PublicKey[:6], prefix) {
			fullKey = contact.PublicKey
			break
		}
	}
	c.mu.RUnlock()
	if fullKey == nil {
		fullKey = prefix
		c.logger.Warn("ContactV3: unknown prefix, using as key", "prefix", fmt.Sprintf("%x", prefix))
	}

	contact := &Contact{
		PublicKey:  fullKey,
		Name:       name,
		Type:       0,
		Flags:      0,
		PathLength: 0,
		Path:       []byte{},
		Latitude:   nil,
		Longitude:  nil,
		LastSeen:   time.Now(),
	}

	c.mu.Lock()
	c.contacts[pubKeyHex(fullKey)] = contact
	c.mu.Unlock()
	c.logger.Debug("ContactV3 parsed", "name", name, "public_key", fmt.Sprintf("%x", fullKey))
	if c.onContact != nil {
		c.onContact(contact)
	}
}

// parseChannelInfo разбирает RESP_CODE_CHANNEL_INFO (0x12).
//
// Формат (из MyMesh.cpp, CMD_GET_CHANNEL):
//
//	[code(1)][channel_idx(1)][name(32, null-padded)][psk(16)]
//
// Имя занимает ровно 32 байта (фиксировано), PSK — ровно 16 байт после имени.
func (c *MeshCoreClient) parseChannelInfo(data []byte) {
	// минимум: code(1)+idx(1)+name(32)+psk(16) = 50 байт
	const minLen = 1 + 1 + 32 + 16
	if len(data) < minLen {
		c.logger.Warn("ChannelInfo too short", "len", len(data))
		return
	}
	index := int(data[1])
	// Имя занимает фиксированные 32 байта в позиции 2..33.
	// Firmware пишет имя через strcpy(), поэтому после нуль-терминатора
	// могут идти произвольные байты из буфера — берём только до первого нуля.
	nameRaw := data[2:34]
	nameEnd := bytes.IndexByte(nameRaw, 0)
	var name string
	if nameEnd >= 0 {
		name = string(nameRaw[:nameEnd])
	} else {
		name = string(nameRaw)
	}
	// PSK — 16 байт после фиксированного имени, в позиции 34..49
	psk := make([]byte, 16)
	copy(psk, data[34:50])

	ch := &Channel{
		Index:   index,
		Name:    name,
		PSK:     psk,
		IsEmpty: len(name) == 0,
	}
	c.mu.Lock()
	c.channels[index] = ch
	c.mu.Unlock()
	c.logger.Debug("Channel info parsed", "index", index, "name", name)
	if c.onChannel != nil {
		c.onChannel(ch)
	}
}

func (c *MeshCoreClient) parseSent(data []byte) {
	c.logger.Debug("Sent confirmation received")
	c.notifySent()
}

func (c *MeshCoreClient) parseSendConfirmed(data []byte) {
	c.logger.Debug("Send confirmed")
	c.notifyDelivered()
}

func (c *MeshCoreClient) parsePathUpdated(data []byte) {
	c.logger.Debug("Path updated", "data", fmt.Sprintf("%x", data))
}

func (c *MeshCoreClient) parseAdvert(data []byte) {
	c.logger.Info("Advert received", "data", fmt.Sprintf("%x", data))

	// Frame format: [code(1)][pub_key(32)]  — total 33 bytes.
	// The firmware pushes PUSH_CODE_ADVERT (0x80) with the full 32-byte
	// public key of the node whose advert was just received over-the-air.
	// SNR/RSSI are not included in this push frame.
	if len(data) < 1+pubKeySize {
		c.logger.Warn("parseAdvert: frame too short", "len", len(data), "need", 1+pubKeySize)
		return
	}

	pubKey := make([]byte, pubKeySize)
	copy(pubKey, data[1:1+pubKeySize])

	// Look up the full contact from the contacts cache so that we can
	// include name, type, GPS and path-length in the MQTT advert message.
	c.mu.RLock()
	contact := c.contacts[pubKeyHex(pubKey)]
	c.mu.RUnlock()

	if contact == nil {
		// Contact not yet in cache (advert arrived before CMD_GET_CONTACTS
		// response or auto-add is disabled).  Build a minimal stub so MQTT
		// at least gets the public key.
		c.logger.Warn("parseAdvert: contact not in cache, using stub",
			"pub_key", fmt.Sprintf("%x", pubKey))
		contact = &Contact{
			PublicKey: pubKey,
			LastSeen:  time.Now(),
		}
	} else {
		// The device has updated this contact's data (GPS, name, path) on its
		// side. Fetch just this one contact by key — avoids triggering the
		// full GET_CONTACTS iteration protocol on every advert.
		if err := c.sendGetContactByKey(pubKey); err != nil {
			c.logger.Warn("parseAdvert: failed to refresh contact", "error", err)
		}
	}

	c.publishPendingRawToMQTT(0, 0)

	if c.onAdvert != nil {
		c.onAdvert(contact)
	}
}

// parseNewAdvert handles PUSH_CODE_NEW_ADVERT (0x8A). The frame format is
// identical to RESP_CODE_CONTACT, so we reuse parseContact to update the
// cache. We also remember the contact in pendingNewContact: if the device
// follows up with PUSH_CODE_CONTACTS_FULL (0x90), the contact was not stored
// on the device and we need to add it manually after evicting the oldest.
func (c *MeshCoreClient) parseNewAdvert(data []byte) {
	c.parseContact(data)

	// Retrieve the contact that parseContact just stored.
	if len(data) < 33 {
		return
	}
	pubKey := data[1:33]
	c.mu.Lock()
	c.pendingNewContact = c.contacts[pubKeyHex(pubKey)]
	c.mu.Unlock()
	c.logger.Info("New contact discovered", "pub_key", fmt.Sprintf("%x", pubKey))
}

// parseContactDeleted handles PUSH_CODE_CONTACT_DELETED (0x8F).
// Frame: [code(1)][pub_key(32)]. Removes the contact from the local cache.
func (c *MeshCoreClient) parseContactDeleted(data []byte) {
	if len(data) < 1+pubKeySize {
		c.logger.Warn("parseContactDeleted: frame too short", "len", len(data))
		return
	}
	pubKey := data[1 : 1+pubKeySize]
	keyHex := pubKeyHex(pubKey)
	c.mu.Lock()
	delete(c.contacts, keyHex)
	c.mu.Unlock()
	c.logger.Info("Contact deleted by device", "pub_key", fmt.Sprintf("%x", pubKey))
}

// handleContactsFull handles PUSH_CODE_CONTACTS_FULL (0x90). The device has
// hit its 350-contact limit and could not store the most recent new contact.
// We evict the oldest contact (by last activity) and then write the pending
// new contact to the device so it gets a proper slot.
func (c *MeshCoreClient) handleContactsFull() {
	c.mu.Lock()
	pending := c.pendingNewContact
	c.pendingNewContact = nil

	// Find the oldest contact by activity, skipping the pending new one.
	var oldest *Contact
	for _, ct := range c.contacts {
		if pending != nil && pubKeyHex(ct.PublicKey) == pubKeyHex(pending.PublicKey) {
			continue
		}
		if oldest == nil {
			oldest = ct
			continue
		}
		oldestActivity := oldest.LastSeen
		if !oldest.LastMessageAt.IsZero() && oldest.LastMessageAt.After(oldestActivity) {
			oldestActivity = oldest.LastMessageAt
		}
		ctActivity := ct.LastSeen
		if !ct.LastMessageAt.IsZero() && ct.LastMessageAt.After(ctActivity) {
			ctActivity = ct.LastMessageAt
		}
		if ctActivity.Before(oldestActivity) {
			oldest = ct
		}
	}
	if oldest != nil {
		delete(c.contacts, pubKeyHex(oldest.PublicKey))
	}
	c.mu.Unlock()

	if oldest == nil {
		c.logger.Warn("handleContactsFull: no contact to evict")
		return
	}

	c.logger.Info("Contacts full: evicting oldest contact",
		"name", oldest.Name, "pub_key", fmt.Sprintf("%x", oldest.PublicKey),
		"last_seen", oldest.LastSeen)

	if err := c.sendRemoveContact(oldest.PublicKey); err != nil {
		c.logger.Error("handleContactsFull: failed to remove contact", "error", err)
		return
	}

	if pending == nil {
		return
	}
	if err := c.sendUpdateContact(pending); err != nil {
		c.logger.Error("handleContactsFull: failed to add new contact", "error", err)
	}
}

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

// parseTraceData handles PUSH_CODE_TRACE_DATA (0x89).
// Payload: [code][reserved][path_len][flags][tag(4)][auth(4)][hashes(path_len)][snrs(path_len>>path_sz)][final_snr]
func (c *MeshCoreClient) parseTraceData(data []byte) {
	const minLen = 12 // code+reserved+path_len+flags+tag(4)+auth(4)
	if len(data) < minLen {
		c.logger.Warn("parseTraceData: too short", "len", len(data))
		return
	}
	pathLen := int(data[2])
	flags := data[3]
	pathSz := int(flags & 0x03)
	tag := binary.LittleEndian.Uint32(data[4:8])
	auth := binary.LittleEndian.Uint32(data[8:12])

	snrCount := pathLen >> pathSz
	needed := 12 + pathLen + snrCount + 1
	if len(data) < needed {
		c.logger.Warn("parseTraceData: truncated", "len", len(data), "needed", needed)
		return
	}

	hashes := make([]byte, pathLen)
	copy(hashes, data[12:12+pathLen])

	snrs := make([]float32, snrCount)
	for i := range snrs {
		snrs[i] = float32(int8(data[12+pathLen+i])) / 4.0
	}
	finalSNR := float32(int8(data[12+pathLen+snrCount])) / 4.0

	c.logger.Debug("Trace data received",
		"tag", fmt.Sprintf("%08x", tag),
		"hops", snrCount, // path_len is the hash byte length; one SNR per hop = hop count
		"final_snr", finalSNR)

	c.publishPendingRawToMQTT(float64(finalSNR), 0)

	if c.onTraceData != nil {
		c.onTraceData(&TraceData{
			Tag:        tag,
			AuthCode:   auth,
			PathHashes: hashes,
			PathSNRs:   snrs,
			FinalSNR:   finalSNR,
		})
	}
}

func (c *MeshCoreClient) OnTraceData(f func(*TraceData)) { c.onTraceData = f }

// parseChannelData handles RESP_CODE_CHANNEL_DATA_RECV (0x1B).
// Payload: [code][snr*4][reserved][reserved][chan_idx][path_len][dtype_lo][dtype_hi][data_len][data...]
func (c *MeshCoreClient) parseChannelData(data []byte) {
	const minLen = 9
	if len(data) < minLen {
		c.logger.Warn("parseChannelData: too short", "len", len(data))
		return
	}
	snr := float32(int8(data[1])) / 4.0
	chanIdx := int(data[4])
	pathLen := int(data[5])
	dataType := uint16(data[6]) | uint16(data[7])<<8
	dataLen := int(data[8])
	if len(data) < 9+dataLen {
		c.logger.Warn("parseChannelData: truncated", "len", len(data), "data_len", dataLen)
		return
	}
	payload := make([]byte, dataLen)
	copy(payload, data[9:9+dataLen])

	c.logger.Debug("Channel data received",
		"channel", chanIdx,
		"data_type", fmt.Sprintf("0x%04x", dataType),
		"data_len", dataLen)

	c.publishPendingRawToMQTT(float64(snr), 0)

	if c.onChannelData != nil {
		c.onChannelData(&ChannelDataMessage{
			ChannelIndex: chanIdx,
			DataType:     dataType,
			Data:         payload,
			SNR:          snr,
			PathLength:   pathLen,
		})
	}
}

func (c *MeshCoreClient) OnChannelData(f func(*ChannelDataMessage)) { c.onChannelData = f }

// parseControlData handles PUSH_CODE_CONTROL_DATA (0x8E, firmware v8+).
// Payload: [code][snr*4][rssi][path_len][payload...]
func (c *MeshCoreClient) parseControlData(data []byte) {
	if len(data) < 4 {
		c.logger.Warn("parseControlData: too short", "len", len(data))
		return
	}
	snr := float32(int8(data[1])) / 4.0
	rssi := int8(data[2])
	pathLen := int(data[3])
	payload := make([]byte, len(data)-4)
	copy(payload, data[4:])

	c.logger.Info("Control data received",
		"snr", snr, "rssi", rssi, "path_len", pathLen, "payload_len", len(payload))

	c.publishPendingRawToMQTT(float64(snr), float64(rssi))

	if c.onControlData != nil {
		c.onControlData(&ControlData{SNR: snr, RSSI: rssi, PathLen: pathLen, Payload: payload})
	}
}

func (c *MeshCoreClient) OnControlData(f func(*ControlData)) { c.onControlData = f }

func (c *MeshCoreClient) parseOk(data []byte) {
	c.logger.Debug("Received OK")
	c.notifySent() // для канальных сообщений OK означает принятие команды
}

func (c *MeshCoreClient) parseLogRxData(data []byte) {
	// Frame: [0x88][SNR*4 as int8][RSSI as int8][raw RF bytes...]
	if len(data) < 4 {
		c.logger.Debug("Log RX data too short", "len", len(data))
		return
	}
	snr := float64(int8(data[1])) / 4.0
	rssi := float64(int8(data[2]))
	rawData := make([]byte, len(data)-3)
	copy(rawData, data[3:])

	c.logger.Debug("Log RX data", "snr", snr, "rssi", rssi, "raw_len", len(rawData))

	// If a channel send is waiting to hear the mesh rebroadcast its packet,
	// check this raw frame against it. The firmware emits 0x88 before dedup, so
	// repeats of our own message reach us here even though no decoded 0x11 does.
	c.noteRepeatCandidate(rawData)

	// Flush any stale pending raw packets that never got a matching 0x11 frame.
	c.flushStalePendingRaw()

	// Store in pending; parseChannelMessageV3 will correlate and publish.
	c.pendingRaw = append(c.pendingRaw, pendingRawPacket{
		rawData:   rawData,
		snr:       snr,
		rssi:      rssi,
		arrivedAt: time.Now(),
	})
	if len(c.pendingRaw) > 8 {
		c.pendingRaw = c.pendingRaw[len(c.pendingRaw)-8:]
	}
}

// publishPendingRawToMQTT correlates the oldest non-expired 0x88 raw frame with the
// current RF event and queues it to MQTT. When no pending frame is available it
// falls back to fallbackSNR/fallbackRSSI so the event is still recorded.
func (c *MeshCoreClient) publishPendingRawToMQTT(fallbackSNR, fallbackRSSI float64) {
	c.mu.RLock()
	publisher := c.publisher
	c.mu.RUnlock()
	if publisher == nil {
		return
	}
	rawData, snr, rssi := []byte(nil), fallbackSNR, fallbackRSSI
	for i, p := range c.pendingRaw {
		if time.Since(p.arrivedAt) <= pendingRawTTL {
			rawData, snr, rssi = p.rawData, p.snr, p.rssi
			c.pendingRaw = append(c.pendingRaw[:i], c.pendingRaw[i+1:]...)
			break
		}
	}
	publisher.QueuePacket(rawData, false, snr, rssi)
}

// flushStalePendingRaw publishes any raw packets that have waited longer than
// pendingRawTTL without a matching decoded channel message frame.
func (c *MeshCoreClient) flushStalePendingRaw() {
	if len(c.pendingRaw) == 0 {
		return
	}
	c.mu.RLock()
	publisher := c.publisher
	c.mu.RUnlock()

	now := time.Now()
	remaining := c.pendingRaw[:0]
	for _, p := range c.pendingRaw {
		if now.Sub(p.arrivedAt) > pendingRawTTL {
			if publisher != nil {
				publisher.QueuePacket(p.rawData, false, p.snr, p.rssi)
			}
		} else {
			remaining = append(remaining, p)
		}
	}
	c.pendingRaw = remaining
}

// parseBatteryAndStorage разбирает RESP_CODE_BATT_AND_STORAGE (0x0C).
//
// Формат (из MyMesh.cpp, CMD_GET_BATT_AND_STORAGE):
//
//	[code(1)][battery_mv(2)][used_kb(4)][total_kb(4)]
func (c *MeshCoreClient) parseBatteryAndStorage(data []byte) {
	if len(data) < 11 {
		c.logger.Warn("BatteryAndStorage too short", "len", len(data))
		return
	}
	mv := binary.LittleEndian.Uint16(data[1:3])
	usedKb := binary.LittleEndian.Uint32(data[3:7])
	totalKb := binary.LittleEndian.Uint32(data[7:11])
	c.logger.Info("Battery and storage", "millivolts", mv, "used_kb", usedKb, "total_kb", totalKb)
	if c.onBattery != nil {
		c.onBattery(int(mv), int(usedKb), int(totalKb))
	}
}

// parseStats разбирает RESP_CODE_STATS (0x18) для всех типов статистики.
//
// Форматы (из MeshCore/docs/stats_binary_frames.md):
//
//	STATS_TYPE_CORE (0):    [code(1)][type(1)][battery_mv(2)][uptime_secs(4)][errors(2)][queue_len(1)] = 11 bytes
//	STATS_TYPE_RADIO (1):   [code(1)][type(1)][noise_floor(2)][last_rssi(1)][last_snr(1)][tx_air_secs(4)][rx_air_secs(4)] = 14 bytes
//	STATS_TYPE_PACKETS (2): [code(1)][type(1)][recv(4)][sent(4)][flood_tx(4)][direct_tx(4)][flood_rx(4)][direct_rx(4)] = 26-30 bytes
func (c *MeshCoreClient) parseStats(data []byte) {
	if len(data) < 2 {
		c.logger.Warn("Stats too short", "len", len(data))
		return
	}

	// Get or create stats object (accumulate all stats types)
	c.mu.Lock()
	if c.stats == nil {
		c.stats = &CompanionRadioStats{}
	}
	stats := c.stats
	stats.LastUpdate = time.Now()
	c.mu.Unlock()

	statsType := data[1]
	switch statsType {
	case STATS_TYPE_CORE: // 0 - Core stats
		// [battery_mv(2)][uptime_secs(4)][errors(2)][queue_len(1)] = 9 bytes after header
		if len(data) < 11 {
			c.logger.Warn("Stats CORE too short", "len", len(data))
			return
		}
		stats.BatteryMv = binary.LittleEndian.Uint16(data[2:4])
		stats.UptimeSecs = binary.LittleEndian.Uint32(data[4:8])
		stats.Errors = binary.LittleEndian.Uint16(data[8:10])
		stats.QueueLen = data[10]
		c.logger.Debug("Stats CORE parsed",
			"battery_mv", stats.BatteryMv,
			"uptime_secs", stats.UptimeSecs,
			"errors", stats.Errors,
			"queue_len", stats.QueueLen)

	case STATS_TYPE_RADIO: // 1 - Radio stats
		// [noise_floor(2)][last_rssi(1)][last_snr(1)][tx_air_secs(4)][rx_air_secs(4)] = 12 bytes after header
		if len(data) < 14 {
			c.logger.Warn("Stats RADIO too short", "len", len(data))
			return
		}
		noiseFloor := int16(binary.LittleEndian.Uint16(data[2:4]))
		stats.NoiseFloorDbm = int8(noiseFloor)
		stats.LastRssi = int8(data[4])
		stats.LastSnrDb = float32(int8(data[5])) / 4.0
		stats.TxAirSecs = binary.LittleEndian.Uint32(data[6:10])
		stats.RxAirSecs = binary.LittleEndian.Uint32(data[10:14])
		c.logger.Debug("Stats RADIO parsed",
			"noise_floor", stats.NoiseFloorDbm,
			"last_rssi", stats.LastRssi,
			"last_snr", stats.LastSnrDb,
			"tx_air_secs", stats.TxAirSecs,
			"rx_air_secs", stats.RxAirSecs)

	case STATS_TYPE_PACKETS: // 2 - Packet stats
		// [recv(4)][sent(4)][flood_tx(4)][direct_tx(4)][flood_rx(4)][direct_rx(4)][recv_errors(4)] = 26-30 bytes after header
		if len(data) < 28 {
			c.logger.Warn("Stats PACKETS too short", "len", len(data))
			return
		}
		stats.PacketsRecv = binary.LittleEndian.Uint32(data[2:6])
		stats.PacketsSent = binary.LittleEndian.Uint32(data[6:10])
		stats.PacketsFloodTx = binary.LittleEndian.Uint32(data[10:14])
		stats.PacketsDirectTx = binary.LittleEndian.Uint32(data[14:18])
		stats.PacketsFloodRx = binary.LittleEndian.Uint32(data[18:22])
		stats.PacketsDirectRx = binary.LittleEndian.Uint32(data[22:26])
		// recv_errors present only in 30-byte frame
		if len(data) >= 30 {
			stats.PacketsRecvErrors = binary.LittleEndian.Uint32(data[26:30])
		}
		c.logger.Debug("Stats PACKETS parsed",
			"recv", stats.PacketsRecv,
			"sent", stats.PacketsSent,
			"flood_tx", stats.PacketsFloodTx,
			"direct_tx", stats.PacketsDirectTx,
			"flood_rx", stats.PacketsFloodRx,
			"direct_rx", stats.PacketsDirectRx,
			"recv_errors", stats.PacketsRecvErrors)

	default:
		c.logger.Warn("Stats: unknown stats_type", "type", statsType)
		return
	}

	c.logger.Debug("Stats received", "stats", fmt.Sprintf("%+v", stats))
	if c.onStats != nil {
		c.onStats(stats)
	}
}

func (c *MeshCoreClient) parseAutoAddConfig(data []byte) {
	c.logger.Debug("AutoAddConfig received")
}

func (c *MeshCoreClient) parseCustomVars(data []byte) {
	c.logger.Debug("CustomVars received")
}

// updateMessagesWithFullKey заменяет 6-байтные префиксные ключи в сообщениях
// на полный 32-байтный ключ, как только он становится известен из контакта.
func (c *MeshCoreClient) updateMessagesWithFullKey(fullKey []byte) {
	if len(fullKey) < 6 {
		return
	}
	prefix := fullKey[:6]
	prefixStr := pubKeyHex(prefix)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Ищем сообщения, у которых senderKey == prefix
	if messages, ok := c.messages[prefixStr]; ok {
		// Перекладываем в новый ключ
		fullKeyStr := pubKeyHex(fullKey)
		c.messages[fullKeyStr] = append(c.messages[fullKeyStr], messages...)
		delete(c.messages, prefixStr)
		// Обновляем SenderKey в каждом сообщении
		for _, msg := range c.messages[fullKeyStr] {
			msg.SenderKey = fullKey
		}
	}
}

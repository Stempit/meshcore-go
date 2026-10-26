package meshcore

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// Обработчик входящих фреймов
// ============================================================================

func (c *MeshCoreClient) frameHandler() {
	defer c.wg.Done() // для канала входящих сообщений

	for {
		select {
		case data := <-c.notifyCh:
			c.logger.Debug("RX frame", "data", fmt.Sprintf("%x", data))
			c.handleFrame(data)
		case <-c.doneCh:
			return
		}
	}
}

// parseContactMessageV3 разбирает сообщение V3 от контакта (код 0x10).
//
// Формат (из MyMesh.cpp, queueMessage, app_target_ver >= 3):
//
//	[code(1)][snr(1)][res(1)][res(1)][prefix(6)][path_len(1)][txt_type(1)][timestamp(4)][extra(4, только для signed)][text]
//
// path_len — только метаданные из пакета; реальных байт пути в companion-фрейме нет.
// extra (4 байта sender_prefix) присутствует только когда txt_type == txtTypeSigned.
func (c *MeshCoreClient) parseContactMessageV3(data []byte) {
	// минимум: code(1)+snr+res+res(3)+prefix(6)+path_len(1)+txt_type(1)+timestamp(4) = 16
	const minLen = 1 + 3 + 6 + 1 + 1 + 4
	if len(data) < minLen {
		c.logger.Warn("parseContactMessageV3: data too short", "len", len(data))
		return
	}
	prefix := data[4:10]
	// data[10] = path_len (метаданные, байты пути в companion-фрейме не передаются).
	// Поле — кодированный байт path_len (или 0xFF для прямой доставки); берём из
	// него число прыжков, а не «сырое» значение (важно для многобайтных хопов).
	pathLen := decodeMsgHops(data[10])
	txtType := data[11]
	timestampRaw := binary.LittleEndian.Uint32(data[12:16])

	// Для signed-сообщений после timestamp идут 4 байта extra (sender_prefix)
	textStart := 16
	if txtType == txtTypeSigned {
		textStart = 20
	}
	if textStart >= len(data) {
		c.logger.Warn("parseContactMessageV3: text start beyond data", "textStart", textStart, "len", len(data))
		return
	}
	textBytes := data[textStart:]
	nullIdx := bytes.IndexByte(textBytes, 0)
	var text string
	if nullIdx >= 0 {
		text = string(textBytes[:nullIdx])
	} else {
		text = string(textBytes)
	}

	// Поиск контакта по префиксу
	var fullKey []byte
	var senderName string
	var contactPath []byte
	c.mu.RLock()
	for _, contact := range c.contacts {
		if len(contact.PublicKey) >= 6 && bytes.Equal(contact.PublicKey[:6], prefix) {
			fullKey = contact.PublicKey
			senderName = contact.Name
			contactPath = contact.Path
			break
		}
	}
	c.mu.RUnlock()

	if fullKey == nil {
		fullKey = prefix
		c.logger.Warn("parseContactMessageV3: unknown sender", "prefix", fmt.Sprintf("%x", prefix))
	}

	dedupKey := fmt.Sprintf("dm:%x-%d-%s", prefix, timestampRaw, text)
	if c.isDuplicateMessage(dedupKey) {
		c.logger.Debug("Duplicate DM dropped (sender retry)",
			"prefix", fmt.Sprintf("%x", prefix), "timestamp", timestampRaw, "text", text)
		return
	}

	// The companion frame carries only the hop count; the actual path bytes are
	// in the correlated raw RF packet (present only for live, not history-synced,
	// messages). This reflects the real route the message took — e.g. a sender's
	// forced path — rather than the bot's stored outbound path to the contact.
	pathBytes, pathHops := c.takePendingRawPath()
	msgPathLen := pathLen
	msgPathBytes := contactPath
	if len(pathBytes) > 0 {
		msgPathLen = pathHops
		msgPathBytes = pathBytes
	}

	msg := &Message{
		SenderKey:  fullKey,
		Text:       text,
		Timestamp:  time.Unix(int64(timestampRaw), 0),
		IsOutgoing: false,
		Status:     "delivered",
		PathLength: msgPathLen,
		PathBytes:  msgPathBytes,
		MessageID:  fmt.Sprintf("%x-%d", prefix, timestampRaw),
	}

	c.mu.Lock()
	key := pubKeyHex(fullKey)
	c.messages[key] = append(c.messages[key], msg)
	c.mu.Unlock()

	c.logger.Info("Received contact message V3", "sender", senderName, "key", shortKeyHex(fullKey), "text", text,
		"pathLen", displayHops(msg.PathLength), "path", pathHex(msg.PathBytes, msg.PathLength))

	if c.histLog != nil {
		if err := c.histLog.LogDMIncoming(fullKey, senderName, text, msg.Timestamp); err != nil {
			c.logger.Error("historylog: failed to log incoming DM", "error", err)
		}
	}

	if c.onMessage != nil {
		c.onMessage(msg)
	}

	select {
	case c.incomingCh <- msg:
	default:
		c.logger.Warn("Incoming message channel full, dropping message")
	}
}

// parseChannelMessageV3 разбирает сообщение V3 в канале (код 0x11).
//
// Формат (из MyMesh.cpp, onChannelMessageRecv, app_target_ver >= 3):
//
//	[code(1)][snr(1)][res(1)][res(1)][channel(1)][path_len(1)][txt_type(1)][timestamp(4)][text]
//
// Префикс отправителя в канальных сообщениях НЕ передаётся.
// Имя отправителя встроено в текст в формате "Имя: текст".
func (c *MeshCoreClient) parseChannelMessageV3(data []byte) {
	// минимум: code(1)+snr+res+res(3)+channel(1)+path_len(1)+txt_type(1)+timestamp(4) = 11
	const minLen = 1 + 3 + 1 + 1 + 1 + 4
	if len(data) < minLen {
		c.logger.Warn("parseChannelMessageV3: data too short", "len", len(data))
		return
	}
	channelIndex := int(data[4])
	// data[5] = path_len (кодированный байт; число прыжков в младших 6 битах)
	pathLen := decodeMsgHops(data[5])
	// data[6] = txt_type
	timestampRaw := binary.LittleEndian.Uint32(data[7:11])

	textStart := 11
	if textStart >= len(data) {
		c.logger.Warn("parseChannelMessageV3: text start beyond data", "textStart", textStart, "len", len(data))
		return
	}
	textBytes := data[textStart:]
	nullIdx := bytes.IndexByte(textBytes, 0)
	var text string
	if nullIdx >= 0 {
		text = string(textBytes[:nullIdx])
	} else {
		text = string(textBytes)
	}

	// Поиск имени отправителя по первым байтам текста «Имя: »,
	// которые прошивка встраивает в sendGroupMessage.
	senderName := ""
	if idx := strings.Index(text, ": "); idx > 0 {
		senderName = text[:idx]
		text = text[idx+2:]
		c.logger.Debug("Extracted sender name from channel text V3", "name", senderName, "text", text)
	} else {
		senderName = fmt.Sprintf("?ch%d", channelIndex)
		c.logger.Warn("parseChannelMessageV3: no sender prefix in text")
	}

	dedupKey := fmt.Sprintf("ch:%d-%d-%s-%s", channelIndex, timestampRaw, senderName, text)
	if c.isDuplicateMessage(dedupKey) {
		c.logger.Debug("Duplicate channel message dropped (sender retry)",
			"channel", channelIndex, "timestamp", timestampRaw, "sender", senderName, "text", text)
		return
	}

	msg := &ChannelMessage{
		ChannelIndex: channelIndex,
		SenderName:   senderName,
		Text:         text,
		Timestamp:    time.Unix(int64(timestampRaw), 0),
		IsOutgoing:   false,
		Status:       "delivered",
		PathLength:   pathLen,
		MessageID:    fmt.Sprintf("%d-%d", channelIndex, timestampRaw),
	}

	c.mu.Lock()
	c.channelMsgs[channelIndex] = append(c.channelMsgs[channelIndex], msg)
	c.mu.Unlock()

	if c.onChannelMessage != nil {
		c.onChannelMessage(msg)
	}

	// Publish to MQTT packets topic with decoded fields.
	// Correlate with the most recent pending 0x88 raw frame (same radio packet).
	c.mu.RLock()
	ch := c.channels[channelIndex]
	publisher := c.publisher
	c.mu.RUnlock()

	channelName := fmt.Sprintf("ch%d", channelIndex)
	var channelKeyHex string
	if ch != nil && ch.Name != "" {
		channelName = ch.Name
	}
	if ch != nil && len(ch.PSK) > 0 {
		channelKeyHex = hex.EncodeToString(ch.PSK)
	}

	if c.histLog != nil {
		if err := c.histLog.LogChannelIncoming(channelName, senderName, text, msg.Timestamp); err != nil {
			c.logger.Error("historylog: failed to log incoming channel message", "error", err)
		}
	}

	snr := float64(int8(data[1])) / 4.0
	rssi := 0.0 // V3 frame does not carry RSSI; use pending raw value if found

	// Correlate this message with its own 0x88 raw frame, matching on the GRP_TXT
	// type and channel hash rather than mere arrival order. A wrong match would
	// stamp this message with another packet's path; better to report no path than
	// the path of an unrelated packet.
	var psk []byte
	if ch != nil {
		psk = ch.PSK
	}
	rawData, rawRSSI := c.takeChannelRaw(psk)
	if rawData != nil {
		rssi = rawRSSI
	}

	if publisher != nil {
		publisher.QueueChannelPacket(rawData, false, snr, rssi, channelIndex, channelName, senderName, text, channelKeyHex)
	}

	// Extract path bytes from raw RF packet so the bot can show repeater IDs.
	// The path is only on the wire for live (just-delivered) messages; those
	// synced from the device's history arrive without a correlated 0x88 frame.
	if pathBytes, hopCount := extractPathFromRaw(rawData); len(pathBytes) > 0 {
		msg.PathBytes = pathBytes
		msg.PathLength = hopCount
	}

	c.logger.Info("Channel message V3 received",
		"channel", channelIndex, "sender", senderName, "text", text,
		"pathLen", displayHops(msg.PathLength), "path", pathHex(msg.PathBytes, msg.PathLength))

	// отправляем в канал входящих сообщений
	select {
	case c.incomingCh <- msg:
	default:
		c.logger.Warn("Incoming message channel full, dropping message")
	}
}

// pathHashCount returns the number of hops encoded in a MeshCore path_len byte
// (low 6 bits). See Packet.h getPathHashCount().
func pathHashCount(pathLen byte) int { return int(pathLen & 0x3F) }

// PathHashCount is the exported form of pathHashCount for callers outside this
// package (e.g. detecting a node's routing hash size from Contact.PathLength).
func PathHashCount(pathLen byte) int { return pathHashCount(pathLen) }

// pathHashSize returns the bytes-per-hop encoded in a MeshCore path_len byte
// (top 2 bits + 1 → 1, 2 or 3). See Packet.h getPathHashSize().
func pathHashSize(pathLen byte) int { return int((pathLen>>6)&0x03) + 1 }

// PathHashSize is the exported form of pathHashSize for callers outside this
// package (e.g. detecting whether a node still uses 1-byte routing hashes).
func PathHashSize(pathLen byte) int { return pathHashSize(pathLen) }

// pathByteLen returns the number of meaningful path bytes for an encoded
// path_len byte: hash_count × hash_size.
func pathByteLen(pathLen byte) int { return pathHashCount(pathLen) * pathHashSize(pathLen) }

// decodeMsgHops converts a companion message frame's path_len field to a hop
// count. The field is 0xFF for direct delivery (kept as the direct sentinel) or
// a flooded packet's encoded path_len byte, whose low 6 bits hold the hop count.
func decodeMsgHops(pathLen byte) int {
	if pathLen == pathLengthFlood {
		return pathLengthFlood
	}
	return pathHashCount(pathLen)
}

// pathHex renders a routing path as colon-separated hop IDs for logging, or
// "n/a" when the path is unavailable (e.g. a message synced from device history
// with no live packet). hopCount lets it group multi-byte hops correctly; bytes
// are grouped into len(path)/hopCount-byte hops, e.g. "8888:1cdb:619f:4444".
func pathHex(path []byte, hopCount int) string {
	if len(path) == 0 {
		return "n/a"
	}
	hashSize := 1
	if hopCount > 0 && hopCount != pathLengthFlood && len(path)%hopCount == 0 {
		hashSize = len(path) / hopCount
	}
	return hopsHex(path, hashSize)
}

// displayHops normalizes a path hop count for logging: the 0xFF "direct
// delivery" sentinel is shown as 0 hops, which reads more naturally in logs.
func displayHops(pathLen int) int {
	if pathLen == pathLengthFlood {
		return 0
	}
	return pathLen
}

// shortKeyHex renders a public key as its first 8 hex characters (4 bytes) for
// compact logging.
func shortKeyHex(pubKey []byte) string {
	h := fmt.Sprintf("%x", pubKey)
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// hopsHex renders path bytes as colon-separated hop IDs, grouping bytes into
// hashSize-byte hops, e.g. "8888:1cdb" for hashSize=2 or "db:58:79" for
// hashSize=1. Falls back to one group per byte when hashSize is invalid or does
// not evenly divide the path.
func hopsHex(path []byte, hashSize int) string {
	if hashSize < 1 || len(path)%hashSize != 0 {
		hashSize = 1
	}
	parts := make([]string, 0, len(path)/hashSize)
	for i := 0; i < len(path); i += hashSize {
		parts = append(parts, fmt.Sprintf("%x", path[i:i+hashSize]))
	}
	return strings.Join(parts, ":")
}

// takePendingRawPath returns the path hops from the oldest still-valid pending
// raw RF packet and removes it. The 0x88 raw frame arrives just before its
// decoded companion frame, so the oldest entry corresponds to this message.
// Returns nil, 0 when no live raw packet is available (e.g. a message synced
// from device history). Must be called from the frameHandler goroutine, which
// is the only writer of c.pendingRaw.
func (c *MeshCoreClient) takePendingRawPath() (pathBytes []byte, hopCount int) {
	for i := 0; i < len(c.pendingRaw); i++ {
		p := c.pendingRaw[i]
		if time.Since(p.arrivedAt) <= pendingRawTTL {
			c.pendingRaw = append(c.pendingRaw[:i], c.pendingRaw[i+1:]...)
			return extractPathFromRaw(p.rawData)
		}
	}
	return nil, 0
}

// takeChannelRaw finds and removes the oldest still-valid pending raw frame that
// belongs to the channel message being decoded. Matching on the GRP_TXT payload
// type and channel hash (when the PSK is known) stops an unrelated packet —
// advert, ACK, a different channel — that merely arrived within pendingRawTTL
// from lending its path to this message. Returns nil when no matching live raw
// is available (e.g. a message synced from device history). Must be called from
// the frameHandler goroutine, the only writer of c.pendingRaw.
func (c *MeshCoreClient) takeChannelRaw(psk []byte) (rawData []byte, rssi float64) {
	wantHash, haveHash := channelHashByte(psk)
	for i := 0; i < len(c.pendingRaw); i++ {
		p := c.pendingRaw[i]
		if time.Since(p.arrivedAt) > pendingRawTTL {
			continue
		}
		if !rawIsChannelMsg(p.rawData, wantHash, haveHash) {
			continue
		}
		c.pendingRaw = append(c.pendingRaw[:i], c.pendingRaw[i+1:]...)
		return p.rawData, p.rssi
	}
	return nil, 0
}

// extractPathFromRaw parses the path bytes out of a raw RF packet.
//
// Wire format (from Packet.cpp writeTo()):
//
//	[header(1)] [transport_codes(4) — only for TRANSPORT routes] [path_len(1)] [path...] [payload...]
//
// path_len byte encoding (from Packet.h):
//
//	hash_count = path_len & 0x3F   (bits 0–5, number of hops)
//	hash_size  = (path_len >> 6) + 1  (bits 6–7 → 1, 2, or 3 bytes per hop)
//
// Returns the raw path bytes and the hop count. Returns nil, 0 when data is
// too short or contains no hops.
func extractPathFromRaw(data []byte) (pathBytes []byte, hopCount int) {
	if len(data) < 2 {
		return nil, 0
	}
	routeType := data[0] & 0x03
	hasTransportCodes := routeType == 0x00 || routeType == 0x03
	headerLen := 2
	if hasTransportCodes {
		headerLen = 6
	}
	if len(data) < headerLen {
		return nil, 0
	}
	pathLenByte := data[headerLen-1]
	hashCount := pathHashCount(pathLenByte)
	if hashCount == 0 {
		return nil, 0
	}
	end := headerLen + pathByteLen(pathLenByte)
	if end > len(data) {
		return nil, 0
	}
	return data[headerLen:end], hashCount
}

// parseContactMessage разбирает сообщение от контакта (код 0x07).
//
// Формат (из MyMesh.cpp, queueMessage, app_target_ver < 3):
//
//	[code(1)][prefix(6)][path_len(1)][txt_type(1)][timestamp(4)][extra(4, только для signed)][text]
//
// path_len — метаданные из пакета (например, 0xFF = прямая доставка).
// Реальных байт пути в companion-фрейме нет.
func (c *MeshCoreClient) parseContactMessage(data []byte) {
	// минимум: code(1)+prefix(6)+path_len(1)+txt_type(1)+timestamp(4) = 13
	const minLen = 1 + 6 + 1 + 1 + 4
	if len(data) < minLen {
		c.logger.Warn("parseContactMessage: data too short", "len", len(data))
		return
	}
	prefix := data[1:7]
	// data[7] = path_len (кодированный байт; число прыжков в младших 6 битах)
	pathLen := decodeMsgHops(data[7])
	txtType := data[8]
	timestampRaw := binary.LittleEndian.Uint32(data[9:13])

	// Для signed-сообщений после timestamp идут 4 байта extra (sender_prefix)
	textStart := 13
	if txtType == txtTypeSigned {
		textStart = 17
	}
	if textStart >= len(data) {
		c.logger.Warn("parseContactMessage: no text data")
		return
	}
	textBytes := data[textStart:]
	nullIdx := bytes.IndexByte(textBytes, 0)
	var text string
	if nullIdx >= 0 {
		text = string(textBytes[:nullIdx])
	} else {
		text = string(textBytes)
	}

	var fullKey []byte
	var contactPath []byte
	c.mu.RLock()
	for _, contact := range c.contacts {
		if len(contact.PublicKey) >= 6 && bytes.Equal(contact.PublicKey[:6], prefix) {
			fullKey = contact.PublicKey
			contactPath = contact.Path
			break
		}
	}
	c.mu.RUnlock()
	if fullKey == nil {
		fullKey = prefix
		c.logger.Warn("parseContactMessage: unknown sender", "prefix", fmt.Sprintf("%x", prefix))
	}

	// Prefer the real path from the correlated raw RF packet over the bot's
	// stored outbound path to the contact (see parseContactMessageV3).
	pathBytes, pathHops := c.takePendingRawPath()
	msgPathLen := pathLen
	msgPathBytes := contactPath
	if len(pathBytes) > 0 {
		msgPathLen = pathHops
		msgPathBytes = pathBytes
	}

	msg := &Message{
		SenderKey:  fullKey,
		Text:       text,
		Timestamp:  time.Unix(int64(timestampRaw), 0),
		IsOutgoing: false,
		Status:     "delivered",
		PathLength: msgPathLen,
		PathBytes:  msgPathBytes,
		MessageID:  fmt.Sprintf("%x-%d", prefix, timestampRaw),
	}

	c.mu.Lock()
	key := pubKeyHex(fullKey)
	c.messages[key] = append(c.messages[key], msg)
	c.mu.Unlock()

	c.logger.Debug("Received contact message", "sender", shortKeyHex(fullKey), "text", text,
		"pathLen", displayHops(msg.PathLength), "path", pathHex(msg.PathBytes, msg.PathLength))

	if c.onMessage != nil {
		c.onMessage(msg)
	}

	// отправляем в канал входящих сообщений
	select {
	case c.incomingCh <- msg:
	default:
		c.logger.Warn("Incoming message channel full, dropping message")
	}
}

// parseChannelMessage разбирает сообщение в канале (код 0x08).
//
// Формат (из MyMesh.cpp, onChannelMessageRecv, app_target_ver < 3):
//
//	[code(1)][channel(1)][path_len(1)][txt_type(1)][timestamp(4)][text]
//
// Префикс отправителя в канальных сообщениях НЕ передаётся.
// Имя отправителя встроено в текст в формате "Имя: текст".
func (c *MeshCoreClient) parseChannelMessage(data []byte) {
	// минимум: code(1)+channel(1)+path_len(1)+txt_type(1)+timestamp(4) = 8
	const minLen = 1 + 1 + 1 + 1 + 4
	if len(data) < minLen {
		c.logger.Warn("parseChannelMessage: data too short", "len", len(data))
		return
	}
	channelIndex := int(data[1])
	// data[2] = path_len (кодированный байт; число прыжков в младших 6 битах)
	pathLen := decodeMsgHops(data[2])
	// data[3] = txt_type (для каналов всегда TXT_TYPE_PLAIN)
	timestampRaw := binary.LittleEndian.Uint32(data[4:8])

	textStart := 8
	if textStart >= len(data) {
		c.logger.Warn("parseChannelMessage: no text data")
		return
	}
	textBytes := data[textStart:]
	nullIdx := bytes.IndexByte(textBytes, 0)
	var text string
	if nullIdx >= 0 {
		text = string(textBytes[:nullIdx])
	} else {
		text = string(textBytes)
	}

	// Имя отправителя встроено прошивкой в текст: «Имя: сообщение»
	senderName := ""
	if idx := strings.Index(text, ": "); idx > 0 {
		senderName = text[:idx]
		text = text[idx+2:]
		c.logger.Debug("Extracted sender name from channel text", "name", senderName, "text", text)
	} else {
		senderName = fmt.Sprintf("?ch%d", channelIndex)
		c.logger.Warn("parseChannelMessage: no sender prefix in text")
	}

	msg := &ChannelMessage{
		ChannelIndex: channelIndex,
		SenderName:   senderName,
		Text:         text,
		Timestamp:    time.Unix(int64(timestampRaw), 0),
		IsOutgoing:   false,
		Status:       "delivered",
		PathLength:   pathLen,
		MessageID:    fmt.Sprintf("%d-%d", channelIndex, timestampRaw),
	}

	c.mu.Lock()
	c.channelMsgs[channelIndex] = append(c.channelMsgs[channelIndex], msg)
	c.mu.Unlock()

	if c.onChannelMessage != nil {
		c.onChannelMessage(msg)
	}

	// Publish to MQTT packets topic with decoded fields.
	c.mu.RLock()
	ch := c.channels[channelIndex]
	publisher := c.publisher
	c.mu.RUnlock()

	channelName := fmt.Sprintf("ch%d", channelIndex)
	var channelKeyHex string
	if ch != nil && ch.Name != "" {
		channelName = ch.Name
	}
	if ch != nil && len(ch.PSK) > 0 {
		channelKeyHex = hex.EncodeToString(ch.PSK)
	}

	// Correlate with the oldest pending 0x88 raw frame (arrives before decoded frame).
	var rawData []byte
	var snr, rssi float64
	for i := 0; i < len(c.pendingRaw); i++ {
		p := c.pendingRaw[i]
		if time.Since(p.arrivedAt) <= pendingRawTTL {
			rawData = p.rawData
			snr = p.snr
			rssi = p.rssi
			c.pendingRaw = append(c.pendingRaw[:i], c.pendingRaw[i+1:]...)
			break
		}
	}

	if publisher != nil {
		publisher.QueueChannelPacket(rawData, false, snr, rssi, channelIndex, channelName, senderName, text, channelKeyHex)
	}

	// Extract path bytes from the raw RF packet so the bot can show repeater IDs.
	if pathBytes, hopCount := extractPathFromRaw(rawData); len(pathBytes) > 0 {
		msg.PathBytes = pathBytes
		msg.PathLength = hopCount
	}

	c.logger.Info("Channel message received (V1)",
		"channel", channelIndex, "sender", fmt.Sprintf("%q", senderName), "text", fmt.Sprintf("%q", text),
		"pathLen", displayHops(msg.PathLength), "path", pathHex(msg.PathBytes, msg.PathLength))

	// отправляем в канал входящих сообщений
	select {
	case c.incomingCh <- msg:
	default:
		c.logger.Warn("Incoming message channel full, dropping message")
	}
}

func (c *MeshCoreClient) handleFrame(data []byte) {
	if len(data) == 0 {
		return
	}
	code := data[0]
	c.logger.Debug("handleFrame", "code", fmt.Sprintf("0x%02x", code), "len", len(data))
	switch code {
	case respCodeSelfInfo:
		c.parseSelfInfo(data)
	case respCodeDeviceInfo:
		c.parseDeviceInfo(data)
	case respCodeContactsStart:
		if len(data) >= 5 {
			total := binary.LittleEndian.Uint32(data[1:5])
			c.logger.Info("Contact sync started", "device_total", total)
		}
	case respCodeContact:
		if len(data) < contactFrameSize {
			c.parseContactV3(data)
		} else {
			c.parseContact(data)
		}
	case respCodeEndOfContacts:
		c.mu.RLock()
		cached := len(c.contacts)
		c.mu.RUnlock()
		c.logger.Info("Contact sync complete", "contacts", cached)
	case respCodeChannelInfo:
		if len(data) < 2 {
			c.logger.Warn("ChannelInfo too short", "len", len(data))
			return
		}
		c.parseChannelInfo(data)
	case respCodeContactMsgRecv:
		c.parseContactMessage(data)
		_ = c.sendSyncNextMessage()
	case respCodeContactMsgRecvV3:
		c.parseContactMessageV3(data)
		_ = c.sendSyncNextMessage()
	case respCodeChannelMsgRecv:
		c.parseChannelMessage(data)
		_ = c.sendSyncNextMessage()
	case respCodeChannelMsgRecvV3:
		c.parseChannelMessageV3(data)
		_ = c.sendSyncNextMessage()
	case respCodeSent:
		c.parseSent(data)
	case respCodeNoMoreMessages:
		c.logger.Debug("Received respCodeNoMoreMessages")
		if c.onNoMoreMessages != nil {
			c.onNoMoreMessages()
		}
	case pushCodeSendConfirmed:
		c.parseSendConfirmed(data)
	case pushCodePathUpdated:
		c.parsePathUpdated(data)
	case pushCodeMsgWaiting:
		// Есть сообщения в очереди, запросить
		c.logger.Debug("Messages waiting, requesting sync")
		_ = c.sendSyncNextMessage()
	case respCodeBattAndStorage:
		c.parseBatteryAndStorage(data)
	case respCodeStats:
		c.parseStats(data)
	case respCodeAutoAddConfig:
		c.parseAutoAddConfig(data)
	case respCodeCustomVars:
		c.parseCustomVars(data)
	case respCodeOk:
		c.parseOk(data)
	case respCodeErr:
		var errCode byte
		var errName string
		if len(data) >= 2 {
			errCode = data[1]
			errName = errCodeName(errCode)
		}
		c.logger.Error("Received error response", "err_code", errCode, "err_name", errName)
		c.notifyErr(fmt.Errorf("device error %d (%s)", errCode, errName))
	case pushCodeAdvert:
		c.parseAdvert(data)
	case pushCodeNewAdvert:
		c.parseNewAdvert(data)
	case pushCodeContactDeleted:
		c.parseContactDeleted(data)
	case pushCodeContactsFull:
		c.handleContactsFull()
	case pushCodeLogRxData:
		c.parseLogRxData(data)

	// Priority 1 — full parse + MQTT
	case pushCodeTraceData:
		c.parseTraceData(data)
	case pushCodeControlData:
		c.parseControlData(data)

	// Priority 2 — full parse + MQTT (response via MSG_WAITING queue)
	case respCodeChannelDataRecv:
		c.parseChannelData(data)
		_ = c.sendSyncNextMessage()

	// Priority 3 — log + MQTT publish
	case pushCodeRawData:
		snr := float64(0)
		rssi := float64(0)
		if len(data) >= 3 {
			snr = float64(int8(data[1])) / 4.0
			rssi = float64(int8(data[2]))
		}
		c.logger.Debug("Raw data packet", "snr", snr, "rssi", rssi,
			"payload", fmt.Sprintf("%x", data[3:]))
		c.publishPendingRawToMQTT(snr, rssi)

	case pushCodeLoginSuccess:
		if len(data) >= 7 {
			c.logger.Info("Login success", "pubkey_prefix", fmt.Sprintf("%x", data[1:7]))
		}
		c.publishPendingRawToMQTT(0, 0)

	case pushCodeLoginFail:
		if len(data) >= 7 {
			c.logger.Warn("Login failed", "pubkey_prefix", fmt.Sprintf("%x", data[1:7]))
		}
		c.publishPendingRawToMQTT(0, 0)

	case pushCodeStatusResponse:
		c.logger.Debug("Status response", "data", fmt.Sprintf("%x", data))
		c.publishPendingRawToMQTT(0, 0)

	case pushCodeTelemetryResponse:
		c.logger.Debug("Telemetry response", "data", fmt.Sprintf("%x", data))
		c.publishPendingRawToMQTT(0, 0)

	case pushCodeBinaryResponse:
		c.logger.Debug("Binary response", "data", fmt.Sprintf("%x", data))
		c.publishPendingRawToMQTT(0, 0)

	case pushCodePathDiscoveryResponse:
		c.logger.Debug("Path discovery response", "data", fmt.Sprintf("%x", data))
		c.publishPendingRawToMQTT(0, 0)

	// Reply to cmdGetDeviceTime: the device's current RTC (unix seconds).
	case respCodeCurrTime:
		if len(data) >= 5 {
			deviceTime := int64(binary.LittleEndian.Uint32(data[1:5]))
			c.logger.Debug("Device current time",
				"device_unix", deviceTime,
				"device_utc", time.Unix(deviceTime, 0).UTC().Format(time.RFC3339))
		} else {
			c.logger.Warn("Short CURR_TIME frame", "data", fmt.Sprintf("%x", data))
		}

	// Priority 4 — constants only; no command sender yet
	case respCodeExportContact, respCodePrivateKey,
		respCodeDisabled, respCodeSignStart, respCodeSignature,
		respCodeAdvertPath, respCodeTuningParams, respCodeDefaultFloodScope:
		c.logger.Debug("Unhandled response code (no command sender yet)",
			"code", fmt.Sprintf("0x%02x", data[0]), "len", len(data))

	default:
		c.logger.Warn("Unknown frame code", "code", fmt.Sprintf("0x%02x", code))
	}
}

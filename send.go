package meshcore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// ============================================================================
// Отправка команд (сборка фреймов)
// ============================================================================

func (c *MeshCoreClient) sendAppStart() error {
	// [cmd(1)][reserved(7)][app_name][0x00]
	// Firmware checks len >= 8, reads app_name from cmd_frame[8].
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdAppStart)
	buf.Write([]byte{0, 0, 0, 0, 0, 0, 0}) // 7 reserved bytes (bytes 1-7)
	buf.WriteString("MeshCoreOpen")
	buf.WriteByte(0)
	data := buf.Bytes()
	c.logger.Debug("Sending AppStart", "data", fmt.Sprintf("%x", data))
	return c.write(data)
}

func (c *MeshCoreClient) sendDeviceQuery() error {
	data := []byte{cmdDeviceQuery, appProtocolVersion}
	c.logger.Debug("Sending DeviceQuery", "data", fmt.Sprintf("%x", data))
	return c.write(data)
}

func (c *MeshCoreClient) sendGetContacts(since uint32) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdGetContacts)
	if since > 0 {
		_ = binary.Write(buf, binary.LittleEndian, since)
	}
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendGetChannel(index byte) error {
	return c.write([]byte{cmdGetChannel, index})
}

func (c *MeshCoreClient) sendSetChannel(index byte, name string, psk []byte) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdSetChannel)
	buf.WriteByte(index)
	// имя: 32 байта, null-padded
	nameBytes := []byte(name)
	if len(nameBytes) > 31 {
		nameBytes = nameBytes[:31]
	}
	buf.Write(nameBytes)
	for i := len(nameBytes); i < 32; i++ {
		buf.WriteByte(0)
	}
	// PSK: 16 байт, нули если короче
	pskPadded := make([]byte, 16)
	copy(pskPadded, psk)
	buf.Write(pskPadded)
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendTextMessage(recipient []byte, text string, attempt byte, timestamp uint32) (string, error) {
	// [cmd(1)][txt_type(1)][attempt(1)][timestamp(4)][pub_key_prefix(6)][text][0x00]
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdSendTxtMsg)
	buf.WriteByte(txtTypePlain)
	buf.WriteByte(attempt)
	_ = binary.Write(buf, binary.LittleEndian, timestamp)
	if len(recipient) < 6 {
		return "", fmt.Errorf("recipient key too short")
	}
	buf.Write(recipient[:6])
	buf.WriteString(text)
	buf.WriteByte(0)

	// Генерируем ID сообщения так же, как в парсере
	messageID := fmt.Sprintf("%x-%d", recipient[:6], timestamp)

	if err := c.write(buf.Bytes()); err != nil {
		return messageID, err
	}
	return messageID, nil
}

// подтверждение отправки
func (c *MeshCoreClient) notifySent() {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()
	if c.sentConfirmCh != nil {
		c.sentConfirmCh <- nil
		c.sentConfirmCh = nil
	}
}

// clearSentConfirm clears the shared sent-confirmation slot, but only if it
// still points at the channel this caller registered. This avoids clobbering a
// confirmation slot that a newer send has already installed.
func (c *MeshCoreClient) clearSentConfirm(ch chan error) {
	c.confirmMu.Lock()
	if c.sentConfirmCh == ch {
		c.sentConfirmCh = nil
	}
	c.confirmMu.Unlock()
}

// подтверждение доставки
func (c *MeshCoreClient) notifyDelivered() {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()
	if c.deliveredConfirmCh != nil {
		c.deliveredConfirmCh <- nil
		c.deliveredConfirmCh = nil
	}
}

// notifyErr сигнализирует об ошибке всем ожидающим каналам подтверждения.
func (c *MeshCoreClient) notifyErr(err error) {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()
	if c.sentConfirmCh != nil {
		c.sentConfirmCh <- err
		c.sentConfirmCh = nil
	}
	if c.deliveredConfirmCh != nil {
		c.deliveredConfirmCh <- err
		c.deliveredConfirmCh = nil
	}
}

func errCodeName(code byte) string {
	switch code {
	case errCodeUnsupportedCmd:
		return "unsupported_cmd"
	case errCodeNotFound:
		return "not_found"
	case errCodeTableFull:
		return "table_full"
	case errCodeBadState:
		return "bad_state"
	case errCodeFileIOError:
		return "file_io_error"
	case errCodeIllegalArg:
		return "illegal_arg"
	default:
		return "unknown"
	}
}

// sendChannelTextMessage returns the timestamp embedded in the command so
// callers can build a matching synthetic RF packet with the same timestamp.
func (c *MeshCoreClient) sendChannelTextMessage(channelIndex byte, text string) (uint32, error) {
	return c.sendChannelTextMessageTS(channelIndex, text, uint32(time.Now().Unix()))
}

// sendChannelTextMessageTS is like sendChannelTextMessage but uses a caller-
// supplied timestamp. Because the firmware derives a packet's dedup hash from
// its payload — which begins with this timestamp (see BaseChatMesh::sendGroupMessage
// and Packet::calculatePacketHash in the firmware) — reusing one timestamp across
// repeated transmissions yields an identical packet_hash, so receivers dedup it
// and show the message only once.
func (c *MeshCoreClient) sendChannelTextMessageTS(channelIndex byte, text string, timestamp uint32) (uint32, error) {
	// [cmd(1)][txt_type(1)][channel(1)][timestamp(4)][text][0x00]
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdSendChannelTxtMsg)
	buf.WriteByte(txtTypePlain)
	buf.WriteByte(channelIndex)
	_ = binary.Write(buf, binary.LittleEndian, timestamp)
	buf.WriteString(text)
	buf.WriteByte(0)
	return timestamp, c.write(buf.Bytes())
}

// sendGetDeviceTime queries the device's current RTC. The reply arrives as a
// respCodeCurrTime frame.
func (c *MeshCoreClient) sendGetDeviceTime() error {
	return c.write([]byte{cmdGetDeviceTime})
}

func (c *MeshCoreClient) sendSetDeviceTime(timestamp uint32) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdSetDeviceTime)
	_ = binary.Write(buf, binary.LittleEndian, timestamp)
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendGetBatteryAndStorage() error {
	return c.write([]byte{cmdGetBattAndStorage})
}

func (c *MeshCoreClient) sendGetStats(statsType byte) error {
	return c.write([]byte{cmdGetStats, statsType})
}

func (c *MeshCoreClient) sendSetAdvertName(name string) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdSetAdvertName)
	nameBytes := []byte(name)
	if len(nameBytes) > maxNameSize-1 {
		nameBytes = nameBytes[:maxNameSize-1]
	}
	buf.Write(nameBytes)
	buf.WriteByte(0)
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendSetAdvertLatLon(lat, lon float64) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdSetAdvertLatLon)
	_ = binary.Write(buf, binary.LittleEndian, int32(lat*1e6))
	_ = binary.Write(buf, binary.LittleEndian, int32(lon*1e6))
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendResetPath(pubKey []byte) error {
	if len(pubKey) < 32 {
		return fmt.Errorf("reset path requires full 32-byte public key")
	}
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdResetPath)
	buf.Write(pubKey[:32])
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendUpdateContact(contact *Contact) error {
	if len(contact.PublicKey) < 32 {
		return fmt.Errorf("update contact requires full 32-byte public key")
	}
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdAddUpdateContact)
	buf.Write(contact.PublicKey[:32])
	buf.WriteByte(byte(contact.Type))
	buf.WriteByte(byte(contact.Flags))
	// PathLength хранит кодированный байт out_path_len прошивки (число прыжков +
	// размер хеша, или 0xFF для flood). Пишем его обратно как есть, чтобы устройство
	// корректно сохранило маршрут для многобайтных хопов — не путать с числом байт
	// в contact.Path.
	buf.WriteByte(byte(contact.PathLength))
	// Путь: 64 байта
	pathPadded := make([]byte, 64)
	copy(pathPadded, contact.Path)
	buf.Write(pathPadded)
	// Имя: 32 байта null-terminated
	nameBytes := []byte(contact.Name)
	if len(nameBytes) > 31 {
		nameBytes = nameBytes[:31]
	}
	buf.Write(nameBytes)
	for i := len(nameBytes); i < 32; i++ {
		buf.WriteByte(0)
	}
	// Таймстемп (last_advert_timestamp)
	_ = binary.Write(buf, binary.LittleEndian, uint32(time.Now().Unix()))
	// Координаты (если есть)
	latInt, lonInt := int32(0), int32(0)
	if contact.Latitude != nil && contact.Longitude != nil {
		latInt = int32(*contact.Latitude * 1e6)
		lonInt = int32(*contact.Longitude * 1e6)
	}
	_ = binary.Write(buf, binary.LittleEndian, latInt)
	_ = binary.Write(buf, binary.LittleEndian, lonInt)
	// last_modified (optional)
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendGetContactByKey(pubKey []byte) error {
	if len(pubKey) < pubKeySize {
		return fmt.Errorf("get contact by key requires full %d-byte public key", pubKeySize)
	}
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdGetContactByKey)
	buf.Write(pubKey[:pubKeySize])
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendRemoveContact(pubKey []byte) error {
	if len(pubKey) < 32 {
		return fmt.Errorf("remove contact requires full 32-byte public key")
	}
	buf := new(bytes.Buffer)
	buf.WriteByte(cmdRemoveContact)
	buf.Write(pubKey[:32])
	return c.write(buf.Bytes())
}

func (c *MeshCoreClient) sendSyncNextMessage() error {
	return c.write([]byte{cmdSyncNextMessage})
}

func (c *MeshCoreClient) sendReboot() error {
	return c.write([]byte{cmdReboot, 'r', 'e', 'b', 'o', 'o', 't'})
}

func (c *MeshCoreClient) write(data []byte) error {
	c.mu.RLock()
	conn, connected := c.conn, c.connected
	c.mu.RUnlock()
	if !connected || conn == nil {
		return fmt.Errorf("not connected")
	}

	err := conn.WriteFrame(data)
	if err != nil {
		c.logger.Error("Write failed", "error", err)
		// Detach: write() also runs on the frame-handler goroutine (the
		// post-message sendSyncNextMessage, among others), and markDisconnected
		// waits for that very handler's handlersDone — calling it synchronously
		// here would deadlock with itself. Same rule as connectionLost.
		go c.markDisconnected()
	}
	return err
}

// markDisconnected transitions the client to a disconnected state after a
// write or read failure. It is safe to call concurrently; subsequent calls are no-ops.
func (c *MeshCoreClient) markDisconnected() {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return
	}
	c.connected = false
	select {
	case <-c.doneCh:
	default:
		close(c.doneCh)
	}
	// Wait only for this connection generation's frame handler; a concurrent
	// Reconnect may already have started a new one on a fresh done channel.
	handlersDone := c.handlersDone
	conn := c.conn
	c.mu.Unlock()
	<-handlersDone
	if conn != nil {
		_ = conn.Close()
	}
	c.logger.Warn("Connection lost; reconnection required")
}

// IsConnected reports whether the client currently has an active connection.
func (c *MeshCoreClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Reconnect re-establishes the connection (BLE, serial or TCP) to the
// previously used address/port.
func (c *MeshCoreClient) Reconnect() error {
	c.mu.RLock()
	reconnectFn := c.reconnect
	addr := c.address
	c.mu.RUnlock()
	if reconnectFn != nil {
		return reconnectFn()
	}
	return c.Connect(addr)
}

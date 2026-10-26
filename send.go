package meshcore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"go.bug.st/serial"
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
	if !c.connected {
		c.mu.RUnlock()
		return fmt.Errorf("not connected")
	}
	isSerial := c.serialPort != nil
	c.mu.RUnlock()

	var err error
	if isSerial {
		err = c.serialWriteFramed(data)
	} else {
		c.mu.RLock()
		if c.rxChar == nil {
			c.mu.RUnlock()
			return fmt.Errorf("not connected")
		}
		c.writeMu.Lock()
		_, err = c.rxChar.Write(data)
		c.writeMu.Unlock()
		c.mu.RUnlock()
	}
	if err != nil {
		c.logger.Error("Write failed", "error", err)
		c.markDisconnected()
	}
	return err
}

// serialWriteFramed wraps data in the MeshCore serial frame format:
// '<' + len_LSB + len_MSB + payload
func (c *MeshCoreClient) serialWriteFramed(data []byte) error {
	frame := make([]byte, 3+len(data))
	frame[0] = '<'
	binary.LittleEndian.PutUint16(frame[1:3], uint16(len(data)))
	copy(frame[3:], data)
	c.writeMu.Lock()
	_, err := c.serialPort.Write(frame)
	c.writeMu.Unlock()
	return err
}

// serialReadLoop reads framed packets from the serial port and feeds them into
// notifyCh. Frame format: '>' + len_LSB + len_MSB + payload (device→host).
func (c *MeshCoreClient) serialReadLoop() {
	defer c.wg.Done()

	const (
		stateIdle       = 0
		stateGotMarker  = 1
		stateGotLen1    = 2
		stateCollecting = 3
	)

	state := stateIdle
	var lenBuf [2]byte
	var frameLen int
	var frame []byte
	oneByte := make([]byte, 1)

	for {
		select {
		case <-c.doneCh:
			return
		default:
		}

		n, err := c.serialPort.Read(oneByte)
		if err != nil {
			if err == io.EOF {
				continue
			}
			select {
			case <-c.doneCh:
				return
			default:
				c.logger.Error("Serial read error", "error", err)
				c.markDisconnected()
				return
			}
		}
		if n == 0 {
			continue
		}
		b := oneByte[0]

		switch state {
		case stateIdle:
			if b == '>' {
				state = stateGotMarker
			}
		case stateGotMarker:
			lenBuf[0] = b
			state = stateGotLen1
		case stateGotLen1:
			lenBuf[1] = b
			frameLen = int(binary.LittleEndian.Uint16(lenBuf[:]))
			if frameLen == 0 || frameLen > 512 {
				c.logger.Warn("Serial frame: invalid length, resetting", "len", frameLen)
				state = stateIdle
				continue
			}
			frame = make([]byte, 0, frameLen)
			state = stateCollecting
		case stateCollecting:
			frame = append(frame, b)
			if len(frame) >= frameLen {
				frameCopy := make([]byte, len(frame))
				copy(frameCopy, frame)
				select {
				case c.notifyCh <- frameCopy:
				default:
					c.logger.Warn("Serial notification channel full, dropping frame")
				}
				state = stateIdle
			}
		}
	}
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
	isSerial := c.serialPort != nil
	c.mu.Unlock()
	c.wg.Wait() // wait for goroutines to exit
	if isSerial {
		_ = c.serialPort.Close()
		c.logger.Warn("Serial connection lost; reconnection required")
	} else if c.device != nil {
		_ = c.device.Disconnect()
		c.logger.Warn("Connection lost; reconnection required")
	}
}

// IsConnected reports whether the client currently has an active connection.
func (c *MeshCoreClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Reconnect re-establishes the connection (BLE or serial) to the previously used address/port.
func (c *MeshCoreClient) Reconnect() error {
	c.mu.RLock()
	isSerial := c.serialPort != nil || c.serialPortPath != ""
	portPath := c.serialPortPath
	baud := c.serialBaud
	addr := c.address
	c.mu.RUnlock()
	if isSerial {
		return c.ConnectSerial(portPath, baud)
	}
	return c.Connect(addr)
}

// ConnectSerial opens a serial port and initialises the MeshCore protocol over it.
// portPath is the OS path to the serial device (e.g. "/dev/ttyUSB0", "COM3").
// baud should be 115200 for standard MeshCore firmware.
func (c *MeshCoreClient) ConnectSerial(portPath string, baud int) error {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	c.serialPortPath = portPath
	c.serialBaud = baud
	select {
	case <-c.doneCh:
	default:
		close(c.doneCh)
	}
	c.doneCh = make(chan struct{})
	c.mu.Unlock()

	// Drain stale notifications from any previous connection.
drainLoop:
	for {
		select {
		case <-c.notifyCh:
		default:
			break drainLoop
		}
	}

	mode := &serial.Mode{BaudRate: baud}
	port, err := serial.Open(portPath, mode)
	if err != nil {
		return fmt.Errorf("open serial port %s: %w", portPath, err)
	}

	c.mu.Lock()
	c.serialPort = port
	c.connected = true
	c.mu.Unlock()

	c.logger.Info("Serial port opened", "port", portPath, "baud", baud)

	// Start the serial read loop (feeds notifyCh like BLE notifications do).
	c.wg.Add(1)
	go c.serialReadLoop()

	// Start the frame handler goroutine (same as BLE path).
	c.wg.Add(1)
	go c.frameHandler()

	if err := c.sendAppStart(); err != nil {
		c.Disconnect()
		return err
	}
	time.Sleep(200 * time.Millisecond)

	if err := c.sendDeviceQuery(); err != nil {
		c.Disconnect()
		return err
	}
	time.Sleep(200 * time.Millisecond)

	if err := c.sendSetDeviceTime(uint32(time.Now().Unix())); err != nil {
		c.logger.Warn("Failed to sync device time", "error", err)
	} else {
		c.logger.Info("Device time synced")
	}
	return nil
}

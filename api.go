package meshcore

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// Публичные методы API
// ============================================================================

func (c *MeshCoreClient) GetSelfInfo() *SelfInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.selfInfo
}

func (c *MeshCoreClient) GetContacts() map[string]*Contact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// возвращаем копию
	res := make(map[string]*Contact)
	for k, v := range c.contacts {
		res[k] = v
	}
	return res
}

func (c *MeshCoreClient) GetChannels() map[int]*Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make(map[int]*Channel)
	for k, v := range c.channels {
		res[k] = v
	}
	return res
}

// FindContactByKeyPrefix returns the contact whose public key starts with the
// given prefix, or nil if none matches. Used to resolve a message sender from
// the short (6-byte) key carried in incoming messages.
func (c *MeshCoreClient) FindContactByKeyPrefix(prefix []byte) *Contact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ct := range c.contacts {
		if len(ct.PublicKey) >= len(prefix) && bytes.Equal(ct.PublicKey[:len(prefix)], prefix) {
			return ct
		}
	}
	return nil
}

// FindContactByName returns the first contact with the given name, or nil.
func (c *MeshCoreClient) FindContactByName(name string) *Contact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ct := range c.contacts {
		if ct.Name == name {
			return ct
		}
	}
	return nil
}

// GetChannelByName returns the channel with the given name, or nil. Empty
// channel slots are skipped.
func (c *MeshCoreClient) GetChannelByName(name string) *Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ch := range c.channels {
		if !ch.IsEmpty && ch.Name == name {
			return ch
		}
	}
	return nil
}

// SelfName returns the device's own advertised name, or "" if unknown.
func (c *MeshCoreClient) SelfName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.selfInfo != nil {
		return c.selfInfo.Name
	}
	return ""
}

func (c *MeshCoreClient) SyncTime() error {
	now := uint32(time.Now().Unix())
	return c.sendSetDeviceTime(now)
}

func (c *MeshCoreClient) RequestContacts(since uint32) error {
	return c.sendGetContacts(since)
}

func (c *MeshCoreClient) RequestChannels(maxChannels int) error {
	for i := 0; i < maxChannels; i++ {
		if err := c.sendGetChannel(byte(i)); err != nil {
			return err
		}
	}
	return nil
}

// SendMessageWithRetry отправляет личное сообщение с повторными попытками.
//
// waitForDelivery: ждать pushCodeSendConfirmed (true) или respCodeSent (false).
// savedPathTries:  сколько попыток сделать с сохранённым путём (минимум 1).
// resetPathTries:  сколько дополнительных попыток сделать со сбросом пути.
// timeout:         таймаут ожидания подтверждения для каждой попытки.
//
// Пример: savedPathTries=3, resetPathTries=3 — сначала 3 попытки по сохранённому
// пути, затем ещё 3 с принудительным сбросом пути перед каждой попыткой.
// formatPath renders a contact's routing path as hex hop IDs for logging, or
// "flood" when the path is unknown (flood-routed). pathLen is the firmware's
// encoded out_path_len byte: it carries the hop count (low 6 bits) and the
// bytes-per-hop (top 2 bits), so multi-byte hops are grouped correctly, e.g.
// "8888:1cdb". "direct" is shown for a zero-hop path.
func formatPath(pathLen int, path []byte) string {
	if pathLen == pathLengthFlood {
		return "flood"
	}
	size := pathHashSize(byte(pathLen))
	n := pathByteLen(byte(pathLen))
	if n > len(path) {
		n = len(path)
	}
	if n == 0 {
		return "direct"
	}
	return hopsHex(path[:n], size)
}

func (c *MeshCoreClient) SendMessageWithRetry(contact *Contact, text string, waitForDelivery bool, savedPathTries, resetPathTries int, timeout time.Duration) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	defer time.Sleep(2 * time.Second) // частая отправка приводит к пропаже сообщений

	if savedPathTries < 1 {
		savedPathTries = 1
	}
	totalAttempts := savedPathTries + resetPathTries

	// One timestamp shared by every attempt. The firmware embeds it in the
	// message payload as the sender-side message identity, so reusing it makes
	// all retransmissions the SAME logical message for receivers (and our own
	// dedup), instead of N separate messages. Only the attempt byte differs
	// between tries — that keeps each packet_hash unique so mesh repeaters
	// re-flood the retry instead of suppressing it as a duplicate. This mirrors
	// SendChannelMessageWithRetry, which reuses one timestamp for the same reason.
	timestamp := uint32(time.Now().Unix())

	c.logger.Info("Sending private message with retry",
		"key", shortKeyHex(contact.PublicKey), "name", contact.Name,
		"text", text, "waitForDelivery", waitForDelivery,
		"savedPathTries", savedPathTries, "resetPathTries", resetPathTries,
		"pathLen", displayHops(decodeMsgHops(byte(contact.PathLength))), "path", formatPath(contact.PathLength, contact.Path))

	// A single confirmation channel for the WHOLE retry sequence. The companion
	// device remembers the expected ACK of every attempt (its expected_ack_table),
	// so a delivery push (pushCodeSendConfirmed) may arrive for ANY earlier try —
	// during a later try's wait window or even after the last send. Keeping one
	// registered channel for the entire sequence means a late ACK for any try is
	// still counted as delivered instead of being dropped between attempts.
	confirmCh := make(chan error, 1)
	defer func() {
		c.confirmMu.Lock()
		if c.deliveredConfirmCh == confirmCh {
			c.deliveredConfirmCh = nil
		}
		if c.sentConfirmCh == confirmCh {
			c.sentConfirmCh = nil
		}
		c.confirmMu.Unlock()
	}()

	// arm (re)installs our shared confirmation slot; a previous attempt's device
	// error consumes the slot, so we re-arm before each send.
	arm := func() {
		c.confirmMu.Lock()
		if waitForDelivery {
			c.deliveredConfirmCh = confirmCh
		} else {
			c.sentConfirmCh = confirmCh
		}
		c.confirmMu.Unlock()
	}

	confirmed := func() error {
		if c.histLog != nil {
			if err := c.histLog.LogDMOutgoing(contact.PublicKey, text, time.Now()); err != nil {
				c.logger.Error("historylog: failed to log outgoing DM", "error", err)
			}
		}
		c.publishTXDirectMessage(contact, text, timestamp)
		return nil
	}

	rejections := 0
	for attempt := 0; attempt < totalAttempts; attempt++ {
		if attempt >= savedPathTries {
			if contact.PathLength != pathLengthFlood {
				c.logger.Info("Resetting contact path before retry",
					"attempt", attempt, "name", contact.Name, "key", shortKeyHex(contact.PublicKey))
				if err := c.sendResetPath(contact.PublicKey); err != nil {
					c.logger.Warn("Path reset failed", "error", err)
				}
				time.Sleep(1 * time.Second)
			} else {
				c.logger.Debug("Skipping path reset, contact is flood-routed",
					"attempt", attempt, "name", contact.Name)
			}
		}

		// Arm after the path-reset command (whose own reply must not land on our
		// slot) and just before the send, so we cannot miss a fast response.
		arm()

		if _, err := c.sendTextMessage(contact.PublicKey, text, byte(attempt), timestamp); err != nil {
			if attempt == totalAttempts-1 {
				break
			}
			time.Sleep(timeout)
			continue
		}

		select {
		case sendErr := <-confirmCh:
			if sendErr != nil {
				rejections++
				c.logger.Warn("Device rejected message, retrying",
					"error", sendErr, "attempt", attempt, "name", contact.Name)
				time.Sleep(2 * time.Second)
				continue
			}
			return confirmed()
		case <-time.After(timeout):
			// No confirmation yet. Leave the slot armed and send the next try; a
			// late ACK for this try (or any earlier one) still lands on confirmCh.
		}
	}

	// All tries sent without confirmation yet. Wait one more window for a late
	// delivery push belonging to any of the attempts.
	select {
	case sendErr := <-confirmCh:
		if sendErr == nil {
			return confirmed()
		}
		rejections++
	case <-time.After(timeout):
	}

	// Every attempt was rejected by the device (e.g. queue/table full): nothing
	// was ever queued, so signal ErrDeviceRejected and skip a pointless fallback.
	if rejections >= totalAttempts {
		return fmt.Errorf("%w after %d attempts", ErrDeviceRejected, totalAttempts)
	}
	return fmt.Errorf("%w: no %s confirmation after %d attempts (%d saved-path, %d reset-path)",
		ErrNoConfirmation,
		map[bool]string{true: "delivery", false: "sent"}[waitForDelivery],
		totalAttempts, savedPathTries, resetPathTries)
}

// SendMessage отправляет личное сообщение (без ожидания подтверждения).
func (c *MeshCoreClient) SendMessage(contact *Contact, text string) error {
	timestamp := uint32(time.Now().Unix())
	_, err := c.sendTextMessage(contact.PublicKey, text, 0, timestamp)
	if err == nil {
		c.publishTXDirectMessage(contact, text, timestamp)
	}
	return err
}

// channelTxGap is the pause between successive transmissions of the same
// channel packet when adaptive listening is unavailable. It spaces the
// transmissions out on air without flooding the device's command path or
// its TX duty-cycle budget.
const channelTxGap = 3 * time.Second

// defaultRepeatWindow is the fallback listen window used when the LoRa airtime
// cannot be estimated (no self-info / radio params). Normally the window is
// derived from real airtime by repeatWindowFor.
const defaultRepeatWindow = 4 * time.Second

// SendChannelMessageWithRetry sends a message to a channel, transmitting the
// SAME packet `transmissions` times to improve delivery reliability (the
// official apps do the same).
//
// Why multiple transmissions are needed: channel messages have no delivery ACK,
// and respCodeOk only means the firmware accepted the command and QUEUED the
// packet — not that it was transmitted. After OK, the firmware can still
// silently drop the queued packet (TX duty-cycle budget exhausted,
// channel-activity/CAD busy, radio TX start failure, or a full outbound queue)
// with no notification to the companion app, and its own seen-hash dedup hides
// RF rebroadcasts of the packet from us. So a single OK is a weak guarantee and
// the old "retry on rejection" loop never helped — OK arrives reliably on the
// first attempt while the packet is lost on air. Transmitting the identical
// packet several times gives each transmission an independent chance to win the
// channel; receivers dedup by packet_hash (see sendChannelTextMessageTS) and
// show the message only once.
//
// Note: this is our own node sending the same packet repeatedly; it is NOT the
// MeshCore network "repeat"/retransmit by repeater nodes.
//
// Adaptive sending: rather than blindly transmitting all copies, after each
// transmission we listen (via the firmware's pre-dedup 0x88 raw frames) for the
// mesh rebroadcasting our exact packet — recognised by its firmware dedup hash
// (see grpTxtPacketHash). Once we hear our message repeated, it is on the
// network and we stop, sparing the device and the channel the remaining copies.
// If we cannot compute the hash (no PSK / self info) or no repeater is in range,
// we fall back to spaced blind transmissions. `transmissions` is the MAX copies.
//
// waitForDelivery is accepted for API compatibility but ignored.
// maxRetries: per-transmission retries when the device explicitly REJECTS the
// command (0 = single attempt per transmission).
// transmissions: maximum number of times to transmit the packet (values < 1 → 1).
// timeout: how long to wait for the device OK per attempt.
func (c *MeshCoreClient) SendChannelMessageWithRetry(channel *Channel, text string, waitForDelivery bool, maxRetries, transmissions int, timeout time.Duration) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	defer time.Sleep(150 * time.Millisecond) // avoid flooding the device

	if transmissions < 1 {
		transmissions = 1
	}

	// Reuse one timestamp across every transmission so they share a packet_hash
	// and receivers (and our repeat watch) collapse them into a single message.
	timestamp := uint32(time.Now().Unix())

	// Compute the dedup hash the mesh will rebroadcast so we can stop early once
	// we hear our own message echoed. nil ⇒ adaptive listening unavailable.
	// Size the listen window from the real LoRa airtime of this packet so it
	// covers the repeater's flood backoff regardless of the radio preset.
	c.mu.RLock()
	selfInfo := c.selfInfo
	c.mu.RUnlock()
	var expectedHash []byte
	window := defaultRepeatWindow
	if selfInfo != nil {
		if pkt := buildGRPTXTPacket(channel.PSK, selfInfo.Name, text, timestamp); pkt != nil {
			// pkt is [header][path_len=0][payload…]; hash is over the payload.
			expectedHash = packetHash(payloadTypeGrpTxt, pkt[2:])
			// +6 ≈ transport codes (4) plus a repeater's appended path hashes.
			window = repeatWindowFor(selfInfo, len(pkt)+6)
		}
	}

	c.logger.Info("Sending channel message with retry",
		"channel", channel.Index, "name", channel.Name, "text", text,
		"max_transmissions", transmissions, "adaptive", expectedHash != nil,
		"repeat_window", window.Round(time.Millisecond))

	var repeatCh chan struct{}
	if expectedHash != nil {
		repeatCh = c.armRepeatWatch(expectedHash)
		defer c.disarmRepeatWatch()
	}

	queued := 0
	var lastErr error
	stoppedEarly := false
loop:
	for tx := 0; tx < transmissions; tx++ {
		if tx > 0 {
			// Before resending, give the mesh a chance to rebroadcast the
			// previous transmission. If we hear it, the message is on the
			// network — stop. Otherwise (window elapsed) resend.
			if repeatCh != nil {
				select {
				case <-repeatCh:
					stoppedEarly = true
					c.logger.Info("Channel message repeated by mesh, stopping early",
						"channel", channel.Name, "transmissions_sent", queued)
					break loop
				case <-time.After(window):
					c.logger.Debug("No mesh repeat heard, resending",
						"channel", channel.Name, "next_tx", tx+1)
				}
			} else {
				time.Sleep(channelTxGap)
			}
		}
		if err := c.sendChannelOnce(byte(channel.Index), text, timestamp, channel.Name, tx+1, transmissions, maxRetries, timeout); err != nil {
			lastErr = err
			c.logger.Warn("Channel message transmission not queued",
				"channel", channel.Name, "tx", fmt.Sprintf("%d/%d", tx+1, transmissions), "error", err)
			continue
		}
		queued++
	}

	if queued == 0 {
		return fmt.Errorf("channel message not accepted by device after %d transmissions: %w", transmissions, lastErr)
	}

	c.logger.Info("Channel message transmitted",
		"channel", channel.Name, "transmissions_sent", queued,
		"max_transmissions", transmissions, "confirmed_by_mesh", stoppedEarly)

	// Publish/log once: all transmissions are the same logical message.
	c.publishTXChannelPacket(channel, text, timestamp)
	if c.histLog != nil {
		if err := c.histLog.LogChannelOutgoing(channel.Name, text, time.Now()); err != nil {
			c.logger.Error("historylog: failed to log outgoing channel message", "error", err)
		}
	}
	return nil
}

// sendChannelOnce performs a single transmission of a channel packet (with the
// given fixed timestamp) and waits for the device to confirm it queued the
// packet (respCodeOk). It retries only when the device explicitly rejects the
// command.
func (c *MeshCoreClient) sendChannelOnce(channelIndex byte, text string, timestamp uint32, channelName string, txNum, transmissions, maxRetries int, timeout time.Duration) error {
	txLabel := fmt.Sprintf("%d/%d", txNum, transmissions)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}

		// Register BEFORE the write so we cannot miss a fast response.
		confirmCh := make(chan error, 1)
		c.confirmMu.Lock()
		c.sentConfirmCh = confirmCh
		c.confirmMu.Unlock()

		if _, err := c.sendChannelTextMessageTS(channelIndex, text, timestamp); err != nil {
			c.clearSentConfirm(confirmCh)
			if attempt == maxRetries {
				return fmt.Errorf("send failed after %d attempts: %w", maxRetries+1, err)
			}
			continue
		}

		select {
		case sendErr := <-confirmCh:
			if sendErr != nil {
				c.logger.Warn("Device rejected channel message, retrying",
					"error", sendErr, "channel", channelName, "tx", txLabel, "attempt", attempt)
				if attempt == maxRetries {
					return fmt.Errorf("channel message rejected after %d attempts: %w", maxRetries+1, sendErr)
				}
				continue
			}
			c.logger.Info("Channel message transmission queued by device",
				"channel", channelName, "tx", txLabel)
			return nil
		case <-time.After(timeout):
			c.clearSentConfirm(confirmCh)
			c.logger.Warn("Channel message: no device OK, retrying",
				"channel", channelName, "tx", txLabel, "attempt", attempt)
		}
	}
	return fmt.Errorf("channel message not accepted by device after %d attempts", maxRetries+1)
}

// SendChannelMessage отправляет сообщение в канал (без ожидания подтверждения).
func (c *MeshCoreClient) SendChannelMessage(channel *Channel, text string) error {
	ts, err := c.sendChannelTextMessage(byte(channel.Index), text)
	if err != nil {
		return err
	}
	c.publishTXChannelPacket(channel, text, ts)
	return nil
}

// SetContactPath updates a contact's stored route. pathLen must be the
// firmware-encoded out_path_len byte (hop count in the low 6 bits, bytes-per-hop
// in the top 2 bits, or 0xFF for flood), and path the matching meaningful path
// bytes — the same representation parseContact stores and sendUpdateContact
// writes back.
func (c *MeshCoreClient) SetContactPath(contact *Contact, path []byte, pathLen int) error {
	// Создаём копию с новым путём
	updated := *contact
	updated.Path = path
	updated.PathLength = pathLen
	return c.sendUpdateContact(&updated)
}

func (c *MeshCoreClient) ResetContactPath(contact *Contact) error {
	return c.sendResetPath(contact.PublicKey)
}

func (c *MeshCoreClient) RemoveContact(contact *Contact) error {
	return c.sendRemoveContact(contact.PublicKey)
}

func (c *MeshCoreClient) SetChannel(channelIndex int, name string, psk []byte) error {
	return c.sendSetChannel(byte(channelIndex), name, psk)
}

func (c *MeshCoreClient) SetNodeName(name string) error {
	return c.sendSetAdvertName(name)
}

func (c *MeshCoreClient) SetNodeLocation(lat, lon float64) error {
	return c.sendSetAdvertLatLon(lat, lon)
}

func (c *MeshCoreClient) SendSelfAdvert(flood bool) error {
	floodByte := byte(0)
	if flood {
		floodByte = 1
	}
	return c.write([]byte{cmdSendSelfAdvert, floodByte})
}

func (c *MeshCoreClient) Reboot() error {
	return c.sendReboot()
}

// RequestStats requests all stats types from the device (Core, Radio, Packets).
// Stats are accumulated and returned via the OnStats callback.
func (c *MeshCoreClient) RequestAllStats() error {
	if err := c.sendGetStats(STATS_TYPE_CORE); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := c.sendGetStats(STATS_TYPE_RADIO); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	return c.sendGetStats(STATS_TYPE_PACKETS)
}

// RequestStats requests radio stats (legacy method).
func (c *MeshCoreClient) RequestStats() error {
	return c.sendGetStats(STATS_TYPE_RADIO)
}

// GetStats returns the current accumulated stats.
// Returns nil if no stats have been received yet.
func (c *MeshCoreClient) GetStats() *CompanionRadioStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// FormatStats returns a human-readable string of all stats.
func (c *MeshCoreClient) FormatStats() string {
	stats := c.GetStats()
	if stats == nil {
		return "Stats: no data"
	}

	var b strings.Builder

	// Header
	b.WriteString("┌─ Radio Stats ─────────────────────────────\n")

	// Core stats
	b.WriteString(fmt.Sprintf("│ Battery:    %d mV (%.2f V)\n", stats.BatteryMv, float64(stats.BatteryMv)/1000.0))
	b.WriteString(fmt.Sprintf("│ Uptime:     %s\n", formatDuration(stats.UptimeSecs)))
	b.WriteString(fmt.Sprintf("│ Queue:      %d packets\n", stats.QueueLen))
	if stats.Errors > 0 {
		b.WriteString(fmt.Sprintf("│ Errors:     0x%04X\n", stats.Errors))
	}

	// Radio stats
	b.WriteString(fmt.Sprintf("│ Noise:      %d dBm\n", stats.NoiseFloorDbm))
	b.WriteString(fmt.Sprintf("│ RSSI:       %d dBm\n", stats.LastRssi))
	b.WriteString(fmt.Sprintf("│ SNR:        %.1f dB\n", stats.LastSnrDb))
	b.WriteString(fmt.Sprintf("│ TX Airtime: %s\n", formatDuration(stats.TxAirSecs)))
	b.WriteString(fmt.Sprintf("│ RX Airtime: %s\n", formatDuration(stats.RxAirSecs)))

	// Packet stats
	b.WriteString("├─ Packets ──────────────────────────────────\n")
	b.WriteString(fmt.Sprintf("│ Recv:       %d (flood: %d, direct: %d)\n", stats.PacketsRecv, stats.PacketsFloodRx, stats.PacketsDirectRx))
	b.WriteString(fmt.Sprintf("│ Sent:       %d (flood: %d, direct: %d)\n", stats.PacketsSent, stats.PacketsFloodTx, stats.PacketsDirectTx))
	if stats.PacketsRecvErrors > 0 {
		b.WriteString(fmt.Sprintf("│ RX Errors:  %d\n", stats.PacketsRecvErrors))
	}

	// Footer
	if !stats.LastUpdate.IsZero() {
		b.WriteString(fmt.Sprintf("└─ Updated: %s", stats.LastUpdate.Format("15:04:05")))
	} else {
		b.WriteString("└─")
	}

	return b.String()
}

// formatDuration converts seconds to human-readable duration string.
func formatDuration(secs uint32) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm %ds", secs/60, secs%60)
	}
	if secs < 86400 {
		return fmt.Sprintf("%dh %dm %ds", secs/3600, (secs%3600)/60, secs%60)
	}
	return fmt.Sprintf("%dd %dh %dm", secs/86400, (secs%86400)/3600, (secs%3600)/60)
}

func (c *MeshCoreClient) RequestBattery() error {
	return c.sendGetBatteryAndStorage()
}

// SyncNextMessage запрашивает следующее сообщение из очереди устройства.
func (c *MeshCoreClient) SyncNextMessage() error {
	return c.sendSyncNextMessage()
}

// OnNoMoreMessages устанавливает коллбэк для respCodeNoMoreMessages.
func (c *MeshCoreClient) OnNoMoreMessages(f func()) {
	c.onNoMoreMessages = f
}

func (c *MeshCoreClient) GetAllMessages() []*Message {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var all []*Message
	for _, msgs := range c.messages {
		all = append(all, msgs...)
	}
	return all
}

func (c *MeshCoreClient) GetAllChannelMessages() []*ChannelMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var all []*ChannelMessage
	for _, msgs := range c.channelMsgs {
		all = append(all, msgs...)
	}
	return all
}

// IncomingMessages возвращает канал для чтения входящих сообщений (*Message и *ChannelMessage).
func (c *MeshCoreClient) IncomingMessages() <-chan interface{} {
	return c.incomingCh
}

package meshcore

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.bug.st/serial"
	"tinygo.org/x/bluetooth"
)

// pendingRawPacket holds a raw RF packet from a 0x88 BLE frame while waiting
// to be correlated with the corresponding decoded 0x11 channel message frame.
// ErrDeviceRejected is returned by SendMessageWithRetry when the device
// explicitly rejects every send attempt (e.g. radio queue full, message too
// long). Callers should not retry with different confirmation settings because
// the failure is not about ACK delivery — the message was never queued.
var ErrDeviceRejected = fmt.Errorf("device rejected send")

// ErrNoConfirmation is returned by SendMessageWithRetry when every attempt was
// written to the device but no sent/delivered confirmation arrived within the
// timeout windows. The message may still reach its destination (a late ACK can
// arrive after the caller gave up), so a retry can duplicate the message.
var ErrNoConfirmation = fmt.Errorf("no send confirmation")

type pendingRawPacket struct {
	rawData   []byte
	snr       float64
	rssi      float64
	arrivedAt time.Time
}

// pendingRawTTL is how long a raw packet waits for a matching decoded frame
// before being published as-is (without decoded channel fields).
const pendingRawTTL = 2 * time.Second

// ============================================================================
// Основной клиент
// ============================================================================

type MeshCoreClient struct {
	adapter   *bluetooth.Adapter
	device    *bluetooth.Device
	rxChar    *bluetooth.DeviceCharacteristic
	txChar    *bluetooth.DeviceCharacteristic
	connected bool
	address   bluetooth.Address
	mu        sync.RWMutex

	// Состояние
	selfInfo          *SelfInfo
	contacts          map[string]*Contact // ключ – hex(pubKey)
	syncLastmod       uint32              // max lastmod seen across all parsed contact frames
	pendingNewContact *Contact            // contact from last pushCodeNewAdvert, may not be stored on device yet
	channels          map[int]*Channel
	messages          map[string][]*Message // ключ – hex(senderKey)
	channelMsgs       map[int][]*ChannelMessage
	stats             *CompanionRadioStats // накопленная статистика (все типы)

	// publisher is an optional sink for received/transmitted RF packets
	// (e.g. the MQTT bridge). nil when no publisher is attached.
	publisher PacketPublisher

	// pendingRaw holds raw RF packets from 0x88 frames waiting to be correlated
	// with a decoded 0x11 channel message frame. Both frames arrive sequentially
	// on the same BLE notification stream, so no mutex is needed — only the BLE
	// goroutine reads and writes this slice.
	pendingRaw []pendingRawPacket

	// Каналы для обработки
	notifyCh chan []byte
	doneCh   chan struct{}

	// Коллбэки
	onContact        func(*Contact)
	onChannel        func(*Channel)
	onMessage        func(*Message)
	onChannelMessage func(*ChannelMessage)
	onSelfInfo       func(*SelfInfo)
	onStats          func(*CompanionRadioStats)
	onBattery        func(millivolts int, usedKb, totalKb int)
	onError          func(error)
	onNoMoreMessages func()
	onAdvert         func(*Contact)
	onTraceData      func(*TraceData)
	onChannelData    func(*ChannelDataMessage)
	onControlData    func(*ControlData)

	// serialPort is non-nil when connected via USB/serial instead of BLE.
	serialPort     serial.Port
	serialPortPath string
	serialBaud     int

	// writeMu serializes raw writes — held only for the duration of the write
	// call. Prevents concurrent writes which cause timeouts.
	writeMu sync.Mutex

	// sendMu serializes full send operations (write + confirmation wait).
	// Only one Send*WithRetry can be in flight at a time.
	// The frame handler must never lock sendMu (it delivers confirmations).
	sendMu sync.Mutex

	// для ожидания подтверждения
	sentConfirmCh      chan error // для respCodeSent / respCodeOk
	deliveredConfirmCh chan error // для pushCodeSendConfirmed
	confirmMu          sync.Mutex

	// repeatWatch lets a channel send detect when the mesh has rebroadcast its
	// own packet. While armed, every incoming 0x88 raw frame is hashed and
	// compared to repeatHash; a match (a repeater echoing our message) is
	// signalled on repeatCh so the sender can stop early instead of blindly
	// transmitting all copies.
	repeatHash []byte
	repeatCh   chan struct{}
	repeatMu   sync.Mutex

	// канал входящих сообщений
	incomingCh chan interface{}
	wg         sync.WaitGroup

	// Логгер
	logger  *slog.Logger
	histLog HistoryLogger

	// dedupMu guards seenMsgs, a cache of recently received message keys used to
	// drop protocol-level retransmissions. A MeshCore sender retransmits a DM or
	// channel message (with the same sender timestamp) when it does not receive
	// the ACK; the companion device passes every copy up over BLE, so without
	// this the bot would process and reply to the same message twice.
	dedupMu  sync.Mutex
	seenMsgs map[string]time.Time
}

// dedupTTL is how long a received message key is remembered. It comfortably
// exceeds a sender's retry burst (seconds to tens of seconds) while keeping the
// cache bounded to one window's worth of traffic.
const dedupTTL = 3 * time.Minute

// isDuplicateMessage reports whether key was seen within dedupTTL. On a miss it
// records key with the current time and evicts expired entries. The key should
// combine sender + sender timestamp + text so that genuine retries (all equal)
// are dropped while distinct messages — including a later resend that carries a
// fresh timestamp — are kept. Including the text also guards channel-message
// collisions (channel frames carry no sender prefix) and senders with a wrong
// RTC clock that reuse the same timestamp.
func (c *MeshCoreClient) isDuplicateMessage(key string) bool {
	now := time.Now()
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()

	if seen, ok := c.seenMsgs[key]; ok && now.Sub(seen) < dedupTTL {
		return true
	}

	if c.seenMsgs == nil {
		c.seenMsgs = make(map[string]time.Time)
	}
	for k, t := range c.seenMsgs {
		if now.Sub(t) >= dedupTTL {
			delete(c.seenMsgs, k)
		}
	}
	c.seenMsgs[key] = now
	return false
}

func NewMeshCoreClient(adapter *bluetooth.Adapter) *MeshCoreClient {
	return &MeshCoreClient{
		adapter:     adapter,
		contacts:    make(map[string]*Contact),
		channels:    make(map[int]*Channel),
		messages:    make(map[string][]*Message),
		channelMsgs: make(map[int][]*ChannelMessage),
		notifyCh:    make(chan []byte, 100),
		doneCh:      make(chan struct{}),
		incomingCh:  make(chan interface{}, 100),
		logger:      log(),
	}
}

// SetLogger позволяет установить собственный логгер.
func (c *MeshCoreClient) SetLogger(logger *slog.Logger) {
	c.logger = logger
}

package meshcore

import "time"

// ============================================================================
// Типы данных
// ============================================================================

type Contact struct {
	PublicKey         []byte
	Name              string
	Type              int
	Flags             int
	PathLength        int    // firmware-encoded out_path_len byte: hop count (low 6 bits) + bytes-per-hop (top 2 bits); 0xFF = flood/unknown
	Path              []byte // meaningful path bytes only (hop_count × bytes-per-hop)
	Latitude          *float64
	Longitude         *float64
	LastSeen          time.Time
	LastMessageAt     time.Time
	IsActive          bool
	PathOverride      *int // -1 = flood, nil = auto, >=0 = specific length
	PathOverrideBytes []byte
	LastMod           uint32 // device RTC timestamp of last modification
}

type Channel struct {
	Index       int
	Name        string
	PSK         []byte // поле для ключа канала
	UnreadCount int
	IsEmpty     bool
}

type Message struct {
	SenderKey  []byte
	Text       string
	Timestamp  time.Time
	IsOutgoing bool
	IsCli      bool
	Status     string // "pending", "sent", "delivered", "failed"
	PathLength int
	PathBytes  []byte
	MessageID  string
}

type ChannelMessage struct {
	ChannelIndex int
	SenderName   string
	Text         string
	Timestamp    time.Time
	IsOutgoing   bool
	Status       string
	PathLength   int
	PathBytes    []byte
	MessageID    string
}

// CompanionRadioStats holds all radio statistics from the device.
// Received via CMD_GET_STATS command with STATS_TYPE_CORE, STATS_TYPE_RADIO, STATS_TYPE_PACKETS.
type CompanionRadioStats struct {
	// Core stats (STATS_TYPE_CORE)
	BatteryMv  uint16 // Battery voltage in millivolts
	UptimeSecs uint32 // Device uptime in seconds
	Errors     uint16 // Error flags bitmask
	QueueLen   uint8  // Outbound packet queue length

	// Radio stats (STATS_TYPE_RADIO)
	TxAirSecs     uint32  // Cumulative transmit airtime in seconds
	RxAirSecs     uint32  // Cumulative receive airtime in seconds
	NoiseFloorDbm int8    // Radio noise floor in dBm
	LastRssi      int8    // Last received signal strength in dBm
	LastSnrDb     float32 // Last SNR in dB (divided by 4.0)

	// Packet stats (STATS_TYPE_PACKETS)
	PacketsRecv       uint32 // Total packets received
	PacketsSent       uint32 // Total packets sent
	PacketsFloodTx    uint32 // Packets sent via flood routing
	PacketsDirectTx   uint32 // Packets sent via direct routing
	PacketsFloodRx    uint32 // Packets received via flood routing
	PacketsDirectRx   uint32 // Packets received via direct routing
	PacketsRecvErrors uint32 // Receive/CRC errors (RadioLib)

	// Timestamps when stats were received
	LastUpdate time.Time
}

type SelfInfo struct {
	PublicKey       []byte
	Name            string
	TxPower         int8
	MaxTxPower      int8
	Latitude        float64
	Longitude       float64
	MultiAcks       int
	AdvertLocPolicy int
	TeleModeBase    int
	TeleModeLoc     int
	TeleModeEnv     int
	ManualAdd       bool
	FreqHz          uint32
	BwHz            uint32
	Sf              int
	Cr              int
	ClientRepeat    bool
	FirmwareVersion string
	Model           string
}

// TraceData holds the result of a 0x89 PUSH_CODE_TRACE_DATA frame.
type TraceData struct {
	Tag        uint32
	AuthCode   uint32
	PathHashes []byte    // one byte per hop, node-ID hash
	PathSNRs   []float32 // dB, one entry per hop
	FinalSNR   float32   // SNR on the last leg to this node
}

// ChannelDataMessage holds binary channel data from a 0x1B frame.
type ChannelDataMessage struct {
	ChannelIndex int
	DataType     uint16
	Data         []byte
	SNR          float32
	PathLength   int // 0xFF = flood-routed
}

// ControlData holds a raw binary control payload from a 0x8E frame (firmware v8+).
type ControlData struct {
	SNR     float32
	RSSI    int8
	PathLen int
	Payload []byte
}

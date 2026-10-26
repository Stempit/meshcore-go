package meshcore

// ============================================================================
// Константы протокола
// ============================================================================

const (
	serviceUUID          = "6e400001-b5a3-f393-e0a9-e50e24dcca9e"
	rxCharacteristicUUID = "6e400002-b5a3-f393-e0a9-e50e24dcca9e"
	txCharacteristicUUID = "6e400003-b5a3-f393-e0a9-e50e24dcca9e"
)

// Command codes (to device). The full set the firmware understands
// (companion_radio/MyMesh.cpp CMD_*); some are defined here for future use and
// have no sender helper yet.
const (
	cmdAppStart             = 0x01
	cmdSendTxtMsg           = 0x02
	cmdSendChannelTxtMsg    = 0x03
	cmdGetContacts          = 0x04 // with optional 'since' for efficient sync
	cmdGetDeviceTime        = 0x05 // query device RTC (reply: respCodeCurrTime)
	cmdSetDeviceTime        = 0x06
	cmdSendSelfAdvert       = 0x07
	cmdSetAdvertName        = 0x08
	cmdAddUpdateContact     = 0x09
	cmdSyncNextMessage      = 0x0A
	cmdSetRadioParams       = 0x0B
	cmdSetRadioTxPower      = 0x0C
	cmdResetPath            = 0x0D
	cmdSetAdvertLatLon      = 0x0E
	cmdRemoveContact        = 0x0F
	cmdShareContact         = 0x10
	cmdExportContact        = 0x11 // reply: respCodeExportContact
	cmdImportContact        = 0x12
	cmdReboot               = 0x13
	cmdGetBattAndStorage    = 0x14 // was CMD_GET_BATTERY_VOLTAGE
	cmdSetTuningParams      = 0x15
	cmdDeviceQuery          = 0x16
	cmdExportPrivateKey     = 0x17 // reply: respCodePrivateKey
	cmdImportPrivateKey     = 0x18
	cmdSendRawData          = 0x19
	cmdSendLogin            = 0x1A // reply: pushCodeLoginSuccess/Fail
	cmdSendStatusReq        = 0x1B // reply: pushCodeStatusResponse
	cmdHasConnection        = 0x1C
	cmdLogout               = 0x1D // 'Disconnect'
	cmdGetContactByKey      = 0x1E
	cmdGetChannel           = 0x1F
	cmdSetChannel           = 0x20
	cmdSignStart            = 0x21 // reply: respCodeSignStart
	cmdSignData             = 0x22 // reply: respCodeSignature
	cmdSignFinish           = 0x23
	cmdSendTracePath        = 0x24 // reply: pushCodeTraceData
	cmdSetDevicePin         = 0x25
	cmdSetOtherParams       = 0x26
	cmdSendTelemetryReq     = 0x27 // reply: pushCodeTelemetryResponse
	cmdGetCustomVar         = 0x28 // CMD_GET_CUSTOM_VARS
	cmdSetCustomVar         = 0x29
	cmdGetAdvertPath        = 0x2A // reply: respCodeAdvertPath
	cmdGetTuningParams      = 0x2B // reply: respCodeTuningParams
	cmdSendBinaryReq        = 0x32 // reply: pushCodeBinaryResponse
	cmdFactoryReset         = 0x33
	cmdSendPathDiscoveryReq = 0x34 // reply: pushCodePathDiscoveryResponse
	cmdSetFloodScopeKey     = 0x36 // v8+
	cmdSendControlData      = 0x37 // v8+, reply: pushCodeControlData
	cmdGetStats             = 0x38 // v8+, second byte is stats type
	cmdSendAnonReq          = 0x39
	cmdSetAutoAddConfig     = 0x3A
	cmdGetAutoAddConfig     = 0x3B
	cmdGetAllowedRepeatFreq = 0x3C
	cmdSetPathHashMode      = 0x3D
	cmdSendChannelData      = 0x3E // reply: respCodeChannelDataRecv
	cmdSetDefaultFloodScope = 0x3F
	cmdGetDefaultFloodScope = 0x40 // reply: respCodeDefaultFloodScope
	cmdSendRawPacket        = 0x41
)

// Response codes (from device)
const (
	respCodeOk                = 0x00
	respCodeErr               = 0x01
	respCodeContactsStart     = 0x02
	respCodeContact           = 0x03
	respCodeEndOfContacts     = 0x04
	respCodeSelfInfo          = 0x05
	respCodeSent              = 0x06
	respCodeContactMsgRecv    = 0x07
	respCodeChannelMsgRecv    = 0x08
	respCodeCurrTime          = 0x09 // reply to CMD_GET_DEVICE_TIME
	respCodeNoMoreMessages    = 0x0A
	respCodeExportContact     = 0x0B // reply to CMD_EXPORT_CONTACT
	respCodeBattAndStorage    = 0x0C
	respCodeDeviceInfo        = 0x0D
	respCodePrivateKey        = 0x0E // reply to CMD_EXPORT_PRIVATE_KEY
	respCodeDisabled          = 0x0F // generic "feature disabled"
	respCodeContactMsgRecvV3  = 0x10
	respCodeChannelMsgRecvV3  = 0x11
	respCodeChannelInfo       = 0x12
	respCodeSignStart         = 0x13 // signing flow (CMD_SIGN_DATA)
	respCodeSignature         = 0x14 // signing flow (64-byte Ed25519 signature)
	respCodeCustomVars        = 0x15
	respCodeAdvertPath        = 0x16 // reply to CMD_GET_ADVERT_PATH
	respCodeTuningParams      = 0x17 // reply to CMD_GET_TUNING_PARAMS
	respCodeStats             = 0x18
	respCodeAutoAddConfig     = 0x19
	respCodeChannelDataRecv   = 0x1B // binary channel data packet
	respCodeDefaultFloodScope = 0x1C // reply to CMD_GET_DEFAULT_FLOOD_SCOPE
)

// Push codes (async from device)
const (
	pushCodeAdvert                = 0x80
	pushCodePathUpdated           = 0x81
	pushCodeSendConfirmed         = 0x82
	pushCodeMsgWaiting            = 0x83
	pushCodeRawData               = 0x84 // raw (non-message) RF data packet
	pushCodeLoginSuccess          = 0x85 // login to room server accepted
	pushCodeLoginFail             = 0x86 // login to room server rejected
	pushCodeStatusResponse        = 0x87 // reply to a status query
	pushCodeLogRxData             = 0x88
	pushCodeTraceData             = 0x89 // trace echo received; path + per-hop SNR
	pushCodeNewAdvert             = 0x8A // new contact discovered; same frame format as respCodeContact
	pushCodeTelemetryResponse     = 0x8B // contact responded to telemetry request
	pushCodeBinaryResponse        = 0x8C // contact responded to binary request
	pushCodePathDiscoveryResponse = 0x8D // path discovery to a contact completed
	pushCodeControlData           = 0x8E // v8+: raw binary control payload from another node
	pushCodeContactDeleted        = 0x8F // contact was deleted/overwritten; frame: [code][pubkey(32)]
	pushCodeContactsFull          = 0x90 // device contact storage full; bot must evict oldest
)

// Text message types
const (
	txtTypePlain   = 0x00
	txtTypeCliData = 0x01
	txtTypeSigned  = 0x02
)

// Contact/advertisement types
const (
	advTypeChat     = 1
	advTypeRepeater = 2
	advTypeRoom     = 3
	advTypeSensor   = 4
)

// Contact flags
const (
	contactFlagFavorite = 0x01
	contactFlagTeleBase = 0x02
	contactFlagTeleLoc  = 0x04
	contactFlagTeleEnv  = 0x08
)

// Auto-add flags
const (
	autoAddOverwriteOldest = 1 << 0
	autoAddChat            = 1 << 1
	autoAddRepeater        = 1 << 2
	autoAddRoomServer      = 1 << 3
	autoAddSensor          = 1 << 4
)

// Telemetry modes
const (
	teleModeDeny       = 0
	teleModeAllowFlags = 1
	teleModeAllowAll   = 2
)

// Размеры
const (
	pubKeySize         = 32
	signatureSize      = 64
	maxPathSize        = 64
	maxNameSize        = 32
	maxFrameSize       = 172
	appProtocolVersion = 3
	contactFrameSize   = 148 // Размер фрейма контакта
)

// Stats types for CMD_GET_STATS
const (
	STATS_TYPE_CORE    = 0
	STATS_TYPE_RADIO   = 1
	STATS_TYPE_PACKETS = 2
)

// pathLengthFlood is OUT_PATH_UNKNOWN (0xFF): device will flood-route the message.
// A contact at this value has no known directed path.
const pathLengthFlood = 0xFF

// Error codes (byte 1 of RESP_CODE_ERR frames, from MyMesh.cpp ERR_CODE_*)
const (
	errCodeUnsupportedCmd = 0x01
	errCodeNotFound       = 0x02
	errCodeTableFull      = 0x03
	errCodeBadState       = 0x04
	errCodeFileIOError    = 0x05
	errCodeIllegalArg     = 0x06
)

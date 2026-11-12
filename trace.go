package meshcore

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Trace command constants. CMD_SEND_TRACE_PATH sends a PAYLOAD_TYPE_TRACE
// packet along an explicit path of repeater pubkey-prefix hashes (MeshCore
// v1.11+ trace feature); the trace RESULT is not a reply to this command — it
// arrives later as an asynchronous PUSH_CODE_TRACE_DATA (0x89) frame, delivered
// through OnTraceData. The only synchronous reply is RESP_CODE_SENT (the
// device accepted the command and queued the radio transmission).
const (
	// maxTracePathEntries mirrors the firmware-side usable maximum: the
	// completion guard in Mesh::onRecvPacket drops TRACE packets whose path_len
	// (SNR count) reaches MAX_PATH_SIZE (64), so a 64-entry trace can never
	// complete. Keep one entry of headroom.
	maxTracePathEntries = 63
	// maxTracePathBytes mirrors the firmware frame-length check
	// (len - 10 < MAX_PACKET_PAYLOAD - 5) for the hash bytes after the header.
	maxTracePathBytes = 178
	// traceSentReplyLen is the RESP_CODE_SENT frame: code, reserved, tag(4), est_timeout(4).
	traceSentReplyLen = 10
)

// TraceFlagsForHashSize maps a path-hash entry size in bytes to the flags byte
// for CMD_SEND_TRACE_PATH (entry size = 1 << (flags & 0x03)). Supported sizes:
// 1, 2, 4 and 8. Note the trace payload's own encoding differs from the
// packet-header path_len byte (which caps the entry size at 4).
func TraceFlagsForHashSize(hashSize int) (byte, error) {
	switch hashSize {
	case 1:
		return 0, nil
	case 2:
		return 1, nil
	case 4:
		return 2, nil
	case 8:
		return 3, nil
	default:
		return 0, fmt.Errorf("unsupported trace hash size %d (must be 1, 2, 4 or 8)", hashSize)
	}
}

// SendTracePath sends a trace-route command (CMD_SEND_TRACE_PATH) to the
// device: the device transmits a direct-routed TRACE packet that walks the
// given path of repeater pubkey-prefix hashes in order (revisits allowed —
// a path may return through the same repeaters). Each traversed repeater
// appends its inbound SNR; whoever hears the FINAL entry's retransmission
// receives the result, so paths must start at a repeater that hears the device
// and end at one the device can hear (typically back at the first entry).
//
// The synchronous return value is the device's own estimate of how long the
// forward trip takes (RESP_CODE_SENT's est_timeout, 500 + (airtime*6 + 250)
// × (hops+1) ms) — a sensible basis for the caller's result timeout. The trace
// result itself arrives asynchronously as a PUSH_CODE_TRACE_DATA (0x89) frame
// carrying the same tag; correlate via OnTraceData.
//
// flags encodes the hash entry size (see TraceFlagsForHashSize); path must be
// a whole number of entries, at most 63 entries and 178 bytes. auth is opaque
// to the firmware and relayed verbatim in the result.
//
// Serialization: sendMu is held for the duration, so no message send can be
// in flight while the respCodeSent waiter is armed (their confirmation slots
// are code-matched, and a stolen RESP_CODE_SENT would hang a text send).
func (c *MeshCoreClient) SendTracePath(tag, auth uint32, flags byte, path []byte) (time.Duration, error) {
	entrySize := 1 << (flags & 0x03)
	if len(path) == 0 {
		return 0, fmt.Errorf("trace path is empty")
	}
	if len(path)%entrySize != 0 {
		return 0, fmt.Errorf("trace path length %d is not a multiple of the %d-byte entry size", len(path), entrySize)
	}
	if entries := len(path) / entrySize; entries > maxTracePathEntries {
		return 0, fmt.Errorf("trace path too long: %d entries (max %d — a 64-entry trace cannot complete)", entries, maxTracePathEntries)
	}
	if len(path) > maxTracePathBytes {
		return 0, fmt.Errorf("trace path too long: %d bytes (max %d)", len(path), maxTracePathBytes)
	}

	frame := make([]byte, 10+len(path))
	frame[0] = cmdSendTracePath
	binary.LittleEndian.PutUint32(frame[1:5], tag)
	binary.LittleEndian.PutUint32(frame[5:9], auth)
	frame[9] = flags
	copy(frame[10:], path)

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	data, err := c.runCommand(frame, respCodeSent, defaultCommandTimeout)
	if err != nil {
		return 0, err
	}

	// RESP_CODE_SENT frame: [0x06][0][tag:4 LE][est_timeout:4 LE] (MyMesh.cpp).
	if len(data) < traceSentReplyLen {
		return 0, fmt.Errorf("short SENT reply (%d bytes, want %d)", len(data), traceSentReplyLen)
	}
	replyTag := binary.LittleEndian.Uint32(data[2:6])
	if replyTag != tag {
		return 0, fmt.Errorf("SENT reply tag 0x%08x does not match the sent tag 0x%08x", replyTag, tag)
	}
	est := time.Duration(binary.LittleEndian.Uint32(data[6:10])) * time.Millisecond
	return est, nil
}

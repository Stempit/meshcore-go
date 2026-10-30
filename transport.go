package meshcore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"go.bug.st/serial"
)

// Connection is one transport carrying MeshCore companion protocol frames.
// A frame is a raw protocol message body whose first byte is the command or
// response code; stream transports delimit frames with a length prefix while
// BLE delivers one frame per notification.
type Connection interface {
	// WriteFrame sends one frame payload to the device.
	WriteFrame(payload []byte) error
	// Close shuts the transport down synchronously: any read goroutine the
	// transport runs has exited when Close returns. Safe to call more than
	// once and concurrently with WriteFrame.
	Close() error
}

// maxStreamFrameLen is the sanity bound on a device→host frame length in the
// stream framing. The protocol payload is ≤ 176 bytes (maxFrameSize + transport
// overhead); anything above 512 means the parser has lost synchronisation.
const maxStreamFrameLen = 512

// maxWriteFrameLen is the firmware's MAX_FRAME_SIZE (BaseSerialInterface.h):
// the device rejects larger host→device frames, so fail fast client-side
// instead of losing the command silently.
const maxWriteFrameLen = 176

// ============================================================================
// Stream transport (USB serial and TCP)
// ============================================================================

// streamTransport implements Connection over any byte stream. USB serial and
// TCP share the same framing (the firmware's SerialWifiInterface deliberately
// reuses the serial header): '<' + len_LSB + len_MSB + payload (host→device),
// '>' + len_LSB + len_MSB + payload (device→host).
type streamTransport struct {
	stream io.ReadWriteCloser

	onFrame func([]byte) // one complete device→host frame
	onLost  func(error)  // read loop died unexpectedly (not via Close)
	logger  *slog.Logger

	writeMu   sync.Mutex
	stopCh    chan struct{}
	doneCh    chan struct{} // closed when the read loop has exited
	closeOnce sync.Once
}

// newStreamTransport wraps stream and starts its read loop. onFrame and
// onLost are invoked from the read loop goroutine and must not block.
func newStreamTransport(stream io.ReadWriteCloser, onFrame func([]byte), onLost func(error), logger *slog.Logger) *streamTransport {
	t := &streamTransport{
		stream:  stream,
		onFrame: onFrame,
		onLost:  onLost,
		logger:  logger,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go t.readLoop()
	return t
}

// WriteFrame wraps payload in the MeshCore stream frame format:
// '<' + len_LSB + len_MSB + payload.
func (t *streamTransport) WriteFrame(payload []byte) error {
	if len(payload) > maxWriteFrameLen {
		return fmt.Errorf("frame payload %d bytes exceeds device maximum %d", len(payload), maxWriteFrameLen)
	}
	frame := make([]byte, 3+len(payload))
	frame[0] = '<'
	binary.LittleEndian.PutUint16(frame[1:3], uint16(len(payload)))
	copy(frame[3:], payload)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := t.stream.Write(frame)
	return err
}

// Close closes the underlying stream and waits for the read loop to exit.
func (t *streamTransport) Close() error {
	t.closeOnce.Do(func() { close(t.stopCh) })
	err := t.stream.Close()
	<-t.doneCh
	return err
}

// readLoop parses '>'-framed packets from the stream and feeds complete frames
// into onFrame, byte by byte to survive arbitrary stream segmentation.
func (t *streamTransport) readLoop() {
	defer close(t.doneCh)

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
		case <-t.stopCh:
			return
		default:
		}

		n, err := t.stream.Read(oneByte)
		if err != nil {
			select {
			case <-t.stopCh:
				return // closed deliberately; Close() already reported the error
			default:
			}
			if errors.Is(err, io.EOF) {
				t.logger.Info("Stream closed by device")
			} else {
				t.logger.Error("Stream read error", "error", err)
			}
			t.onLost(err)
			return
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
			if frameLen == 0 || frameLen > maxStreamFrameLen {
				t.logger.Warn("Stream frame: invalid length, resetting", "len", frameLen)
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
				t.onFrame(frameCopy)
				state = stateIdle
			}
		}
	}
}

// ============================================================================
// Transport dialers
// ============================================================================

// dialSerial opens an OS serial port. baud should be 115200 for standard
// MeshCore firmware.
func dialSerial(portPath string, baud int) (io.ReadWriteCloser, error) {
	port, err := serial.Open(portPath, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("open serial port %s: %w", portPath, err)
	}
	return port, nil
}

// dialTCP connects to a MeshCore companion device's TCP server (port 5000 on
// stock WiFi/Ethernet companion firmware). Keepalive notices a device that
// disappeared (reboot, power loss) instead of blocking forever.
func dialTCP(addr string) (io.ReadWriteCloser, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", addr, err)
	}
	return conn, nil
}

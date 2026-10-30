package meshcore

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeDevice emulates the MeshCore companion TCP server: it accepts clients,
// parses '<'-framed commands and answers the handshake commands the way the
// real firmware does (companion_radio/MyMesh.cpp handleCmdFrame). A WiFi or
// Ethernet companion device serves exactly this on TCP port 5000.
type fakeDevice struct {
	listener net.Listener

	mu   sync.Mutex
	conn net.Conn

	// responders scripts per-command replies for the send-retry tests; when a
	// command has a responder its return value is written instead of the
	// default handshake answers.
	responders map[byte]*scriptResponder

	cmds chan []byte
}

// scriptResponder answers the nth occurrence of one command code.
type scriptResponder struct {
	n       int
	respond func(nth int, payload []byte) [][]byte
}

// setResponder installs a custom response script for one command code. The
// default handshake behavior remains for every other command. respond runs on
// the device's read goroutine and must not block.
func (d *fakeDevice) setResponder(cmd byte, respond func(nth int, payload []byte) [][]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.responders == nil {
		d.responders = make(map[byte]*scriptResponder)
	}
	d.responders[cmd] = &scriptResponder{respond: respond}
}

func startFakeDevice(t *testing.T) (*fakeDevice, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeDevice{listener: ln, cmds: make(chan []byte, 16)}
	go d.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return d, ln.Addr().String()
}

func (d *fakeDevice) serve() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conn = conn
		d.mu.Unlock()
		go d.handle(conn)
	}
}

// handle reads '<'-framed commands and answers the handshake commands.
func (d *fakeDevice) handle(conn net.Conn) {
	defer conn.Close()
	hdr := make([]byte, 3)
	payload := make([]byte, maxStreamFrameLen)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		if hdr[0] != '<' {
			// Skip until a valid marker. (The firmware instead skips exactly
			// frame_length bytes; the fake only sees well-formed clients.)
			continue
		}
		n := int(binary.LittleEndian.Uint16(hdr[1:3]))
		if n == 0 || n > maxStreamFrameLen {
			continue
		}
		if _, err := io.ReadFull(conn, payload[:n]); err != nil {
			return
		}
		cmd := payload[0]
		select {
		case d.cmds <- append([]byte(nil), payload[:n]...):
		default:
		}
		if frames := d.scriptedReply(cmd, payload[:n]); frames != nil {
			for _, f := range frames {
				_ = d.writeFrame(conn, f)
			}
			continue
		}
		switch cmd {
		case cmdAppStart:
			_ = d.writeFrame(conn, []byte{respCodeOk})
		case cmdDeviceQuery:
			// [code][fw_version]; 14 is the current FIRMWARE_VER_CODE.
			_ = d.writeFrame(conn, []byte{respCodeDeviceInfo, 14})
		case cmdSetDeviceTime:
			_ = d.writeFrame(conn, []byte{respCodeOk})
		}
	}
}

func (d *fakeDevice) writeFrame(conn net.Conn, payload []byte) error {
	frame := make([]byte, 3+len(payload))
	frame[0] = '>'
	binary.LittleEndian.PutUint16(frame[1:3], uint16(len(payload)))
	copy(frame[3:], payload)
	_, err := conn.Write(frame)
	return err
}

// scriptedReply returns the scripted frames for cmd, or nil to fall through to
// the default handshake answers.
func (d *fakeDevice) scriptedReply(cmd byte, payload []byte) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.responders[cmd]
	if r == nil {
		return nil
	}
	r.n++
	return r.respond(r.n, payload)
}

// closeConn simulates the device disappearing (reboot, power loss).
func (d *fakeDevice) closeConn() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
}

func waitUntil(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// flood writes n device frames as fast as the socket accepts them. Full-size
// DeviceInfo frames on purpose: short ones return from parseDeviceInfo before
// its c.mu.Lock(), and the point is to keep the frame handler holding/awaiting
// the client mutex while teardown runs concurrently.
func (d *fakeDevice) flood(n int) {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return
	}
	payload := make([]byte, 80) // parseDeviceInfo takes c.mu when len >= 80
	payload[0] = respCodeDeviceInfo
	payload[1] = 14
	frame := make([]byte, 3+len(payload))
	frame[0] = '>'
	binary.LittleEndian.PutUint16(frame[1:3], uint16(len(payload)))
	copy(frame[3:], payload)
	for i := 0; i < n; i++ {
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

// TestDisconnectDuringFrameProcessing pins the exact deadlock the teardown
// once had: the frame handler parked inside handleFrame while waiting for the
// client mutex, and Disconnect waiting for the frame handler while holding
// that same mutex. Holding client.mu from the test parks the handler inside
// parseDeviceInfo (flooded DeviceInfo frames need the mutex to finish), so
// the interleaving is deterministic instead of a race with the flood.
func TestDisconnectDuringFrameProcessing(t *testing.T) {
	dev, addr := startFakeDevice(t)

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}

	// Park the frame handler inside handleFrame: hold the client mutex and
	// let flooded DeviceInfo frames drive parseDeviceInfo into its Lock.
	client.mu.Lock()
	go dev.flood(800)
	time.Sleep(100 * time.Millisecond) // let the handler enter parseDeviceInfo

	done := make(chan error, 1)
	go func() { done <- client.Disconnect() }()
	time.Sleep(50 * time.Millisecond) // let Disconnect queue on the mutex

	client.mu.Unlock() // now the deadlock (if any) unfolds

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Disconnect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect deadlocked while a frame was being processed")
	}
}

func TestConnectTCPHandshake(t *testing.T) {
	dev, addr := startFakeDevice(t)

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	if !client.IsConnected() {
		t.Fatal("client not connected after ConnectTCP")
	}

	// The first command must be APP_START, wrapped in the stream framing.
	select {
	case cmd := <-dev.cmds:
		if cmd[0] != cmdAppStart {
			t.Fatalf("first command = %#02x, want APP_START", cmd[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for APP_START")
	}

	if err := client.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if client.IsConnected() {
		t.Fatal("client still connected after Disconnect")
	}
}

func TestConnectTCPDeviceGoneAndReconnect(t *testing.T) {
	dev, addr := startFakeDevice(t)

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}

	// The device disappears: the client must notice via its read loop and
	// drop to disconnected without any explicit Disconnect call.
	dev.closeConn()
	waitUntil(t, func() bool { return !client.IsConnected() },
		5*time.Second, "client still connected after device closed the connection")

	// Drop the first handshake's commands so the reconnect assertion below
	// reads freshly received frames, not a stale APP_START.
drainCmds:
	for {
		select {
		case <-dev.cmds:
		default:
			break drainCmds
		}
	}

	// Reconnect must redial the same address and redo the handshake.
	if err := client.Reconnect(); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if !client.IsConnected() {
		t.Fatal("client not connected after Reconnect")
	}
	select {
	case cmd := <-dev.cmds:
		if cmd[0] != cmdAppStart {
			t.Fatalf("reconnect first command = %#02x, want APP_START", cmd[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for APP_START after reconnect")
	}

	if err := client.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

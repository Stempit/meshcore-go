package meshcore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCLICommandFrame pins the CMD_RUN_CLI_COMMAND wire format: the ASCII
// command text directly after the command byte, no prefix, no terminator.
func TestCLICommandFrame(t *testing.T) {
	frame := cliCommandFrame("get tx")
	want := []byte{cmdRunCliCommand, 'g', 'e', 't', ' ', 't', 'x'}
	if string(frame) != string(want) {
		t.Fatalf("frame = %x, want %x", frame, want)
	}
}

// TestRunCLICommand covers the happy path: the device answers the CLI command
// with RESP_CODE_CLI_REPLY and the reply text comes back trimmed.
func TestRunCLICommand(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdRunCliCommand, func(_ int, payload []byte) [][]byte {
		if string(payload[1:]) != "get tx" {
			t.Errorf("device got command %q, want %q", string(payload[1:]), "get tx")
		}
		return [][]byte{append([]byte{respCodeCliReply}, "> 869.618"...)}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	reply, err := client.RunCLICommand("get tx")
	if err != nil {
		t.Fatalf("RunCLICommand: %v", err)
	}
	if reply != "> 869.618" {
		t.Fatalf("reply = %q, want %q", reply, "> 869.618")
	}
}

// TestRunCLICommandErrReply pins the DeviceError contract: a RESP_CODE_ERR
// reply (e.g. unsupported_cmd on firmware older than v14) surfaces as a typed
// error carrying the device error code.
func TestRunCLICommandErrReply(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdRunCliCommand, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeUnsupportedCmd}}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	_, err := client.RunCLICommand("ver")
	var devErr *DeviceError
	if !errors.As(err, &devErr) {
		t.Fatalf("want *DeviceError, got %v", err)
	}
	if devErr.Code != errCodeUnsupportedCmd {
		t.Fatalf("code = %d, want %d", devErr.Code, errCodeUnsupportedCmd)
	}
	if !strings.Contains(devErr.Error(), "unsupported_cmd") {
		t.Fatalf("error text %q lacks the code name", devErr.Error())
	}
}

// TestRunCLICommandNoResponse covers the silent-device path: no CLI_REPLY
// within the window means ErrNoResponse.
func TestRunCLICommandNoResponse(t *testing.T) {
	_, addr := startFakeDevice(t) // silent for cmdRunCliCommand

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	_, err := client.runCommand(cliCommandFrame("ver"), respCodeCliReply, 100*time.Millisecond)
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
}

// TestRunCommandAbortsOnDisconnect pins the disconnect path: a connection
// lost mid-wait aborts the command via handlersDone instead of burning the
// whole timeout.
func TestRunCommandAbortsOnDisconnect(t *testing.T) {
	dev, addr := startFakeDevice(t) // silent for cmdRunCliCommand

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	done := make(chan error, 1)
	go func() {
		_, err := client.runCommand(cliCommandFrame("ver"), respCodeCliReply, 10*time.Second)
		done <- err
	}()

	// Wait until the device actually received the command, then drop the link.
	select {
	case <-dev.cmds:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the CLI command to reach the device")
	}
	dev.closeConn()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNoResponse) {
			t.Fatalf("want ErrNoResponse, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCommand did not abort on disconnect")
	}
}

// TestRunCLICommandInputGuards pins the client-side validation: empty, short
// and oversized commands are rejected before touching the device — asserted
// against a CONNECTED client, since on an unconnected one any write fails
// anyway and the guards could silently regress.
func TestRunCLICommandInputGuards(t *testing.T) {
	dev, addr := startFakeDevice(t)
	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	// Wait for all three handshake commands (APP_START, DEVICE_QUERY,
	// SET_DEVICE_TIME) to reach the device, then drain the channel: the
	// handshake returns before the device's read goroutine has necessarily
	// copied every command into cmds, and the traffic assertion below must
	// read only what RunCLICommand tried to send.
	seen := make(map[byte]bool)
	waitUntil(t, func() bool {
		for {
			select {
			case cmd := <-dev.cmds:
				seen[cmd[0]] = true
			default:
				return len(seen) >= 3
			}
		}
	}, 5*time.Second, "timeout waiting for the handshake commands")

	for _, cmd := range []string{"   ", "x", strings.Repeat("a", maxCliCommandLen+1)} {
		if _, err := client.RunCLICommand(cmd); err == nil {
			t.Fatalf("command %q accepted", cmd)
		}
	}

	// No guard-rejected command may have reached the device.
	select {
	case cmd := <-dev.cmds:
		t.Fatalf("guard-rejected command reached the device: %q", string(cmd[1:]))
	default:
	}
}

// TestRunCommandClearsWaiterAfterCompletion pins waiter hygiene (found by a
// surviving mutation: no other test fails if the cleanup is dropped): after a
// command finishes — reply or timeout — its registration is gone, so a later
// stray frame with that code falls through to normal dispatch.
func TestRunCommandClearsWaiterAfterCompletion(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdRunCliCommand, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeCliReply}}
	})
	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	// Reply path: the registration is cleaned up on success.
	if _, err := client.RunCLICommand("ver"); err != nil {
		t.Fatalf("RunCLICommand: %v", err)
	}
	if client.deliverResp(respCodeCliReply, []byte{respCodeCliReply}) {
		t.Fatal("waiter still registered after a successful command")
	}

	// Timeout path: the registration is cleaned up too.
	dev.setResponder(cmdRunCliCommand, func(_ int, _ []byte) [][]byte { return nil })
	if _, err := client.runCommand(cliCommandFrame("ver"), respCodeCliReply, 50*time.Millisecond); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
	if client.deliverResp(respCodeCliReply, []byte{respCodeCliReply}) {
		t.Fatal("waiter still registered after a timed-out command")
	}
}

// TestWaiterDoesNotInterceptOk pins the dispatch-order invariant that keeps
// message sends working: a waiter armed for a command's reply code must not
// swallow respCodeOk frames — those belong to the send confirmation slots.
func TestWaiterDoesNotInterceptOk(t *testing.T) {
	c := NewMeshCoreClient(nil)
	waiter := make(chan []byte, 1)
	c.armResp(respCodeCliReply, waiter)

	if c.deliverResp(respCodeOk, []byte{respCodeOk}) {
		t.Fatal("respCodeOk intercepted by a waiter armed for a different code")
	}
	// respCodeErr IS intercepted by design: the device answers a failed
	// command with RESP_CODE_ERR instead of the expected reply.
	if !c.deliverResp(respCodeErr, []byte{respCodeErr, errCodeIllegalArg}) {
		t.Fatal("respCodeErr not delivered to the armed waiter")
	}
}

// TestRunCommandSerializesConcurrentCommands pins the cmdMu contract: two
// concurrent commands each get their own reply. Without cmdMu both waiters
// would be registered under the same response code, the second registration
// would overwrite the first, and one caller would lose its reply and time
// out. The responder's sleep widens the overlap window deterministically.
func TestRunCommandSerializesConcurrentCommands(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdRunCliCommand, func(nth int, _ []byte) [][]byte {
		// Runs on the device's read goroutine; nothing else is in flight, so
		// a short sleep only delays the next command's reply on purpose.
		time.Sleep(150 * time.Millisecond)
		return [][]byte{append([]byte{respCodeCliReply}, fmt.Sprintf("reply-%d", nth)...)}
	})
	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	type result struct {
		reply string
		err   error
	}
	done := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			reply, err := client.RunCLICommand("ver")
			done <- result{reply, err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("command %d failed: %v", i, r.err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("timeout waiting for concurrent command %d", i)
		}
	}
}

// TestDeliverRespDoesNotBlockOnFullChannel pins the non-blocking delivery
// rule: the frame handler must never park on a waiter that already gave up.
func TestDeliverRespDoesNotBlockOnFullChannel(t *testing.T) {
	c := NewMeshCoreClient(nil)
	ch := make(chan []byte, 1)
	ch <- []byte{respCodeCliReply} // buffer full: the waiter never consumed it
	c.armResp(respCodeCliReply, ch)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.deliverResp(respCodeCliReply, []byte{respCodeOk})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deliverResp blocked on a full waiter channel")
	}

	// The registration must still be cleanable afterwards.
	c.clearResp(respCodeCliReply, ch)
	if c.deliverResp(respCodeCliReply, []byte{respCodeOk}) {
		t.Fatal("waiter still registered after clearResp")
	}
}

// TestGetDeviceTime covers the happy path: the device answers with
// RESP_CODE_CURR_TIME carrying four little-endian unix seconds, returned as a
// UTC time.
func TestGetDeviceTime(t *testing.T) {
	dev, addr := startFakeDevice(t)
	const wantUnix = uint32(1762612345)
	dev.setResponder(cmdGetDeviceTime, func(_ int, payload []byte) [][]byte {
		if len(payload) != 1 || payload[0] != cmdGetDeviceTime {
			t.Errorf("device got frame %x, want the bare cmdGetDeviceTime byte", payload)
		}
		ts := make([]byte, 4)
		binary.LittleEndian.PutUint32(ts, wantUnix)
		return [][]byte{append([]byte{respCodeCurrTime}, ts...)}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	got, err := client.GetDeviceTime()
	if err != nil {
		t.Fatalf("GetDeviceTime: %v", err)
	}
	if want := time.Unix(int64(wantUnix), 0).UTC(); !got.Equal(want) {
		t.Fatalf("time = %v, want %v", got, want)
	}
	// Equal compares instants and cannot see a dropped .UTC() — pin the
	// location itself, the gateway's device_time JSON shape depends on it.
	if got.Location() != time.UTC {
		t.Fatalf("time location = %v, want UTC", got.Location())
	}
}

// TestGetDeviceTimeShortReply pins the malformed-reply path: a CURR_TIME
// shorter than code + 4 bytes is an error, not a zero time.
func TestGetDeviceTimeShortReply(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdGetDeviceTime, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeCurrTime, 0x01, 0x02}}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	if _, err := client.GetDeviceTime(); err == nil {
		t.Fatal("short CURR_TIME reply accepted")
	}
}

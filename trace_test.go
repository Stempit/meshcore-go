package meshcore

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestTraceFlagsForHashSize pins the flags mapping: entry size = 1 << (flags&3),
// so 1/2/4/8 bytes map to flags 0/1/2/3, and anything else is rejected.
func TestTraceFlagsForHashSize(t *testing.T) {
	for size, want := range map[int]byte{1: 0, 2: 1, 4: 2, 8: 3} {
		got, err := TraceFlagsForHashSize(size)
		if err != nil {
			t.Fatalf("size %d: unexpected error %v", size, err)
		}
		if got != want {
			t.Fatalf("size %d: flags = %d, want %d", size, got, want)
		}
	}
	for _, size := range []int{0, 3, 5, 6, 7, 16, -1} {
		if _, err := TraceFlagsForHashSize(size); err == nil {
			t.Fatalf("size %d accepted", size)
		}
	}
}

// TestSendTracePath pins the CMD_SEND_TRACE_PATH wire format and the
// RESP_CODE_SENT parsing: the frame is [0x24][tag LE4][auth LE4][flags][path],
// and the reply carries the same tag plus the firmware's est_timeout.
func TestSendTracePath(t *testing.T) {
	dev, addr := startFakeDevice(t)

	const (
		tag  = uint32(0x11223344)
		auth = uint32(0xCAFEBABE)
		est  = uint32(3000) // ms
	)
	path := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF} // 3 two-byte entries
	dev.setResponder(cmdSendTracePath, func(_ int, payload []byte) [][]byte {
		want := append([]byte{cmdSendTracePath, 0x44, 0x33, 0x22, 0x11, 0xBE, 0xBA, 0xFE, 0xCA, 0x01}, path...)
		if string(payload) != string(want) {
			t.Errorf("device got frame %x, want %x", payload, want)
		}
		reply := make([]byte, traceSentReplyLen)
		reply[0] = respCodeSent
		binary.LittleEndian.PutUint32(reply[2:6], tag)
		binary.LittleEndian.PutUint32(reply[6:10], est)
		return [][]byte{reply}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	got, err := client.SendTracePath(tag, auth, 0x01, path)
	if err != nil {
		t.Fatalf("SendTracePath: %v", err)
	}
	if got != 3*time.Second {
		t.Fatalf("est_timeout = %v, want 3s", got)
	}
}

// TestSendTracePathErrReply pins the DeviceError contract for traces: the
// device can reject the command (e.g. ERR_CODE_ILLEGAL_ARG for a misaligned
// path, ERR_CODE_TABLE_FULL when the packet pool is exhausted).
func TestSendTracePathErrReply(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSendTracePath, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeIllegalArg}}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	_, err := client.SendTracePath(1, 2, 0x00, []byte{0x42})
	var devErr *DeviceError
	if !errors.As(err, &devErr) {
		t.Fatalf("want *DeviceError, got %v", err)
	}
	if devErr.Code != errCodeIllegalArg {
		t.Fatalf("code = %d, want %d", devErr.Code, errCodeIllegalArg)
	}
}

// TestSendTracePathTagMismatch pins the reply sanity check: a RESP_CODE_SENT
// frame whose tag differs from the sent one (a stale or misrouted frame) must
// surface as an error, not as a success.
func TestSendTracePathTagMismatch(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSendTracePath, func(_ int, _ []byte) [][]byte {
		reply := make([]byte, traceSentReplyLen)
		reply[0] = respCodeSent
		binary.LittleEndian.PutUint32(reply[2:6], 0xDEAD)
		binary.LittleEndian.PutUint32(reply[6:10], 1000)
		return [][]byte{reply}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	_, err := client.SendTracePath(0x11223344, 0, 0x00, []byte{0x42})
	if err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("want tag-mismatch error, got %v", err)
	}
}

// TestSendTracePathShortReply pins the malformed-reply path: a SENT frame
// shorter than code + reserved + tag + est_timeout is an error.
func TestSendTracePathShortReply(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSendTracePath, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeSent, 0, 0x01, 0x02}}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	if _, err := client.SendTracePath(1, 0, 0x00, []byte{0x42}); err == nil {
		t.Fatal("short SENT reply accepted")
	}
}

// TestSendTracePathInputGuards pins the client-side validation: empty paths,
// paths not a multiple of the entry size, over-long paths (>63 entries or
// >178 bytes) are rejected before touching the device — asserted against a
// CONNECTED client, since on an unconnected one any write fails anyway.
func TestSendTracePathInputGuards(t *testing.T) {
	dev, addr := startFakeDevice(t)
	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	// Wait for the three handshake commands, then drain: the traffic
	// assertion below must read only what SendTracePath tried to send.
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

	longEntries := make([]byte, 64*2) // 64 entries of 2 bytes: 1 over the limit
	for _, tc := range []struct {
		name  string
		flags byte
		path  []byte
	}{
		{"empty path", 0x00, nil},
		{"misaligned for 2-byte entries", 0x01, []byte{0x11, 0x22, 0x33}},
		{"64 entries", 0x01, longEntries},
		{"179 bytes", 0x00, make([]byte, 179)},
	} {
		if _, err := client.SendTracePath(1, 1, tc.flags, tc.path); err == nil {
			t.Fatalf("%s: guard accepted the path", tc.name)
		}
	}

	select {
	case cmd := <-dev.cmds:
		t.Fatalf("guard-rejected command reached the device: %x", cmd)
	default:
	}
}

// TestSendTracePathSerializesWithSends pins the sendMu contract: while a trace
// send waits for its RESP_CODE_SENT, a concurrent SendMessageWithRetry must
// block instead of arming its confirmation slot (our code-matched waiter would
// otherwise swallow the text send's confirmation). The responder delays the
// trace reply to widen the overlap window.
func TestSendTracePathSerializesWithSends(t *testing.T) {
	dev, addr := startFakeDevice(t)

	release := make(chan struct{})
	dev.setResponder(cmdSendTracePath, func(_ int, _ []byte) [][]byte {
		<-release // hold the trace reply until the text send had its chance
		reply := make([]byte, traceSentReplyLen)
		reply[0] = respCodeSent
		binary.LittleEndian.PutUint32(reply[2:6], 1)
		binary.LittleEndian.PutUint32(reply[6:10], 100)
		return [][]byte{reply}
	})
	// The text send is accepted instantly once it finally gets its turn.
	dev.setResponder(cmdSendTxtMsg, func(_ int, _ []byte) [][]byte {
		return [][]byte{{respCodeOk}}
	})

	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	traceDone := make(chan error, 1)
	go func() {
		_, err := client.SendTracePath(1, 1, 0x00, []byte{0x42})
		traceDone <- err
	}()

	// Wait for the trace command to reach the device, then fire a text send:
	// it must not complete (nor reach the device) before the trace is done.
	select {
	case <-dev.cmds:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the trace command to reach the device")
	}

	contact := &Contact{PublicKey: make([]byte, 32), Name: "PEER"}
	contact.PublicKey[0] = 0x11
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- client.SendMessageWithRetry(contact, "hello", false, 1, 0, 5*time.Second)
	}()

	select {
	case err := <-sendDone:
		t.Fatalf("text send completed while the trace was still waiting: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-traceDone:
		if err != nil {
			t.Fatalf("SendTracePath: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for SendTracePath")
	}
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("SendMessageWithRetry: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for SendMessageWithRetry after release")
	}
}

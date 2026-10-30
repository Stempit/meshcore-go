package meshcore

import (
	"errors"
	"testing"
	"time"
)

// testDMContact returns a full-key flood-routed contact: the send loop skips
// the reset-path command (and its pacing sleep) for such contacts.
func testDMContact() *Contact {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return &Contact{PublicKey: key, Name: "peer", PathLength: pathLengthFlood}
}

// connectTCPForSend connects and waits out the handshake's trailing OK frames:
// a stray respCodeOk that lands inside a send's armed confirmation slot would
// confirm the send spuriously. The confirmation slots are command-agnostic —
// a known limitation documented in the README, not a test artifact.
func connectTCPForSend(t *testing.T, addr string) *MeshCoreClient {
	client := NewMeshCoreClient(nil)
	if err := client.ConnectTCP(addr); err != nil {
		t.Fatalf("ConnectTCP: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	time.Sleep(300 * time.Millisecond)
	return client
}

// TestSendMessageWithRetrySentConfirm covers the happy path: the device
// answers the TXT_MSG command with RESP_CODE_SENT, which confirms the send.
func TestSendMessageWithRetrySentConfirm(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSendTxtMsg, func(nth int, _ []byte) [][]byte {
		return [][]byte{{respCodeSent}}
	})

	client := connectTCPForSend(t, addr)

	if err := client.SendMessageWithRetry(testDMContact(), "hi", false, 1, 0, 100*time.Millisecond); err != nil {
		t.Fatalf("SendMessageWithRetry: %v", err)
	}
}

// TestSendMessageWithRetryDeviceRejected pins the ErrDeviceRejected contract:
// a device error for EVERY attempt means nothing was ever queued, so callers
// must not retry with different confirmation settings.
func TestSendMessageWithRetryDeviceRejected(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSendTxtMsg, func(nth int, _ []byte) [][]byte {
		return [][]byte{{respCodeErr, errCodeTableFull}}
	})

	client := connectTCPForSend(t, addr)

	err := client.SendMessageWithRetry(testDMContact(), "hi", false, 2, 0, 100*time.Millisecond)
	if !errors.Is(err, ErrDeviceRejected) {
		t.Fatalf("want ErrDeviceRejected, got %v", err)
	}
}

// TestSendMessageWithRetryNoConfirmation pins the ErrNoConfirmation contract:
// a silent device exhausts every attempt without confirming.
func TestSendMessageWithRetryNoConfirmation(t *testing.T) {
	_, addr := startFakeDevice(t) // silent for cmdSendTxtMsg

	client := connectTCPForSend(t, addr)

	err := client.SendMessageWithRetry(testDMContact(), "hi", false, 1, 0, 100*time.Millisecond)
	if !errors.Is(err, ErrNoConfirmation) {
		t.Fatalf("want ErrNoConfirmation, got %v", err)
	}
}

// TestSendMessageWithRetryLateACK pins the shared-confirmation-channel design:
// the delivery push for the FIRST attempt may arrive during the SECOND
// attempt's wait window and must still count as delivered.
func TestSendMessageWithRetryLateACK(t *testing.T) {
	dev, addr := startFakeDevice(t)
	dev.setResponder(cmdSendTxtMsg, func(nth int, _ []byte) [][]byte {
		if nth == 2 {
			// The push for attempt 1 arrives right after attempt 2 is written.
			return [][]byte{{pushCodeSendConfirmed}}
		}
		return nil // attempt 1: silent
	})

	client := connectTCPForSend(t, addr)

	if err := client.SendMessageWithRetry(testDMContact(), "hi", true, 2, 0, 500*time.Millisecond); err != nil {
		t.Fatalf("SendMessageWithRetry: %v", err)
	}
}

// TestNotifyDoesNotBlockOnFullConfirmChannel pins the R3 rule at the notify
// level: the frame handler calls notifyErr under confirmMu, so a blocking
// send on a full buffer would wedge the whole client forever.
func TestNotifyDoesNotBlockOnFullConfirmChannel(t *testing.T) {
	c := NewMeshCoreClient(nil)
	ch := make(chan error, 1)
	ch <- errors.New("stale") // buffer full: the waiter never consumed it
	c.confirmMu.Lock()
	c.sentConfirmCh = ch
	c.deliveredConfirmCh = ch
	c.confirmMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.notifyErr(errors.New("device error"))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyErr blocked on a full confirmation channel")
	}
}

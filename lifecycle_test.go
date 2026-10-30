package meshcore

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// stubConn is a Connection whose frames always fail to write, for exercising
// write-failure teardown without a real transport.
type stubConn struct {
	mu     sync.Mutex
	closed bool
}

func (s *stubConn) WriteFrame(payload []byte) error { return errors.New("transport gone") }

func (s *stubConn) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *stubConn) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// wireConnectedClient fabricates a connected client around conn, as startWith
// would, without a real transport or handshake.
func wireConnectedClient(conn Connection) *MeshCoreClient {
	c := NewMeshCoreClient(nil)
	c.conn = conn
	c.connected = true
	c.doneCh = make(chan struct{})
	c.handlersDone = make(chan struct{})
	c.incomingCh = make(chan interface{}, 1)
	return c
}

// TestWriteFailureInFrameHandlerDoesNotDeadlock pins the teardown rule: write()
// can run on the frame-handler goroutine (the post-message sendSyncNextMessage,
// among others), so its failure path must not wait synchronously for the frame
// handler to exit — it would wait for itself and wedge the client.
func TestWriteFailureInFrameHandlerDoesNotDeadlock(t *testing.T) {
	conn := &stubConn{}
	c := wireConnectedClient(conn)

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		defer close(c.handlersDone) // frameHandler's own defer
		// pushCodeMsgWaiting makes handleFrame write (sendSyncNextMessage).
		c.handleFrame([]byte{pushCodeMsgWaiting})
	}()

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handleFrame deadlocked after a write failure")
	}

	waitUntil(t, func() bool { return !c.IsConnected() },
		2*time.Second, "client still connected after write failure")
	waitUntil(t, conn.isClosed,
		2*time.Second, "transport not closed after write failure")
}

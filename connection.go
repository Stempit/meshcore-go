package meshcore

import (
	"time"

	"tinygo.org/x/bluetooth"
)

// ============================================================================
// Управление подключением
// ============================================================================

// Connect establishes the BLE connection to the device with the given address
// and initialises the MeshCore protocol over it.
func (c *MeshCoreClient) Connect(address bluetooth.Address) error {
	if c.IsConnected() {
		return nil
	}
	c.mu.Lock()
	c.address = address
	c.mu.Unlock()

	return c.startWith(
		func() (Connection, error) {
			return newBLETransport(c.adapter, address, c.frameSink(), c.logger)
		},
		func() error { return c.Connect(address) },
	)
}

// startWith brings up the MeshCore protocol over a transport created by dial.
// The order here is load-bearing: remember the reconnect closure, wait out the
// previous connection generation, drain stale frames, and only then dial —
// once the new transport is live the device may start delivering immediately
// (on reconnect the firmware flushes its queued frames to the newly accepted
// client), so nothing may be drained after the dial. Finally the connection is
// swapped in, the frame handler started and the initial handshake performed.
// On dial or handshake failure the transport is closed again.
func (c *MeshCoreClient) startWith(dial func() (Connection, error), reconnectFn func() error) error {
	if c.IsConnected() {
		return nil
	}
	c.mu.Lock()
	if c.connected { // re-check under the lock: a concurrent connect won
		c.mu.Unlock()
		return nil
	}
	// Remember the transport before dialing so Reconnect() retries it even
	// when the first attempt fails.
	c.reconnect = reconnectFn
	// Wait out the previous generation's frame handler and read loop so they
	// cannot feed stale frames past the drain below.
	prevHandlersDone := c.handlersDone
	prevConn := c.conn
	c.mu.Unlock()

	<-prevHandlersDone
	if prevConn != nil {
		_ = prevConn.Close()
	}

	if c.IsConnected() { // a concurrent connect completed while we waited
		return nil
	}

	// Drain stale frames buffered from any previous connection while no
	// transport can deliver fresh ones.
drainLoop:
	for {
		select {
		case <-c.notifyCh:
		default:
			break drainLoop
		}
	}

	conn, err := dial()
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.connected { // a concurrent connect won the race; drop ours
		c.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	// Закрываем старый doneCh, если он ещё не закрыт (например, после разрыва)
	select {
	case <-c.doneCh:
		// уже закрыт
	default:
		close(c.doneCh)
	}
	c.doneCh = make(chan struct{})
	// Disconnect() closed the previous incoming channel; reconnects need a
	// fresh one, otherwise the frame handler would send on a closed channel.
	c.incomingCh = make(chan interface{}, 100)
	c.conn = conn
	c.connected = true
	doneCh, handlersDone := c.doneCh, make(chan struct{})
	c.handlersDone = handlersDone
	c.mu.Unlock()

	// для канала входящих сообщений
	go c.frameHandler(doneCh, handlersDone)

	if err := c.handshake(); err != nil {
		c.Disconnect()
		return err
	}
	return nil
}

// handshake performs the initial command sequence every fresh connection
// starts with: APP_START, DEVICE_QUERY and time sync.
func (c *MeshCoreClient) handshake() error {
	if err := c.sendAppStart(); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)

	if err := c.sendDeviceQuery(); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)

	if err := c.sendSetDeviceTime(uint32(time.Now().Unix())); err != nil {
		c.logger.Warn("Failed to sync device time", "error", err)
	} else {
		c.logger.Info("Device time synced")
	}
	return nil
}

// frameSink returns the callback transports use to deliver one complete
// protocol frame into the client's processing pipeline.
func (c *MeshCoreClient) frameSink() func([]byte) {
	return func(data []byte) {
		select {
		case c.notifyCh <- data:
		default:
			c.logger.Warn("Notification channel full, dropping data")
		}
	}
}

// connectionLost is the callback transports invoke when the read loop dies
// unexpectedly. It runs on the transport's goroutine, so the state teardown
// happens in a separate goroutine (Close would otherwise wait for it).
func (c *MeshCoreClient) connectionLost(err error) {
	c.logger.Error("Connection lost", "error", err)
	go c.markDisconnected()
}

func (c *MeshCoreClient) Disconnect() error {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return nil
	}
	c.connected = false
	close(c.doneCh)
	// Snapshot the generation: the frame handler must observe the doneCh it
	// was started with, and teardown must wait for exactly it.
	handlersDone := c.handlersDone
	incomingCh := c.incomingCh
	conn := c.conn
	// Release c.mu before waiting: the frame handler may be inside
	// handleFrame waiting for c.mu and would never reach its doneCh select.
	c.mu.Unlock()

	// Close the transport before waiting so any blocked Read() in the
	// transport's read loop (or BLE notification handler) returns immediately
	// and the goroutine can exit.
	var closeErr error
	if conn != nil {
		c.logger.Info("Closing connection")
		closeErr = conn.Close()
	}
	<-handlersDone
	close(incomingCh)
	return closeErr
}

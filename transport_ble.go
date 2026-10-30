package meshcore

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// bleTransport implements Connection over the Nordic-UART-style BLE service.
// There is no length prefix on BLE: one protocol frame per write/notification,
// the characteristic boundaries delimit frames.
type bleTransport struct {
	device *bluetooth.Device
	rxChar *bluetooth.DeviceCharacteristic // host writes commands here
	txChar *bluetooth.DeviceCharacteristic // device notifies here

	writeMu sync.Mutex
}

// newBLETransport connects to the device, discovers the MeshCore service and
// enables notifications on the TX characteristic. Frames from the device are
// delivered to onFrame (one notification = one frame), invoked from the BLE
// stack's goroutine.
func newBLETransport(adapter *bluetooth.Adapter, address bluetooth.Address, onFrame func([]byte), logger *slog.Logger) (*bleTransport, error) {
	device, err := adapter.Connect(address, bluetooth.ConnectionParams{})
	if err != nil {
		logger.Error("Connect failed", "address", address.String(), "error", err)
		return nil, fmt.Errorf("connect failed: %w", err)
	}

	services, err := device.DiscoverServices(nil)
	if err != nil {
		logger.Error("Discover services failed", "error", err)
		device.Disconnect()
		return nil, fmt.Errorf("discover services failed: %w", err)
	}
	var targetService *bluetooth.DeviceService
	for _, svc := range services {
		if svc.UUID().String() == serviceUUID {
			targetService = &svc
			break
		}
	}
	if targetService == nil {
		logger.Error("MeshCore service not found")
		device.Disconnect()
		return nil, fmt.Errorf("meshcore service not found")
	}
	logger.Info("Found MeshCore service")

	chars, err := targetService.DiscoverCharacteristics(nil)
	if err != nil {
		logger.Error("Discover characteristics failed", "error", err)
		device.Disconnect()
		return nil, fmt.Errorf("discover characteristics failed: %w", err)
	}
	var rxChar, txChar *bluetooth.DeviceCharacteristic
	for _, ch := range chars {
		switch ch.UUID().String() {
		case rxCharacteristicUUID:
			rxChar = &ch
			logger.Info("Found RX characteristic")
		case txCharacteristicUUID:
			txChar = &ch
			logger.Info("Found TX characteristic")
		}
	}
	if rxChar == nil || txChar == nil {
		logger.Error("RX/TX characteristics not found")
		device.Disconnect()
		return nil, fmt.Errorf("RX/TX characteristics not found")
	}

	logger.Info("Enabling notifications on TX characteristic...")
	if err := txChar.EnableNotifications(func(data []byte) {
		logger.Debug("Notification received", "data", fmt.Sprintf("%x", data))
		onFrame(data)
	}); err != nil {
		logger.Error("EnableNotifications failed", "error", err)
		device.Disconnect()
		return nil, fmt.Errorf("enable notifications failed: %w", err)
	}
	logger.Info("Notifications enabled")
	// Give the BLE stack time to settle before the first command.
	time.Sleep(500 * time.Millisecond)

	logger.Info("Connected to device", "address", address.String())

	return &bleTransport{device: &device, rxChar: rxChar, txChar: txChar}, nil
}

// WriteFrame writes one frame payload to the RX characteristic.
func (t *bleTransport) WriteFrame(payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := t.rxChar.Write(payload)
	return err
}

// Close disconnects from the device.
func (t *bleTransport) Close() error {
	return t.device.Disconnect()
}

package meshcore

import (
	"fmt"

	"tinygo.org/x/bluetooth"
)

// NewClient connects to a MeshCore device over BLE and returns a client that
// has completed the initial sync. Without options it scans for any device
// advertising the "MeshCore-" prefix and picks the strongest signal; pass
// WithDeviceName or WithDeviceAddress to pin a specific device when several
// are in range. Unlike CreateClient it returns scan, connect and handshake
// failures as errors instead of panicking — the right entry point for tools
// that handle failures themselves.
func NewClient(opts ...BLEOption) (*MeshCoreClient, error) {
	scanOpts := applyBLEOptions(opts)

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enabling Bluetooth: %w", err)
	}
	log().Info("Bluetooth enabled")

	addr, name, err := scanMeshCore(adapter, scanOpts)
	if err != nil {
		return nil, fmt.Errorf("scanning for MeshCore device: %w", err)
	}
	log().Info("Found device", "name", name, "address", addr.String())

	client, selfInfoReceived := newSyncedClient(adapter)

	if err := client.Connect(addr); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", name, err)
	}

	if err := runStartupSync(client, selfInfoReceived, "BLE"); err != nil {
		return nil, err
	}

	return client, nil
}

// CreateClient is NewClient's panicking variant: it connects and syncs the
// same way, but panics on scan, connect or handshake failure instead of
// returning an error.
func CreateClient(opts ...BLEOption) *MeshCoreClient {
	client, err := NewClient(opts...)
	if err != nil {
		log().Error("Failed to create client", "error", err)
		panic(err)
	}
	return client
}

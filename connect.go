package meshcore

import (
	"tinygo.org/x/bluetooth"
)

// CreateClient connects to a MeshCore device over BLE and returns a client
// that has completed the initial sync. Without options it scans for any
// device advertising the "MeshCore-" prefix and picks the strongest signal;
// pass WithDeviceName or WithDeviceAddress to pin a specific device when
// several are in range. It panics on scan, connect or handshake failure.
func CreateClient(opts ...BLEOption) *MeshCoreClient {
	scanOpts := applyBLEOptions(opts)

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		log().Error("Failed to enable Bluetooth", "error", err)
		panic(err)
	}
	log().Info("Bluetooth enabled")

	addr, name, err := scanMeshCore(adapter, scanOpts)
	if err != nil {
		log().Error("Failed to scan MeshCore device", "error", err)
		panic(err)
	}
	log().Info("Found device", "name", name, "address", addr.String())

	client, selfInfoReceived := newSyncedClient(adapter)

	if err := client.Connect(addr); err != nil {
		log().Error("Failed to connect to device", "error", err)
		panic(err)
	}

	runStartupSync(client, selfInfoReceived, "BLE")

	return client
}

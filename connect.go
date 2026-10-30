package meshcore

import (
	"fmt"
	"strings"
	"time"

	"tinygo.org/x/bluetooth"
)

func CreateClient() *MeshCoreClient {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		log().Error("Failed to enable Bluetooth", "error", err)
		panic(err)
	}
	log().Info("Bluetooth enabled")

	addr, name, err := scanMeshCore(adapter)
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
	// defer func() {
	// 	client.Disconnect()
	// 	log().Info("Disconnected from device")
	// }()

	runStartupSync(client, selfInfoReceived, "BLE")

	return client
}

func scanMeshCore(adapter *bluetooth.Adapter) (bluetooth.Address, string, error) {
	resultChan := make(chan bluetooth.ScanResult)
	err := adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
		if strings.HasPrefix(result.LocalName(), "MeshCore-") {
			adapter.StopScan()
			resultChan <- result
		}
	})
	if err != nil {
		return bluetooth.Address{}, "", err
	}
	select {
	case res := <-resultChan:
		return res.Address, res.LocalName(), nil
	case <-time.After(10 * time.Second):
		adapter.StopScan()
		return bluetooth.Address{}, "", fmt.Errorf("timeout")
	}
}

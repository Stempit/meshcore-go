package meshcore

import (
	"fmt"
	"strings"
	"sync"
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

	client := NewMeshCoreClient(adapter)

	// Канал для синхронизации получения SelfInfo
	selfInfoReceived := make(chan struct{})
	var selfInfoOnce sync.Once

	client.OnSelfInfo(func(info *SelfInfo) {
		log().Info("Self info received",
			"name", info.Name,
			"pubkey", fmt.Sprintf("%x", info.PublicKey),
		)
		selfInfoOnce.Do(func() { close(selfInfoReceived) })
	})

	client.OnChannel(func(ch *Channel) {
		if ch.IsEmpty {
			log().Info("Channel empty", "index", ch.Index)
		} else {
			log().Info("Channel", "index", ch.Index, "name", ch.Name)
		}
	})

	if err := client.Connect(addr); err != nil {
		log().Error("Failed to connect to device", "error", err)
		panic(err)
	}
	// defer func() {
	// 	client.Disconnect()
	// 	log().Info("Disconnected from device")
	// }()

	log().Info("Connected to device, waiting for data...")

	time.Sleep(2 * time.Second) // даём время для инициализации
	log().Info("Waiting for self info...")

	// Ждём получения SelfInfo (или таймаут)
	select {
	case <-selfInfoReceived:
		log().Info("Self info ready, requesting channels...")
	case <-time.After(5 * time.Second):
		log().Warn("Timeout waiting for self info")
	}

	// Запрашиваем каналы
	log().Info("Requesting channels (count=8)")
	if err := client.RequestChannels(8); err != nil {
		log().Error("Failed to request channels", "error", err)
		panic(err)
	}

	// Ждём ответы на каналы
	time.Sleep(5 * time.Second)
	log().Info("Waiting for channels completed")

	// Ждём немного, чтобы каналы успели прийти
	time.Sleep(2 * time.Second)

	// Запрос всех контактов (since=0)
	log().Info("Requesting contacts (since=0)")
	if err := client.RequestContacts(0); err != nil {
		log().Error("RequestContacts error", "error", err)
	}

	// Ждём, пока контакты загрузятся (не надёжно, но для теста)
	time.Sleep(3 * time.Second)
	log().Info("Client started")

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

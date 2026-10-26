package meshcore

import (
	"fmt"
	"sync"
	"time"
)

// CreateSerialClient opens the given serial port, performs the MeshCore
// initialisation handshake, and returns a ready-to-use MeshCoreClient.
// portPath is the OS device path (e.g. "/dev/ttyUSB0", "/dev/tty.usbmodem*", "COM3").
// baud should be 115200 for standard MeshCore firmware.
func CreateSerialClient(portPath string, baud int) *MeshCoreClient {
	client := &MeshCoreClient{
		contacts:    make(map[string]*Contact),
		channels:    make(map[int]*Channel),
		messages:    make(map[string][]*Message),
		channelMsgs: make(map[int][]*ChannelMessage),
		notifyCh:    make(chan []byte, 100),
		doneCh:      make(chan struct{}),
		incomingCh:  make(chan interface{}, 100),
		logger:      log(),
	}

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

	if err := client.ConnectSerial(portPath, baud); err != nil {
		log().Error("Failed to connect to serial device", "port", portPath, "error", err)
		panic(err)
	}

	log().Info("Serial connected, waiting for self info...")

	select {
	case <-selfInfoReceived:
		log().Info("Self info ready, requesting channels...")
	case <-time.After(5 * time.Second):
		log().Warn("Timeout waiting for self info")
	}

	log().Info("Requesting channels (count=8)")
	if err := client.RequestChannels(8); err != nil {
		log().Error("Failed to request channels", "error", err)
		panic(err)
	}

	time.Sleep(5 * time.Second)

	time.Sleep(2 * time.Second)

	log().Info("Requesting contacts (since=0)")
	if err := client.RequestContacts(0); err != nil {
		log().Error("RequestContacts error", "error", err)
	}

	time.Sleep(3 * time.Second)
	log().Info("Serial client started")

	return client
}

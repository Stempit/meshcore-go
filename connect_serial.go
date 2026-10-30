package meshcore

import (
	"fmt"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// ConnectSerial opens a serial port and initialises the MeshCore protocol
// over it. portPath is the OS path to the serial device (e.g. "/dev/ttyUSB0",
// "COM3"). baud should be 115200 for standard MeshCore firmware.
func (c *MeshCoreClient) ConnectSerial(portPath string, baud int) error {
	return c.startWith(
		func() (Connection, error) {
			stream, err := dialSerial(portPath, baud)
			if err != nil {
				return nil, err
			}
			c.logger.Info("Serial port opened", "port", portPath, "baud", baud)
			return newStreamTransport(stream, c.frameSink(), c.connectionLost, c.logger), nil
		},
		func() error { return c.ConnectSerial(portPath, baud) },
	)
}

// newSyncedClient returns a fresh client plus the channel that closes when the
// first SelfInfo arrives. All three factories (BLE, serial, TCP) wire the same
// startup callbacks; adapter is nil for the stream transports.
func newSyncedClient(adapter *bluetooth.Adapter) (*MeshCoreClient, chan struct{}) {
	client := NewMeshCoreClient(adapter)

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

	return client, selfInfoReceived
}

// runStartupSync performs the post-connect startup sequence shared by all
// three factories: wait for self info, request channels and contacts.
// Panics on channel-request failure, matching the factories' contract.
func runStartupSync(client *MeshCoreClient, selfInfoReceived chan struct{}, transport string) {
	log().Info(transport + " connected, waiting for self info...")

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
	log().Info(transport + " client started")
}

// CreateSerialClient opens the given serial port, performs the MeshCore
// initialisation handshake, and returns a ready-to-use MeshCoreClient.
// portPath is the OS device path (e.g. "/dev/ttyUSB0", "/dev/tty.usbmodem*", "COM3").
// baud should be 115200 for standard MeshCore firmware.
func CreateSerialClient(portPath string, baud int) *MeshCoreClient {
	client, selfInfoReceived := newSyncedClient(nil)

	if err := client.ConnectSerial(portPath, baud); err != nil {
		log().Error("Failed to connect to serial device", "port", portPath, "error", err)
		panic(err)
	}

	runStartupSync(client, selfInfoReceived, "Serial")

	return client
}

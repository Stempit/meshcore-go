// Command scan lists the MeshCore devices currently advertising over BLE,
// strongest signal first. Use it to discover the Name and AddrString values
// to pass to WithDeviceName/WithDeviceAddress (or to configure as
// device_name/device_address in an application's settings) when several
// MeshCore devices are in range.
//
// Usage:
//
//	go run ./examples/scan
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/stempit/meshcore-go"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	meshcore.SetLogger(logger)

	devices, err := meshcore.ScanDevices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan failed: %v\n", err)
		os.Exit(1)
	}
	if len(devices) == 0 {
		fmt.Println("no MeshCore devices found")
		return
	}
	for _, d := range devices {
		fmt.Printf("%s  %s  %d dBm\n", d.Name, d.AddrString, d.RSSI)
	}
}

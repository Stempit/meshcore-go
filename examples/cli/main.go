// Command cli is a terminal for the device's ASCII CLI: it connects to a
// MeshCore device (BLE by default, or a serial port with -serial), runs the
// commands given on the command line — or reads them line by line from stdin
// — and prints the device's replies.
//
// Usage:
//
//	go run ./examples/cli [-serial /dev/ttyUSB0] [command ...]
//
// With command arguments each one is sent once ("ver", "get tx",
// "set radio 869.618,250,9,5"); without them the example reads commands from
// stdin until EOF, an empty line or "exit". Quote multi-word commands in the
// shell ("get tx") or each word is sent as a separate command. Requires
// companion firmware v14+ (CMD_RUN_CLI_COMMAND); older firmware answers
// unsupported_cmd.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/stempit/meshcore-go"
)

func main() {
	serial := flag.String("serial", "", "serial port path (e.g. /dev/ttyUSB0, COM3); BLE is used when empty")
	flag.Parse()
	cmds := flag.Args()

	// 115200 is the baud rate of standard MeshCore companion firmware.
	var client *meshcore.MeshCoreClient
	if *serial != "" {
		client = meshcore.CreateSerialClient(*serial, 115200)
	} else {
		client = meshcore.CreateClient()
	}
	defer client.Disconnect()

	if len(cmds) > 0 {
		for _, cmd := range cmds {
			runAndPrint(client, cmd)
		}
		return
	}

	fmt.Println("device CLI — empty line or 'exit' to quit")
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			break
		}
		cmd := strings.TrimSpace(sc.Text())
		if cmd == "" || cmd == "exit" || cmd == "quit" {
			break
		}
		runAndPrint(client, cmd)
	}
}

// runAndPrint sends one CLI command and prints the reply or the error.
// CLI-level failures ("Error, ...", "Err - ...") arrive as reply text, not as
// errors — the device's own wording is the most useful thing to show.
func runAndPrint(client *meshcore.MeshCoreClient, cmd string) {
	// Echo the command: with several one-shot commands per run, unlabeled
	// replies would be indistinguishable from each other.
	fmt.Printf("$ %s\n", cmd)
	reply, err := client.RunCLICommand(cmd)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Println(reply)
}

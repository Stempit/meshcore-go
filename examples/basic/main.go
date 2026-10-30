// Command basic is a tour of the meshcore-go driver: it connects to a
// MeshCore device over BLE (the default transport), sends a DM and a channel
// message, syncs the messages stored on the device while the app was down,
// then live-prints and auto-replies to everything that arrives.
//
// Usage:
//
//	go run ./examples/basic [contact-name]
//
// The optional argument names the DM recipient (exact contact name or hex
// public-key prefix); without it the DM section is skipped.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/stempit/meshcore-go"
)

// Send pacing used throughout: 3 tries over the saved path, 3 over the reset
// path, 3 s per attempt — the values the radio link is known to tolerate.
const (
	savedPathTries = 3
	resetPathTries = 3
	sendTimeout    = 3 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	meshcore.SetLogger(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Connect over Bluetooth LE. For a USB/UART device use instead:
	//	client := meshcore.CreateSerialClient("/dev/ttyUSB0", 115200)
	client := meshcore.CreateClient()
	defer client.Disconnect()

	// ─── Send a DM ────────────────────────────────────────────────────────────
	// Recipients resolve by exact contact name or by hex public-key prefix.
	if len(os.Args) > 1 {
		if contact := client.FindContactByName(os.Args[1]); contact != nil {
			// ack=false waits only for the device to confirm the transmission;
			// ack=true waits for the end-to-end delivery confirmation.
			if err := client.SendMessageWithRetry(contact, "ping (sent)", false, savedPathTries, resetPathTries, sendTimeout); err != nil {
				fmt.Printf("send (sent-confirm): %v\n", err)
			}
			if err := client.SendMessageWithRetry(contact, "ping (delivered)", true, savedPathTries, resetPathTries, sendTimeout); err != nil {
				fmt.Printf("send (delivery-confirm): %v\n", err)
			}
		} else {
			fmt.Printf("contact not found: %s\n", os.Args[1])
		}
	}

	// ─── Send a channel message ───────────────────────────────────────────────
	// Channels are addressed by name or index; each channel send is
	// transmitted up to `transmissions` times (deduplicated by receivers) to
	// survive silent RF drops.
	for _, ch := range client.GetChannels() {
		if ch.IsEmpty {
			continue
		}
		if err := client.SendChannelMessageWithRetry(ch, "hello from meshcore-go", false, 1, 3, sendTimeout); err != nil {
			fmt.Printf("channel send: %v\n", err)
		}
		break
	}

	// ─── Sync messages stored on the device ──────────────────────────────────
	// Everything that arrived while the app was down is pulled once: ask for
	// the first message and the device chains the rest, ending with the
	// OnNoMoreMessages callback. The callback is registered once and stays —
	// re-registering after Connect would race the driver's frame handler.
	syncDone := make(chan struct{})
	var syncOnce sync.Once
	client.OnNoMoreMessages(func() { syncOnce.Do(func() { close(syncDone) }) })
	if err := client.SyncNextMessage(); err != nil {
		fmt.Printf("sync: %v\n", err)
		syncOnce.Do(func() { close(syncDone) })
	}
	select {
	case <-syncDone:
	case <-time.After(15 * time.Second):
		fmt.Println("sync timed out, proceeding with what arrived")
	}
	for _, m := range client.GetAllMessages() {
		fmt.Printf("stored DM from %x: %s\n", m.SenderKey[:6], m.Text)
	}
	for _, m := range client.GetAllChannelMessages() {
		fmt.Printf("stored channel %d from %s: %s\n", m.ChannelIndex, m.SenderName, m.Text)
	}

	// ─── Live messages + auto-reply ──────────────────────────────────────────
	// IncomingMessages yields *meshcore.Message (DMs) and
	// *meshcore.ChannelMessage values, interleaved in arrival order.
	go func() {
		for msg := range client.IncomingMessages() {
			switch m := msg.(type) {
			case *meshcore.Message:
				fmt.Printf("DM from %x: %s\n", m.SenderKey[:6], m.Text)
				// Resolve the sender to a contact; unknown keys become
				// throwaway contacts addressed by their key prefix.
				contact := client.FindContactByKeyPrefix(m.SenderKey[:6])
				if contact == nil {
					contact = &meshcore.Contact{
						PublicKey: m.SenderKey,
						Name:      fmt.Sprintf("?%x", m.SenderKey[:6]),
					}
				}
				if err := client.SendMessageWithRetry(contact, "copy that", false, savedPathTries, resetPathTries, sendTimeout); err != nil {
					fmt.Printf("reply: %v\n", err)
				}
			case *meshcore.ChannelMessage:
				fmt.Printf("channel %d from %s: %s\n", m.ChannelIndex, m.SenderName, m.Text)
			}
		}
	}()

	fmt.Println("listening — Ctrl+C to exit")
	<-ctx.Done()
}

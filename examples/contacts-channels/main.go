// Command contacts-channels demonstrates contact and channel administration:
// it connects to a MeshCore device, walks through the channel scenarios
// (create a private channel, join the public and a hashtag channel), prints
// the resulting channel table with keys, optionally adds a contact by public
// key — and then removes everything it created, so the demo leaves no trace
// on the device unless asked to keep it.
//
// Usage:
//
//	go run ./examples/contacts-channels [flags]
//
// Flags:
//
//	-serial string   USB/UART port (e.g. /dev/ttyUSB0); default transport is BLE
//	-tcp     string  TCP address of a WiFi/Ethernet companion (host:5000)
//	-pubkey  string  add a contact with this hex public key (64 hex chars)
//	-name    string  name for the added contact (default "Demo Contact")
//	-type    string  contact type: chat | repeater | room | sensor (default chat)
//	-keep            keep the created channels and contact on the device
//
// The scenario, step by step (the same calls a bot would make):
//
//  1. CreatePrivateChannel("...")   — random 16-byte key into a free slot;
//     the returned Channel.PSK is what you share via
//     meshcore://channel/add?name=...&secret=<32 hex>.
//  2. JoinHashtagChannel("#...")    — key derived as SHA256("#name")[:16],
//     compatible with the official apps.
//  3. JoinPublicChannel("")         — the firmware's well-known public PSK.
//  4. AddContact(...)               — one call for chat/repeater/room/sensor
//     contacts; the key comes from an advert, QR or GetContactByKey.
//  5. RemoveChannel / RemoveContact — free the slots again.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/stempit/meshcore-go"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	meshcore.SetLogger(logger)

	serial := flag.String("serial", "", "serial port (e.g. /dev/ttyUSB0); default transport is BLE")
	tcpAddr := flag.String("tcp", "", "TCP address of the companion (host:5000)")
	pubKeyHex := flag.String("pubkey", "", "add a contact with this hex public key (64 hex chars)")
	contactName := flag.String("name", "Demo Contact", "name for the added contact")
	contactType := flag.String("type", "chat", "contact type: chat | repeater | room | sensor")
	keep := flag.Bool("keep", false, "keep the created channels and contact on the device")
	flag.Parse()

	// ─── Connect: BLE by default, or the transport the flags name ────────────
	var client *meshcore.MeshCoreClient
	switch {
	case *serial != "":
		client = meshcore.CreateSerialClient(*serial, 115200)
	case *tcpAddr != "":
		client = meshcore.CreateTCPClient(*tcpAddr)
	default:
		client = meshcore.CreateClient()
	}
	defer client.Disconnect()

	created := []*meshcore.Channel{}

	// ─── 0. What the device knows right now ──────────────────────────────────
	// ListContacts/ListChannels query the device first, so this is the full
	// picture even right after connect; the records are deep copies carrying
	// every field — keys, PSKs, paths, timestamps.
	if contacts, err := client.ListContacts(); err != nil {
		fmt.Printf("list contacts: %v\n", err)
	} else {
		fmt.Printf("contacts on device (%d):\n", len(contacts))
		for _, ct := range contacts {
			fmt.Printf("  %-16s %-8s %s lastmod %d\n",
				ct.Name, ct.TypeName(), ct.PublicKeyHex(), ct.LastMod)
		}
	}
	if channels, err := client.ListChannels(); err != nil {
		fmt.Printf("list channels: %v\n", err)
	} else {
		fmt.Printf("channel slots (%d):\n", len(channels))
		for _, ch := range channels {
			if ch.IsEmpty {
				fmt.Printf("  [%d] <empty>\n", ch.Index)
				continue
			}
			fmt.Printf("  [%d] %-16s psk %s hash %02x\n",
				ch.Index, ch.Name, ch.PSKHex(), ch.HashByte())
		}
	}

	// ─── 1. Private channel ──────────────────────────────────────────────────
	// A random 16-byte key goes into the first free slot; participants join
	// with the same name+key (the QR string below is what the official apps
	// scan).
	priv, err := client.CreatePrivateChannel("go-demo")
	if err != nil {
		fmt.Printf("create private channel: %v\n", err)
	} else {
		created = append(created, priv)
		fmt.Printf("private channel: slot %d, name %q, PSK %s\n",
			priv.Index, priv.Name, hex.EncodeToString(priv.PSK))
		fmt.Printf("  share as QR: meshcore://channel/add?name=%s&secret=%s\n",
			url.QueryEscape(priv.Name), hex.EncodeToString(priv.PSK))
	}

	// ─── 2. Hashtag channel ──────────────────────────────────────────────────
	// The key is derived client-side (SHA256("#name")[:16]) — the firmware
	// only ever sees a normal channel; the leading # is optional.
	tag, err := client.JoinHashtagChannel("#gotest")
	if err != nil {
		fmt.Printf("join hashtag channel: %v\n", err)
	} else {
		created = append(created, tag)
		fmt.Printf("hashtag channel: slot %d, name %q, PSK %s\n",
			tag.Index, tag.Name, hex.EncodeToString(tag.PSK))
	}

	// ─── 3. Public channel ───────────────────────────────────────────────────
	// The firmware's well-known PSK; slot 0 usually already holds it, this
	// writes an alias copy into the next free slot.
	pub, err := client.JoinPublicChannel("")
	if err != nil {
		fmt.Printf("join public channel: %v\n", err)
	} else {
		created = append(created, pub)
		fmt.Printf("public channel: slot %d, name %q\n", pub.Index, pub.Name)
	}

	// ─── 4. Optional: add a contact by public key ────────────────────────────
	// One AddContact call covers chat, repeater, room server and sensor
	// contacts — only the Type differs. The 32-byte key comes from an advert
	// (OnAdvert), a QR code or another bot.
	var added *meshcore.Contact
	if *pubKeyHex != "" {
		key, err := hex.DecodeString(strings.TrimSpace(*pubKeyHex))
		if err != nil || len(key) != 32 {
			fmt.Printf("bad -pubkey (want 64 hex chars): %q\n", *pubKeyHex)
		} else {
			added = &meshcore.Contact{
				PublicKey: key,
				Name:      *contactName,
				Type:      contactTypeValue(*contactType),
			}
			if err := client.AddContact(added); err != nil {
				fmt.Printf("add contact: %v\n", err)
			} else {
				fmt.Printf("contact added: %q type %d\n", added.Name, added.Type)
				// Read it back the way a bot would verify the write.
				if ct, err := client.GetContactByKey(key); err != nil {
					fmt.Printf("read back: %v\n", err)
				} else {
					fmt.Printf("read back: name %q, lastmod %d\n", ct.Name, ct.LastMod)
				}
			}
		}
	}

	// ─── Resulting channel table ─────────────────────────────────────────────
	fmt.Println("channels now:")
	for _, ch := range client.GetChannels() {
		if ch.IsEmpty {
			fmt.Printf("  [%d] <empty>\n", ch.Index)
			continue
		}
		fmt.Printf("  [%d] %-20s PSK %s\n", ch.Index, ch.Name, hex.EncodeToString(ch.PSK))
	}

	// ─── Cleanup ─────────────────────────────────────────────────────────────
	// "Removing" a channel means overwriting its slot with an empty name and
	// a zero key — there is no dedicated delete command on the wire.
	if *keep {
		fmt.Println("keeping everything created (-keep)")
		return
	}
	for _, ch := range created {
		if err := client.RemoveChannel(ch.Index); err != nil {
			fmt.Printf("remove channel slot %d: %v\n", ch.Index, err)
		} else {
			fmt.Printf("channel slot %d freed\n", ch.Index)
		}
	}
	if added != nil {
		if err := client.RemoveContact(added); err != nil {
			fmt.Printf("remove contact: %v\n", err)
		} else {
			fmt.Printf("contact %q removed\n", added.Name)
		}
	}
}

// contactTypeValue maps the CLI type name to the firmware's ADV_TYPE_* value.
func contactTypeValue(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "repeater":
		return meshcore.AdvTypeRepeater
	case "room", "roomserver", "room server":
		return meshcore.AdvTypeRoom
	case "sensor":
		return meshcore.AdvTypeSensor
	default:
		return meshcore.AdvTypeChat
	}
}

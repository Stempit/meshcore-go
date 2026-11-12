# meshcore-go 📡🔧

**A Go driver for MeshCore LoRa companion devices — BLE, serial or TCP, wire to API.**

*[🇷🇺 Русская версия](README_RU.md)*

`meshcore-go` connects to a [MeshCore](https://meshcore.io) radio device from
Go, speaks the companion-device wire protocol over **Bluetooth LE** (default),
**USB/UART serial** or **TCP** (WiFi/Ethernet companion firmware serves the
same protocol on TCP port 5000), and hands you a client with contacts,
channels, message history, and delivery-confirmed sends. It is the radio layer
of the meshcore family — bots, HTTP gateways and voice bridges are separate
projects built on top of this driver.

## Install

```sh
go get github.com/stempit/meshcore-go
```

**Requirements:** Go per `go.mod`. For BLE, the host needs a Bluetooth adapter;
for serial, access to the device node (on Linux add yourself to the `dialout`
group, e.g. `sudo usermod -aG dialout $USER`). On Linux, BLE scanning also needs
`CAP_NET_RAW` — either run via `sudo`, or grant the binary once:

```sh
sudo setcap cap_net_raw,cap_net_admin+eip ./your-app
```

## Quick start

See [examples/basic](examples/basic/main.go): connect over BLE, send a DM with
retry, read messages stored on the device, process live arrivals and reply.

```go
client := meshcore.CreateClient() // BLE scan (default)
// client := meshcore.CreateSerialClient("/dev/ttyUSB0", 115200)
// client := meshcore.CreateTCPClient("192.168.1.50:5000")
defer client.Disconnect()

client.SendMessageWithRetry(contact, "hello", true, 3, 3, 3*time.Second)
```

### BLE device selection

The BLE scan accepts any device advertising the `MeshCore-` prefix. When
several MeshCore devices are in range it keeps scanning until the picture is
complete, then picks the strongest signal and logs every candidate — pass an
option to pin a specific device instead:

```go
client := meshcore.CreateClient(meshcore.WithDeviceName("MeshCore-AB12"))
// or by platform address — MAC on Linux/Windows, CoreBluetooth UUID on macOS:
client := meshcore.CreateClient(meshcore.WithDeviceAddress("AA:BB:CC:DD:EE:FF"))
```

Run [examples/scan](examples/scan/main.go) to list the devices currently
advertising with their names, addresses and signal strengths;
`ScanDevices()` returns the same list programmatically.

### Device CLI commands

Companion firmware v14+ runs the device's ASCII CLI over the binary protocol
(`CMD_RUN_CLI_COMMAND`): `RunCLICommand(cmd)` sends one command and returns
the device's text reply — `ver`, `get/set radio ...`, `power`, `reboot` and
everything else listed in the firmware's `docs/cli_commands.md`.

```go
reply, err := client.RunCLICommand("get tx")
```

`get` replies start with `"> "`, successful `set` commands answer `OK` (or
`OK - ...`), failures answer `Error, ...` — those come back as the reply
string, not as a Go error; a `*DeviceError` (e.g. `unsupported_cmd`) means
the firmware predates v14. Run [examples/cli](examples/cli/main.go) as a
one-shot command tool or an interactive terminal:

```sh
go run ./examples/cli -serial /dev/ttyUSB0 ver "get tx"
```

(Quote multi-word commands — `"get tx"`, not `get tx`, or the shell splits
them into separate one-shot commands.)

### Contacts & channels administration

The driver can manage what the device stores — contacts (chat, repeater,
room server, sensor) and channels (group chats). Every call waits for the
device's answer: success returns `nil`/the result, a rejection comes back as
`*DeviceError` (the firmware's `ERR_CODE_*`: `table_full`, `not_found`,
`unsupported_cmd`, `illegal_arg`), a silent device as `ErrNoResponse`.

**Contacts.** The 32-byte public key comes from an advert (`OnAdvert`),
`GetContactByKey`, or a QR code (`meshcore://contact/add?public_key=<64 hex>&type=<1-4>`
per the firmware's `docs/qr_codes.md`):

```go
// a repeater you scanned or got from a QR code
err := client.AddContact(&meshcore.Contact{
    PublicKey: key,                // full 32 bytes (required)
    Name:      "Repeater-1",       // trimmed to 31 chars, like the firmware
    Type:      meshcore.AdvTypeRepeater, // Chat=1, Repeater=2, Room=3, Sensor=4
    Flags:     meshcore.ContactFlagFavorite, // bitmask; 0 = no flags
})

ct, err := client.GetContactByKey(key) // read one contact back (cache + callback refresh)
err = client.RemoveContact(contact)    // delete; *DeviceError not_found if absent
```

`AddContact` adds a new contact and updates an existing one alike — the
firmware matches by the full key. `table_full` means the device storage is
full: remove an inactive contact and retry.

**Channels.** On the wire, joining a channel *is* writing it into a free
channel slot (`CMD_SET_CHANNEL`); there is no separate join command. The
driver picks the first free slot for you and returns the filled `Channel`:

```go
// create a private channel: random 16-byte key, share it with participants
ch, err := client.CreatePrivateChannel("Team")
qr := fmt.Sprintf("meshcore://channel/add?name=%s&secret=%s",
    url.QueryEscape(ch.Name), hex.EncodeToString(ch.PSK))

// join a private channel from its QR / a friend's message
secret, _ := hex.DecodeString("<32 hex chars from the QR>")
ch, err = client.AddChannel("Team", secret) // PSK must be exactly 16 bytes

// the well-known public channel, and a hashtag channel
client.JoinPublicChannel("")        // alias defaults to "Public" (firmware PSK)
client.JoinHashtagChannel("#test")  // key = SHA256("#test")[:16], apps-compatible

client.RemoveChannel(ch.Index) // free the slot (empty name + zero key)
```

Rules worth knowing:

- Channel keys are exactly **16 bytes** (128-bit; the firmware rejects
  256-bit keys), channel names and contact names are trimmed to **31
  characters** — same limits as the official apps.
- `JoinHashtagChannel` derives the key client-side (first 16 bytes of
  `SHA256("#name")`) — the firmware knows nothing about hashtags, so channels
  created this way are compatible with the official apps. The leading `#` is
  optional; the canonical name is stored with it.
- `OnContactDeleted(func(pubKey []byte))` fires when the **device** removes a
  contact itself (push `0x8F`, storage-overflow eviction) — use it to keep
  your bot's database in step. Your own `RemoveContact` calls do not fire it;
  their result is the returned error.
- These calls wait for the device reply, so issue no message sends while one
  is in flight (the same one-thing-at-a-time rule as `RunCLICommand`).
- To verify what landed, re-read the state: `GetContactByKey`,
  `GetChannels()` / `RequestChannels(n)`, `RequestContacts(0)`.

**Listing with all details.** `ListContacts()` / `ListChannels()` ask the
device for the current state first (a full `GET_CONTACTS` iteration / a
`GET_CHANNEL` sweep over every slot) and return the complete records —
public keys, PSKs, paths, timestamps — as deep-copied snapshots that are safe
to mutate, serialize or hand to a UI:

```go
contacts, err := client.ListContacts() // sorted by name, fresh from the device
for _, ct := range contacts {
    fmt.Printf("%-16s %-8s %s\n", ct.Name, ct.TypeName(), ct.PublicKeyHex())
}

channels, err := client.ListChannels() // every slot by index, empty ones included
for _, ch := range channels {
    if ch.IsEmpty {
        continue
    }
    fmt.Printf("[%d] %-20s psk %s hash %02x\n",
        ch.Index, ch.Name, ch.PSKHex(), ch.HashByte())
}
```

The same derived helpers work on any `Contact`/`Channel` value, including the
ones from `GetContacts()` / `GetChannelByName()`: `PublicKeyHex()`,
`TypeName()` (chat/repeater/room/sensor), `PSKHex()` and `HashByte()` — the
1-byte channel hash the firmware uses to address group packets, handy for
correlating raw `0x88` frames. `GetContacts()`/`GetChannels()` stay the cheap
cache readers (shared pointers, no device round trip); reach for `List*` when
the data must be current.

Run [examples/contacts-channels](examples/contacts-channels/main.go) to see
the whole flow against a real device (create/join, print, then clean up):

```sh
go run ./examples/contacts-channels -serial /dev/ttyUSB0
```

## What it handles

- **Transport** — BLE ([tinygo.org/x/bluetooth](https://github.com/tinygo-org/bluetooth)),
  serial ([go.bug.st/serial](https://github.com/bugst/go-serial)) or TCP. All
  three implement one internal `Connection` interface; the stream transports
  (serial, TCP) share the firmware's `<`/`>` length-prefixed framing.
- **Framing/parsing** — decodes device frames, including the raw `0x88 LOG_RX_DATA`
  frames used for the channel-reliability early-stop.
- **Sending** — DMs (`SendMessageWithRetry`, sent- or delivery-confirmation
  per call) and channel messages (`SendChannelMessageWithRetry`,
  multi-transmission with adaptive early-stop once the mesh rebroadcasts the
  packet).
- **Device CLI** — `RunCLICommand` runs the device's ASCII CLI commands
  (firmware v14+) and returns the reply text.
- **Contacts & channels administration** — add/remove contacts of every type
  (chat, repeater, room server, sensor), read one by key, create private
  channels, join public/hashtag channels, free channel slots (see the section
  above).
- **Device clock** — `GetDeviceTime` queries the device RTC (unix seconds,
  returned in UTC).
- **Adverts / contacts** — self-advert, the contact list and advert stream,
  contact verification keys.
- **Loose name matching** — `FindContactByNameLoose` and friends: lookups that
  forgive case, emoji and punctuation, plus Cyrillic↔Latin phonetic matching
  (what voice front-ends need).
- **History** — message accumulation with an injectable `HistoryLogger`.
- **Mirroring hook** — implement `PacketPublisher` and attach it to mirror raw
  packets (e.g. to MQTT); the driver itself has no broker dependency.

## Notes

- MeshCore channel (group) messages have **no delivery ACK**; the device's `OK`
  for a channel send only confirms the *link*. The reliability story is
  multi-transmission with early-stop — see the `SendChannelMessageWithRetry`
  docs.
- Only one send can be in flight at a time; concurrent senders must serialise
  (the bot's dispatcher does this with a serialized sender).
- Confirmation slots are command-agnostic: while a send waits for its
  confirmation, a stray OK/error from any other command — the startup
  handshake's trailing one included — can complete it spuriously. Issue no
  other commands while a send is in flight. `RunCLICommand` serialises
  device commands among themselves (a dedicated mutex, never held across
  send retries), but the same rule applies in reverse: issue no message
  sends while a CLI command waits for its reply.
- `CreateClient` / `CreateSerialClient` / `CreateTCPClient` log and panic when
  the transport itself cannot be established; configuration errors are the
  caller's to check.
- TCP: the device must run companion firmware with WiFi or Ethernet enabled
  (`-D ENABLE_WIFI_INTERFACE` builds, or an Ethernet companion board). It
  serves the companion protocol on TCP port **5000** (`TCP_PORT` /
  `ETHERNET_TCP_PORT`) and accepts a **single client** — a new connection
  silently replaces the previous one. There is no TLS/auth: restrict access at
  the network level. (Ethernet builds with `ETHERNET_RAW_LINE=1` speak a
  line-based CLI instead and are not supported by this driver.)
- `SetLogger` swaps the driver's logger (default: `slog.Default()`, tagged
  `module=meshcore`). Call before connecting.

## Related projects

- [meshcore-kit](https://github.com/stempit/meshcore-kit) — the
  shared app runtime built on this driver: connection selection, startup
  stored-message sync, a live inbox with cursors, keepalive/reconnect — plus
  the 140-byte message fitter.
- [meshcore-go-bot](https://github.com/stempit/meshcore-go-bot) — the full-featured mesh chat
  bot (commands, games, Telegram/MQTT bridges).
- [meshcore-http-gateway](https://github.com/stempit/meshcore-http-gateway) —
  the mesh behind a plain HTTP API.
- [meshcore-mcp-gateway](https://github.com/stempit/meshcore-mcp-gateway) —
  the mesh as voice tools for the XiaoZhi assistant.

The firmware this driver talks to is upstream
[MeshCore](https://github.com/meshcore-dev/MeshCore).

## License

MIT — see [LICENSE](LICENSE).

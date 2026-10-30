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
  other commands while a send is in flight.
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

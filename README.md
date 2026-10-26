# meshcore-go 📡🔧

**A Go driver for MeshCore LoRa companion devices — BLE or serial, wire to API.**

*[🇷🇺 Русская версия](README_RU.md)*

`meshcore-go` connects to a [MeshCore](https://meshcore.io) radio device from
Go, speaks the companion-device wire protocol over **Bluetooth LE** (default)
or **USB/UART serial**, and hands you a client with contacts, channels, message
history, and delivery-confirmed sends. It is the radio layer of the meshcore
family — bots, HTTP gateways and voice bridges are separate projects built on
top of this driver.

## Install

```sh
go get github.com/stempit/meshcore-go
```

## Quick start

See [examples/basic](examples/basic/main.go): connect over BLE, send a DM with
retry, read messages stored on the device, process live arrivals and reply.

```go
client := meshcore.CreateClient() // BLE scan (default)
// client := meshcore.CreateSerialClient("/dev/ttyUSB0", 115200)
defer client.Disconnect()

client.SendMessageWithRetry(contact, "hello", true, 3, 3, 3*time.Second)
```

## What it handles

- **Transport** — BLE ([tinygo.org/x/bluetooth](https://github.com/tinygo-org/bluetooth))
  or serial ([go.bug.st/serial](https://github.com/bugst/go-serial)).
- **Framing/parsing** — decodes device frames, including the raw `0x88 LOG_RX_DATA`
  frames used for the channel-reliability early-stop.
- **Sending** — DMs (`SendMessageWithRetry`, ack→no-ack fallback) and channel
  messages (`SendChannelMessageWithRetry`, multi-transmission with adaptive
  early-stop once the mesh rebroadcasts the packet).
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
- `CreateClient` / `CreateSerialClient` log and panic when the transport itself
  cannot be established; configuration errors are the caller's to check.
- `SetLogger` swaps the driver's logger (default: `log` package).

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

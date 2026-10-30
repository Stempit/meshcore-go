# meshcore-go 📡🔧

**Go-драйвер для LoRa-устройств MeshCore — BLE, serial или TCP, от протокола до API.**

*[🇬🇧 English version](README.md)*

`meshcore-go` подключается к LoRa-устройству [MeshCore](https://meshcore.io)
из Go, говорит по протоколу companion-устройства через **Bluetooth LE** (по
умолчанию), **USB/UART serial** или **TCP** (прошивка companion с WiFi или
Ethernet отдаёт тот же протокол на TCP-порту 5000) и отдаёт клиент с
контактами, каналами, историей сообщений и отправкой с подтверждением
доставки. Это радио-слой семейства meshcore — боты, HTTP-шлюзы и голосовые
мосты живут в отдельных проектах поверх этого драйвера.

## Установка

```sh
go get github.com/stempit/meshcore-go
```

**Требования:** Go по версии из `go.mod`. Для BLE нужен Bluetooth-адаптер;
для serial — доступ к устройству (в Linux добавьте себя в группу `dialout`,
например `sudo usermod -aG dialout $USER`). В Linux BLE-сканированию нужен
`CAP_NET_RAW` — запускайте через `sudo` или выдайте бинарнику один раз:

```sh
sudo setcap cap_net_raw,cap_net_admin+eip ./your-app
```

## Быстрый старт

См. [examples/basic](examples/basic/main.go): подключение по BLE, отправка DM
с ретраями, чтение сообщений, хранящихся на устройстве, обработка живых
сообщений и автоответ.

```go
client := meshcore.CreateClient() // BLE-скан (по умолчанию)
// client := meshcore.CreateSerialClient("/dev/ttyUSB0", 115200)
// client := meshcore.CreateTCPClient("192.168.1.50:5000")
defer client.Disconnect()

client.SendMessageWithRetry(contact, "привет", true, 3, 3, 3*time.Second)
```

### Выбор BLE-устройства

BLE-скан принимает любое устройство с именем на префикс `MeshCore-`. Когда в
эфире несколько MeshCore-устройств, скан продолжает работать, пока не соберёт
их все, затем берёт устройство с самым сильным сигналом и пишет в лог всех
кандидатов. Чтобы подключаться к конкретному устройству, передайте опцию:

```go
client := meshcore.CreateClient(meshcore.WithDeviceName("MeshCore-AB12"))
// или по платформенному адресу — MAC в Linux/Windows, CoreBluetooth UUID в macOS:
client := meshcore.CreateClient(meshcore.WithDeviceAddress("AA:BB:CC:DD:EE:FF"))
```

Запустите [examples/scan](examples/scan/main.go), чтобы увидеть устройства в
эфире с их именами, адресами и уровнем сигнала; `ScanDevices()` возвращает
тот же список программно.

## Что умеет

- **Транспорт** — BLE ([tinygo.org/x/bluetooth](https://github.com/tinygo-org/bluetooth)),
  serial ([go.bug.st/serial](https://github.com/bugst/go-serial)) или TCP. Все
  три реализуют один внутренний интерфейс `Connection`; потоковые транспорты
  (serial, TCP) делят фирменное `<`/`>`-обрамление прошивки.
- **Кадры/парсинг** — декодирует кадры устройства, включая сырые
  `0x88 LOG_RX_DATA`-кадры, на которых построен ранний стоп повторных передач
  для каналов.
- **Отправка** — DM (`SendMessageWithRetry`, фолбэк ack→no-ack) и канальные
  сообщения (`SendChannelMessageWithRetry`, несколько передач с адаптивным
  ранним стопом, когда меш ретранслировал пакет).
- **Адверты / контакты** — собственный адверт, список контактов, поток
  адвертов, ключи верификации контактов.
- **Нечёткий поиск имён** — `FindContactByNameLoose` и companions: поиск,
  прощающий регистр, эмодзи и пунктуацию, плюс фонетическое сопоставление
  кириллица↔латиница (то, что нужно голосовым фронтендам).
- **История** — накопление сообщений с инжектируемым `HistoryLogger`.
- **Хук зеркалирования** — реализуйте `PacketPublisher` и подключите его,
  чтобы зеркалировать сырые пакеты (например, в MQTT); сам драйвер не зависит
  от брокера.

## Заметки

- Канальные (групповые) сообщения MeshCore **не имеют ACK о доставке**; `OK`
  устройства для канальной отправки подтверждает только *линк*. Надёжность —
  несколько передач с ранним стопом, см. доки `SendChannelMessageWithRetry`.
- В каждый момент времени возможна только одна отправка; конкуррентные
  отправители должны сериализоваться (диспетчер бота делает это через
  сериализованный отправитель).
- `CreateClient` / `CreateSerialClient` / `CreateTCPClient` логируют и
  паникуют, если транспорт не поднялся; ошибки конфигурации — забота
  вызывающего.
- TCP: на устройстве должна быть прошивка companion с WiFi или Ethernet
  (`-D ENABLE_WIFI_INTERFACE` или Ethernet-плата). Протокол companion
  отдаётся на TCP-порту **5000** (`TCP_PORT` / `ETHERNET_TCP_PORT`),
  устройство принимает **одного клиента** — новое подключение молча заменяет
  предыдущее. TLS/авторизации нет: ограничивайте доступ на уровне сети.
  (Ethernet-сборки с `ETHERNET_RAW_LINE=1` говорят построчным CLI и этим
  драйвером не поддерживаются.)
- `SetLogger` заменяет логгер драйвера (по умолчанию: пакет `log`).

## Родственные проекты

- [meshcore-kit](https://github.com/stempit/meshcore-kit) — общий
  рантайм приложений поверх драйвера: выбор подключения, стартовая синхронизация
  сохранённых сообщений, живой инбокс с курсорами, keepalive/реконнект — плюс
  упаковщик сообщений в 140 байт.
- [meshcore-go-bot](https://github.com/stempit/meshcore-go-bot) — полноценный чат-бот в меше
  (команды, игры, мосты Telegram/MQTT).
- [meshcore-http-gateway](https://github.com/stempit/meshcore-http-gateway) —
  меш за простым HTTP API.
- [meshcore-mcp-gateway](https://github.com/stempit/meshcore-mcp-gateway) —
  меш как голосовые инструменты для ассистента XiaoZhi.

Прошивка, с которой говорит драйвер, — это
[MeshCore](https://github.com/meshcore-dev/MeshCore).

## Лицензия

MIT — см. [LICENSE](LICENSE).

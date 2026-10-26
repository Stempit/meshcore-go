# meshcore-go 📡🔧

**Go-драйвер для LoRa-устройств MeshCore — BLE или serial, от протокола до API.**

*[🇬🇧 English version](README.md)*

`meshcore-go` подключается к LoRa-устройству [MeshCore](https://meshcore.io)
из Go, говорит по протоколу companion-устройства через **Bluetooth LE** (по
умолчанию) или **USB/UART serial** и отдаёт клиент с контактами, каналами,
историей сообщений и отправкой с подтверждением доставки. Это радио-слой
семейства meshbot — боты, HTTP-шлюзы и голосовые мосты живут в отдельных
проектах поверх этого драйвера.

## Установка

```sh
go get github.com/stempit/meshcore-go
```

## Быстрый старт

См. [examples/basic](examples/basic/main.go): подключение по BLE, отправка DM
с ретраями, чтение сообщений, хранящихся на устройстве, обработка живых
сообщений и автоответ.

```go
client := meshcore.CreateClient() // BLE-скан (по умолчанию)
// client := meshcore.CreateSerialClient("/dev/ttyUSB0", 115200)
defer client.Disconnect()

client.SendMessageWithRetry(contact, "привет", true, 3, 3, 3*time.Second)
```

## Что умеет

- **Транспорт** — BLE ([tinygo.org/x/bluetooth](https://github.com/tinygo-org/bluetooth))
  или serial ([go.bug.st/serial](https://github.com/bugst/go-serial)).
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
- `CreateClient` / `CreateSerialClient` логируют и паникуют, если транспорт
  не поднялся; ошибки конфигурации — забота вызывающего.
- `SetLogger` заменяет логгер драйвера (по умолчанию: пакет `log`).

## Родственные проекты

- [meshcore-kit](https://github.com/stempit/meshcore-kit) — общий
  рантайм приложений поверх драйвера: выбор подключения, стартовая синхронизация
  сохранённых сообщений, живой инбокс с курсорами, keepalive/реконнект — плюс
  упаковщик сообщений в 140 байт.
- [meshbot](https://github.com/stempit/meshbot) — полноценный чат-бот в меше
  (команды, игры, мосты Telegram/MQTT).
- [meshcore-http-gateway](https://github.com/stempit/meshcore-http-gateway) —
  меш за простым HTTP API.
- [meshcore-mcp-gateway](https://github.com/stempit/meshcore-mcp-gateway) —
  меш как голосовые инструменты для ассистента XiaoZhi.

Прошивка, с которой говорит драйвер, — это
[MeshCore](https://github.com/meshcore-dev/MeshCore).

## Лицензия

MIT — см. [LICENSE](LICENSE).

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

### CLI-команды устройства

Прошивка companion v14+ исполняет ASCII-CLI устройства поверх бинарного
протокола (`CMD_RUN_CLI_COMMAND`): `RunCLICommand(cmd)` отправляет одну
команду и возвращает текстовый ответ устройства — `ver`,
`get/set radio ...`, `power`, `reboot` и всё остальное из
`docs/cli_commands.md` прошивки.

```go
reply, err := client.RunCLICommand("get tx")
```

Ответы `get` начинаются с `"> "`, успешный `set` отвечает `OK` (или
`OK - ...`), ошибка — `Error, ...` — они возвращаются как строка ответа, а не
как Go-error; `*DeviceError` (например, `unsupported_cmd`) означает прошивку
старше v14. [examples/cli](examples/cli/main.go) работает как разовый
исполнитель команд или интерактивный терминал:

```sh
go run ./examples/cli -serial /dev/ttyUSB0 ver "get tx"
```

(Команды из нескольких слов берите в кавычки — `"get tx"`, а не `get tx`,
иначе шелл разобьёт их на отдельные команды.)

### Администрирование контактов и каналов

Драйвер умеет управлять тем, что хранит устройство: контактами (chat,
repeater, room server, sensor) и каналами (групповые чаты). Каждый вызов
ждёт ответа устройства: успех — `nil`/результат, отказ — `*DeviceError`
(коды прошивки `ERR_CODE_*`: `table_full`, `not_found`, `unsupported_cmd`,
`illegal_arg`), молчание устройства — `ErrNoResponse`.

**Контакты.** Полный 32-байтный публичный ключ берётся из advert
(`OnAdvert`), `GetContactByKey` или QR-кода
(`meshcore://contact/add?public_key=<64 hex>&type=<1-4>`, см.
`docs/qr_codes.md` прошивки):

```go
// репитер, найденный по эфирному advert или из QR-кода
err := client.AddContact(&meshcore.Contact{
    PublicKey: key,                // ровно 32 байта (обязательно)
    Name:      "Repeater-1",       // обрезается до 31 символа, как в прошивке
    Type:      meshcore.AdvTypeRepeater, // Chat=1, Repeater=2, Room=3, Sensor=4
    Flags:     meshcore.ContactFlagFavorite, // битовая маска; 0 = без флагов
})

ct, err := client.GetContactByKey(key) // прочитать контакт обратно (кэш + колбэк)
err = client.RemoveContact(contact)    // удалить; *DeviceError not_found, если нет
```

`AddContact` и добавляет новый контакт, и обновляет существующий — прошивка
ищет запись по полному ключу. `table_full` — хранилище устройства заполнено:
удалите неактивный контакт и повторите.

**Каналы.** На уровне протокола «войти в канал» — это записать его в
свободный слот (`CMD_SET_CHANNEL`); отдельной команды join нет. Драйвер сам
выбирает первый свободный слот и возвращает заполненный `Channel`:

```go
// создать приватный канал: случайный 16-байтный ключ, раздайте участникам
ch, err := client.CreatePrivateChannel("Team")
qr := fmt.Sprintf("meshcore://channel/add?name=%s&secret=%s",
    url.QueryEscape(ch.Name), hex.EncodeToString(ch.PSK))

// войти в приватный канал по QR / присланному ключу
secret, _ := hex.DecodeString("<32 hex-символов из QR>")
ch, err = client.AddChannel("Team", secret) // PSK ровно 16 байт

// стандартный публичный канал и хештег-канал
client.JoinPublicChannel("")        // алиас по умолчанию "Public" (PSK прошивки)
client.JoinHashtagChannel("#test")  // ключ = SHA256("#test")[:16], совместимо с приложением

client.RemoveChannel(ch.Index) // освободить слот (пустое имя + нулевой ключ)
```

Что стоит знать:

- Ключи каналов — ровно **16 байт** (128 бит; 256-битные прошивка отвергает),
  имена каналов и контактов обрезаются до **31 символа** — те же лимиты, что
  у официальных приложений.
- `JoinHashtagChannel` выводит ключ на клиенте (первые 16 байт
  `SHA256("#имя")`) — прошивка о хештегах не знает, поэтому каналы,
  созданные так, совместимы с официальными приложениями. Ведущий `#`
  можно опустить — каноничное имя хранится с ним.
- `OnContactDeleted(func(pubKey []byte))` срабатывает, когда контакт удалило
  само **устройство** (push `0x8F`, вытеснение при переполнении хранилища) —
  удобно, чтобы синхронизировать базу бота. Ваши вызовы `RemoveContact` этот
  колбэк не вызывают — их результат виден в возвращённой ошибке.
- Эти вызовы ждут ответа устройства, поэтому не отправляйте сообщения, пока
  один из них в полёте (то же правило «по одной команде», что и у
  `RunCLICommand`).
- Проверить, что применилось: `GetContactByKey`, `GetChannels()` /
  `RequestChannels(n)`, `RequestContacts(0)`.

**Списки со всеми деталями.** `ListContacts()` / `ListChannels()` сначала
запрашивают актуальное состояние устройства (полная итерация `GET_CONTACTS` /
опрос `GET_CHANNEL` по всем слотам) и возвращают полные записи — публичные
ключи, PSK, пути, метки времени — как глубокие копии: их можно менять,
сериализовать и отдавать в UI, не задевая кэш драйвера:

```go
contacts, err := client.ListContacts() // по имени, свежие данные с устройства
for _, ct := range contacts {
    fmt.Printf("%-16s %-8s %s\n", ct.Name, ct.TypeName(), ct.PublicKeyHex())
}

channels, err := client.ListChannels() // все слоты по индексу, пустые тоже
for _, ch := range channels {
    if ch.IsEmpty {
        continue
    }
    fmt.Printf("[%d] %-20s psk %s hash %02x\n",
        ch.Index, ch.Name, ch.PSKHex(), ch.HashByte())
}
```

Те же производные хелперы работают на любом значении `Contact`/`Channel`,
включая полученные из `GetContacts()` / `GetChannelByName()`: `PublicKeyHex()`,
`TypeName()` (chat/repeater/room/sensor), `PSKHex()` и `HashByte()` —
1-байтовый хеш канала, которым прошивка адресует групповые пакеты (удобно для
корреляции сырых `0x88`-кадров). `GetContacts()`/`GetChannels()` остаются
дешёвыми читателями кэша (общие указатели, без обращения к устройству);
`List*` — когда данные должны быть актуальными.

[examples/contacts-channels](examples/contacts-channels/main.go) показывает
весь сценарий на реальном устройстве (создать/войти, напечатать, убрать за
собой):

```sh
go run ./examples/contacts-channels -serial /dev/ttyUSB0
```

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
- **CLI устройства** — `RunCLICommand` исполняет ASCII-CLI-команды устройства
  (прошивка v14+) и возвращает текст ответа.
- **Администрирование контактов и каналов** — добавление/удаление контактов
  всех типов (chat, repeater, room server, sensor), чтение по ключу,
  создание приватных каналов, вход в публичные/хештег-каналы, освобождение
  слотов каналов (см. раздел выше).
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
- Слоты подтверждения не различают команды: пока отправка ждёт своего
  подтверждения, чужой OK/ошибка — включая завершающие OK стартового
  handshake — может закрыть её ошибочно. Других команд во время отправки не
  отправлять. `RunCLICommand` сериализует команды устройства между собой
  (отдельный мьютекс, никогда не удерживается на ретраях отправки), но то же
  правило действует и наоборот: во время ожидания ответа CLI-команды
  отправок не делать.
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

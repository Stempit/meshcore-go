package meshcore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// defaultCommandTimeout is how long a synchronous device command waits for
// its reply. CLI replies are device-local (no radio round trip involved), so
// a few seconds is generous.
const defaultCommandTimeout = 5 * time.Second

// ErrNoResponse is returned when the device does not answer a synchronous
// command within the timeout window (or the connection was lost while
// waiting). The command may still have reached the device.
var ErrNoResponse = fmt.Errorf("no response from device")

// DeviceError is a RESP_CODE_ERR reply to a synchronous command. Code is one
// of the firmware ERR_CODE_* values, e.g. unsupported_cmd for running the CLI
// on firmware older than v14.
type DeviceError struct {
	Code byte
}

// Error implements error, rendering the code with its firmware name.
func (e *DeviceError) Error() string {
	return fmt.Sprintf("device error %d (%s)", e.Code, errCodeName(e.Code))
}

// runCommand writes a command frame and waits for the expected response code,
// converting a RESP_CODE_ERR reply into *DeviceError. It is the shared
// request/response core for synchronous device commands (RunCLICommand now,
// typed commands later). Replies are matched by code — the wire protocol has
// no sequence numbers — so while a command waits, an in-flight message send
// can still misroute an OK/error frame into its own confirmation slot; issue
// no sends while a command is in flight (same rule the confirm slots
// document in reverse).
func (c *MeshCoreClient) runCommand(frame []byte, expect byte, timeout time.Duration) ([]byte, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	ch := make(chan []byte, 1)
	c.armResp(expect, ch)
	// Every path below returns exactly once, so one deferred cleanup covers
	// them all; cmdMu is held until return, so no newer command can race the
	// entry out from under the ownership check in clearResp.
	defer c.clearResp(expect, ch)
	c.mu.RLock()
	handlersDone := c.handlersDone
	c.mu.RUnlock()

	if err := c.write(frame); err != nil {
		return nil, err
	}

	select {
	case data := <-ch:
		if len(data) > 0 && data[0] == respCodeErr {
			var code byte
			if len(data) >= 2 {
				code = data[1]
			}
			err := &DeviceError{Code: code}
			// While a command waits, this branch can consume an unrelated
			// device error — e.g. a concurrent send's rejection — that would
			// otherwise reach notifyErr (the documented interleaving hazard).
			// Log it so the routing stays visible.
			c.logger.Debug("Device error reply for command",
				"expect", fmt.Sprintf("0x%02x", expect), "err", err)
			return nil, err
		}
		return data, nil
	case <-handlersDone:
		// The frame handler exited (disconnect or reconnect generation swap):
		// no reply can arrive on this connection generation anymore.
		return nil, fmt.Errorf("%w: connection lost while waiting for 0x%02x", ErrNoResponse, expect)
	case <-time.After(timeout):
		return nil, fmt.Errorf("%w: waiting for 0x%02x", ErrNoResponse, expect)
	}
}

// RunCLICommand отправляет текстовую команду ASCII-CLI подключённому
// устройству (CMD_RUN_CLI_COMMAND, прошивка v14+) и возвращает ответ
// устройства одной строкой. Команды и форматы ответов описаны в
// docs/cli_commands.md прошивки: get-команды отвечают "> <значение>",
// успешный set — "OK" или "OK - ...", ошибка — "Error, ..." / "Err - ..."
// (ошибки CLI возвращаются как строка, а не как error — разбор остаётся за
// вызывающим). Ответ длиннее 166 байт устройство обрезает. На прошивке
// старше v14 устройство отвечает ошибкой unsupported_cmd (*DeviceError).
//
// Примеры: "ver", "get tx", "set radio 869.618,250,9,5", "reboot".
func (c *MeshCoreClient) RunCLICommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", fmt.Errorf("empty CLI command")
	}
	// Firmware gates CMD_RUN_CLI_COMMAND with a minimum frame length: a
	// 1-character command would only surface as a misleading unsupported_cmd.
	if len(cmd) < 2 {
		return "", fmt.Errorf("CLI command too short (min 2 characters)")
	}
	if len(cmd) > maxCliCommandLen {
		return "", fmt.Errorf("CLI command too long (%d bytes, max %d)", len(cmd), maxCliCommandLen)
	}
	data, err := c.runCommand(cliCommandFrame(cmd), respCodeCliReply, defaultCommandTimeout)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data[1:]), " \t\r\n"), nil
}

// GetDeviceTime запрашивает время RTC устройства (CMD_GET_DEVICE_TIME,
// ответ RESP_CODE_CURR_TIME) и возвращает его в UTC. Ответ — 4 байта
// unix-секунд little-endian; ответ короче — это ошибка, а не нулевое время.
// Расхождение с хостом видно сравнением с time.Now(): устройство живёт по
// своему RTC.
func (c *MeshCoreClient) GetDeviceTime() (time.Time, error) {
	return c.getDeviceTime(defaultCommandTimeout)
}

// getDeviceTime — ядро GetDeviceTime с настраиваемым таймаутом: SyncTime
// опрашивает часы коротким таймаутом, чтобы рукопожатие на прошивке без
// CMD_GET_DEVICE_TIME не зависало на полный defaultCommandTimeout.
func (c *MeshCoreClient) getDeviceTime(timeout time.Duration) (time.Time, error) {
	data, err := c.runCommand([]byte{cmdGetDeviceTime}, respCodeCurrTime, timeout)
	if err != nil {
		return time.Time{}, err
	}
	if len(data) < 5 {
		return time.Time{}, fmt.Errorf("short CURR_TIME reply (%d bytes)", len(data))
	}
	return time.Unix(int64(binary.LittleEndian.Uint32(data[1:5])), 0).UTC(), nil
}

// ============================================================================
// Администрирование контактов
// ============================================================================

// runOK отправляет фрейм и ждёт RESP_CODE_OK — общий хвост команд
// администрирования, ответ которых не несёт данных.
func (c *MeshCoreClient) runOK(frame []byte) error {
	_, err := c.runCommand(frame, respCodeOk, defaultCommandTimeout)
	return err
}

// AddContact записывает контакт в хранилище устройства (CMD_ADD_UPDATE_CONTACT)
// и ждёт подтверждения. Один метод и добавляет новый контакт, и обновляет
// существующий: прошивка ищет запись по полному 32-байтному pubkey
// (MyMesh.cpp, lookupContactByPubKey).
//
// Обязательны PublicKey (полные 32 байта — из OnAdvert, GetContactByKey или
// QR meshcore://contact/add?...) и Name (обрезается до 31 символа, как в
// прошивке). Type — один из AdvType*, Flags — маска ContactFlag* (0 = без
// флагов). Path/PathLength можно не заполнять (пустой путь = flood-маршрут),
// Latitude/Longitude опциональны; lastmod прошивка проставит по своим часам.
//
// Ответ OK — контакт сохранён; ERR_CODE_TABLE_FULL (*DeviceError) — хранилище
// устройства заполнено: освободите слот через RemoveContact и повторите.
// После подтверждения контакт кладётся в локальный кэш; onContact при этом
// НЕ вызывается — этот колбэк означает фрейм, пришедший от устройства.
func (c *MeshCoreClient) AddContact(contact *Contact) error {
	if contact == nil {
		return fmt.Errorf("contact is nil")
	}
	frame, err := buildUpdateContactFrame(contact)
	if err != nil {
		return err
	}
	if err := c.runOK(frame); err != nil {
		return err
	}
	// Устройство подтвердило запись; push в ответ на ADD_UPDATE_CONTACT
	// прошивка не шлёт, поэтому кэш обновляем локально.
	stored := *contact
	c.mu.Lock()
	c.contacts[pubKeyHex(contact.PublicKey[:pubKeySize])] = &stored
	c.mu.Unlock()
	return nil
}

// RemoveContact удаляет контакт из хранилища устройства (CMD_REMOVE_CONTACT)
// и из локального кэша, ждёт ответа устройства: OK либо *DeviceError
// (ERR_CODE_NOT_FOUND — контакта с таким ключом нет). OnContactDeleted при
// этом не срабатывает: колбэк означает, что устройство удалило контакт само
// (push 0x8F при вытеснении при переполнении хранилища).
func (c *MeshCoreClient) RemoveContact(contact *Contact) error {
	if contact == nil {
		return fmt.Errorf("contact is nil")
	}
	frame, err := buildRemoveContactFrame(contact.PublicKey)
	if err != nil {
		return err
	}
	if err := c.runOK(frame); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.contacts, pubKeyHex(contact.PublicKey[:pubKeySize]))
	c.mu.Unlock()
	return nil
}

// GetContactByKey запрашивает контакт по полному 32-байтному ключу
// (CMD_GET_CONTACT_BY_KEY). Ответ — полный RESP_CODE_CONTACT, тот же фрейм,
// что и при синхронизации: он разбирается обычным parseContact, поэтому кэш
// обновляется и onContact срабатывает, как для любого фрейма от устройства.
// Ошибка NOT_FOUND приходит как *DeviceError. Наружу уходит глубокая копия,
// как и в ListContacts: V3-кадры обновляют кэшированные контакты на месте
// (parseContactMessageV3), поэтому живой указатель гонялся бы с
// горутиной-обработчиком кадров.
func (c *MeshCoreClient) GetContactByKey(pubKey []byte) (*Contact, error) {
	frame, err := buildGetContactByKeyFrame(pubKey)
	if err != nil {
		return nil, err
	}
	data, err := c.runCommand(frame, respCodeContact, defaultCommandTimeout)
	if err != nil {
		return nil, err
	}
	c.parseContact(data)
	c.mu.RLock()
	cached := c.contacts[pubKeyHex(pubKey[:pubKeySize])]
	c.mu.RUnlock()
	if cached == nil {
		return nil, nil
	}
	return copyContact(cached), nil
}

// ============================================================================
// Администрирование каналов
// ============================================================================

// ErrNoEmptyChannelSlot возвращается, когда на устройстве не осталось
// свободных слотов каналов и AddChannel некуда записать новый.
var ErrNoEmptyChannelSlot = errors.New("no empty channel slot on device")

// SetChannel записывает слот канала по индексу (CMD_SET_CHANNEL): имя
// (обрезается до 31 символа) и PSK ровно 16 байт — 256-битные ключи прошивка
// отвергает (ERR_CODE_UNSUPPORTED_CMD), короткие фрейм не распознаёт, поэтому
// длина проверяется на клиенте. Ждёт ответа устройства: OK либо *DeviceError
// (ERR_CODE_NOT_FOUND — неверный индекс). После подтверждения слот обновляется
// в локальном кэше.
//
// Прямое использование — точечная правка конкретного слота; чтобы просто
// «войти в канал», берите AddChannel / CreatePrivateChannel /
// JoinPublicChannel / JoinHashtagChannel.
func (c *MeshCoreClient) SetChannel(channelIndex int, name string, psk []byte) error {
	frame, err := buildSetChannelFrame(byte(channelIndex), name, psk)
	if err != nil {
		return err
	}
	if err := c.runOK(frame); err != nil {
		return err
	}
	ch := &Channel{
		Index:   channelIndex,
		Name:    name,
		PSK:     append([]byte(nil), psk...),
		IsEmpty: name == "",
	}
	c.mu.Lock()
	c.channels[channelIndex] = ch
	c.mu.Unlock()
	return nil
}

// AddChannel — общий примитив «войти в канал»: записывает канал с заданным
// именем и 16-байтным PSK в первый свободный слот устройства и возвращает
// заполненный Channel. Отдельной команды «join» в протоколе нет — вход в
// канал и есть CMD_SET_CHANNEL в свободный слот.
//
// PSK берётся по сценарию: вход в приватный канал — секрет из QR
// meshcore://channel/add?name=...&secret=<32 hex-символа> или от другого
// участника; публичный канал — JoinPublicChannel (или PublicChannelPSK);
// хештег — JoinHashtagChannel. Свободных слотов нет — ErrNoEmptyChannelSlot;
// устройство ответило ошибкой — *DeviceError.
func (c *MeshCoreClient) AddChannel(name string, psk []byte) (*Channel, error) {
	index, err := c.findEmptyChannelSlot()
	if err != nil {
		return nil, err
	}
	if err := c.SetChannel(index, name, psk); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channels[index], nil
}

// CreatePrivateChannel создаёт приватный канал: генерирует случайный
// 16-байтный PSK (crypto/rand) и записывает его в свободный слот. PSK
// созданного канала нужно передать участникам — например, QR-кодом
// meshcore://channel/add?name=...&secret=<32 hex-символа> (docs/qr_codes.md).
func (c *MeshCoreClient) CreatePrivateChannel(name string) (*Channel, error) {
	psk := make([]byte, 16)
	if _, err := rand.Read(psk); err != nil {
		return nil, fmt.Errorf("generating channel PSK: %w", err)
	}
	return c.AddChannel(name, psk)
}

// JoinPublicChannel записывает публичный канал MeshCore в свободный слот.
// PSK фиксирован прошивкой (PUBLIC_GROUP_PSK, MyMesh.cpp) и экспортирован как
// PublicChannelPSK. Имя — алиас для отображения; пустое заменяется на
// "Public", как канал называет сама прошивка при загрузке. Слот 0 обычно уже
// занят этим же каналом — метод нужен после RemoveChannel или на устройствах
// без дефолтного слота.
func (c *MeshCoreClient) JoinPublicChannel(name string) (*Channel, error) {
	if strings.TrimSpace(name) == "" {
		name = "Public"
	}
	return c.AddChannel(name, PublicChannelPSK)
}

// JoinHashtagChannel входит в хештег-канал: PSK выводится из имени как
// первые 16 байт SHA256("#"+name). Это клиентская конвенция (официальное
// приложение, companion_protocol.md «Hashtag channels») — прошивка про
// хештеги не знает и различает каналы только по хешу PSK, поэтому каналы,
// созданные этим методом, совместимы с официальным приложением. Ведущий '#'
// можно опустить; каноничное имя канала хранится с '#'.
func (c *MeshCoreClient) JoinHashtagChannel(name string) (*Channel, error) {
	tag := strings.TrimPrefix(strings.TrimSpace(name), "#")
	if tag == "" {
		return nil, fmt.Errorf("hashtag channel name is empty")
	}
	return c.AddChannel("#"+tag, hashtagChannelPSK(tag))
}

// hashtagChannelPSK выводит ключ хештег-канала: первые 16 байт
// SHA256("#"+tag). Документированный вектор: #test →
// 9cd8fcf22a47333b591d96a2b848b73f (companion_protocol.md).
func hashtagChannelPSK(tag string) []byte {
	sum := sha256.Sum256([]byte("#" + tag))
	return sum[:16]
}

// RemoveChannel «удаляет» канал с индексом index: отдельной команды в
// протоколе нет, слот перезаписывается пустым именем и нулевым PSK
// (companion_protocol.md, «Deleting a channel»). Пустое имя обязательно:
// именованный слот с нулевым PSK продолжал бы ловить незашифрованный
// null-key трафик.
func (c *MeshCoreClient) RemoveChannel(index int) error {
	return c.SetChannel(index, "", make([]byte, 16))
}

// channelSlotCount — сколько слотов каналов запрашивать: maxChannels из
// CMD_DEVICE_QUERY (MAX_GROUP_CHANNELS); когда он неизвестен (прошивка < fw3),
// берётся 8 — как в стартовой синхронизации.
func (c *MeshCoreClient) channelSlotCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.maxChannels > 0 {
		return c.maxChannels
	}
	return 8
}

// ensureChannelsSynced досинхронизирует кэш каналов: слоты 0..limit-1,
// которых в кэше нет, запрашиваются CMD_GET_CHANNEL — прошивка отвечает
// ChannelInfo и для пустых слотов (BaseChatMesh::getChannel возвращает true
// для любого idx < MAX_GROUP_CHANNELS).
func (c *MeshCoreClient) ensureChannelsSynced() error {
	limit := c.channelSlotCount()
	for i := 0; i < limit; i++ {
		c.mu.RLock()
		_, known := c.channels[i]
		c.mu.RUnlock()
		if known {
			continue
		}
		data, err := c.runCommand([]byte{cmdGetChannel, byte(i)}, respCodeChannelInfo, defaultCommandTimeout)
		if err != nil {
			// NOT_FOUND на слот внутри диапазона означает, что слотов меньше,
			// чем сообщил DeviceInfo, — дальше запрашивать бессмысленно.
			var devErr *DeviceError
			if errors.As(err, &devErr) && devErr.Code == errCodeNotFound {
				return nil
			}
			return err
		}
		c.parseChannelInfo(data)
	}
	return nil
}

// findEmptyChannelSlot возвращает индекс первого свободного слота каналов.
func (c *MeshCoreClient) findEmptyChannelSlot() (int, error) {
	if err := c.ensureChannelsSynced(); err != nil {
		return 0, err
	}
	limit := c.channelSlotCount()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := 0; i < limit; i++ {
		if ch, ok := c.channels[i]; ok && ch.IsEmpty {
			return i, nil
		}
	}
	return 0, ErrNoEmptyChannelSlot
}

// ============================================================================
// Списки контактов и каналов
// ============================================================================

// listContactsBuffer — ёмкость буфера кадров RESP_CODE_CONTACT при листинге.
// Прошивка отвечает за один проход не больше MAX_CONTACTS контактов
// (MyMesh.h, до ~350 + 8 анонимных слотов, которые не экспортируются), с
// запасом — чтобы ни один кадр не был выброшен переполненным буфером
// (deliverResp не блокируется, лишний кадр теряется).
const listContactsBuffer = 512

// ListContacts запрашивает у устройства полный список контактов
// (CMD_GET_CONTACTS без since) и возвращает его, дождавшись
// END_OF_CONTACTS. Каждый RESP_CODE_CONTACT разбирается обычным parseContact,
// поэтому кэш и onContact живут как при любой синхронизации; наружу уходят
// глубокие копии присланного списка, упорядоченные по имени — их можно менять
// и сериализовать, не задевая кэш драйвера. Возвращается ровно то, что
// устройство прислало в этой итерации; записи кэша, которых в ответе не было,
// не удаляются. Ошибка устройства (например, bad_state — когда итерация
// контактов уже запущена параллельным RequestContacts) приходит как
// *DeviceError; пока список идёт, других команд не отправлять.
func (c *MeshCoreClient) ListContacts() ([]*Contact, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	contactCh := make(chan []byte, listContactsBuffer)
	endCh := make(chan []byte, 1)
	// Порядок armResp важен для respCodeErr: ошибка устройства маршрутизируется
	// в канал последней вооружённой команды — endCh разбирает и ERR, и END.
	c.armResp(respCodeContact, contactCh)
	c.armResp(respCodeEndOfContacts, endCh)
	defer func() {
		c.clearResp(respCodeContact, contactCh)
		c.clearResp(respCodeEndOfContacts, endCh)
	}()

	c.mu.RLock()
	handlersDone := c.handlersDone
	c.mu.RUnlock()

	if err := c.write([]byte{cmdGetContacts}); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	// collect прогоняет один CONTACT-кадр через обычный parseContact и
	// запоминает ключ — тело общее для основного цикла и дочитывания после END.
	collect := func(data []byte) {
		c.parseContact(data)
		if len(data) >= 1+pubKeySize {
			seen[pubKeyHex(data[1:1+pubKeySize])] = struct{}{}
		}
	}
	for {
		select {
		case data := <-contactCh:
			collect(data)
		case data := <-endCh:
			if len(data) > 0 && data[0] == respCodeErr {
				var code byte
				if len(data) >= 2 {
					code = data[1]
				}
				return nil, &DeviceError{Code: code}
			}
			// END_OF_CONTACTS — итерация устройства завершена; END идёт
			// последним кадром, но контактные кадры из буфера contactCh могли
			// быть ещё не дочитаны (select не даёт порядка) — дочитываем до
			// пуста, новых уже не придёт.
			for {
				select {
				case data := <-contactCh:
					collect(data)
				default:
					return c.contactsByKeys(seen), nil
				}
			}
		case <-handlersDone:
			return nil, fmt.Errorf("%w: connection lost during contact listing", ErrNoResponse)
		case <-time.After(defaultCommandTimeout):
			return nil, fmt.Errorf("%w: waiting for the end of the contact list", ErrNoResponse)
		}
	}
}

// contactsByKeys собирает глубокие копии кэшированных контактов по их
// hex-ключам, упорядоченные по имени (при одинаковых именах — по ключу).
func (c *MeshCoreClient) contactsByKeys(keys map[string]struct{}) []*Contact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]*Contact, 0, len(keys))
	for key := range keys {
		if ct, ok := c.contacts[key]; ok {
			res = append(res, copyContact(ct))
		}
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Name != res[j].Name {
			return res[i].Name < res[j].Name
		}
		return res[i].PublicKeyHex() < res[j].PublicKeyHex()
	})
	return res
}

// ListChannels возвращает все слоты каналов устройства, досинхронизировав кэш
// (CMD_GET_CHANNEL по слотам): имя, ключ и его хеш — всё, что нужно, чтобы
// показать канал или поделиться им. Слоты упорядочены по индексу, пустые
// (IsEmpty) включены — администрированию видно и свободные места. Возвращаются
// глубокие копии: кэш драйвера они не задевают.
func (c *MeshCoreClient) ListChannels() ([]*Channel, error) {
	if err := c.ensureChannelsSynced(); err != nil {
		return nil, err
	}
	limit := c.channelSlotCount()
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]*Channel, 0, len(c.channels))
	for i := 0; i < limit; i++ {
		if ch, ok := c.channels[i]; ok {
			res = append(res, copyChannel(ch))
		}
	}
	return res, nil
}

// copyContact возвращает глубокую копию контакта — снимок для наружу.
func copyContact(ct *Contact) *Contact {
	snapshot := *ct
	snapshot.PublicKey = append([]byte(nil), ct.PublicKey...)
	snapshot.Path = append([]byte(nil), ct.Path...)
	snapshot.PathOverrideBytes = append([]byte(nil), ct.PathOverrideBytes...)
	if ct.Latitude != nil {
		v := *ct.Latitude
		snapshot.Latitude = &v
	}
	if ct.Longitude != nil {
		v := *ct.Longitude
		snapshot.Longitude = &v
	}
	if ct.PathOverride != nil {
		v := *ct.PathOverride
		snapshot.PathOverride = &v
	}
	return &snapshot
}

// copyChannel возвращает глубокую копию канала — снимок для наружу.
func copyChannel(ch *Channel) *Channel {
	snapshot := *ch
	snapshot.PSK = append([]byte(nil), ch.PSK...)
	return &snapshot
}

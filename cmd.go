package meshcore

import (
	"encoding/binary"
	"fmt"
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
	data, err := c.runCommand([]byte{cmdGetDeviceTime}, respCodeCurrTime, defaultCommandTimeout)
	if err != nil {
		return time.Time{}, err
	}
	if len(data) < 5 {
		return time.Time{}, fmt.Errorf("short CURR_TIME reply (%d bytes)", len(data))
	}
	return time.Unix(int64(binary.LittleEndian.Uint32(data[1:5])), 0).UTC(), nil
}

package meshcore

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// meshcoreNamePrefix is the advertised local-name prefix every companion
// radio firmware uses; the scan treats it as the MeshCore marker.
const meshcoreNamePrefix = "MeshCore-"

const (
	// defaultScanTimeout bounds a BLE scan when no timeout is configured.
	defaultScanTimeout = 10 * time.Second
	// scanSettleDelay is how long the scan keeps running after the last NEW
	// device appeared, so devices that start advertising seconds apart (two
	// devices powering up together) are all discovered. Re-advertisements
	// from already-seen devices do not extend it. The cost is paid only on
	// the unfiltered path: an unfiltered connect waits out this quiet period
	// even with a single device, while a WithDeviceName/WithDeviceAddress
	// connect returns as soon as the target is seen.
	scanSettleDelay = 2 * time.Second
)

// BLEDevice describes one MeshCore device discovered by a BLE scan.
type BLEDevice struct {
	// AddrString is the platform-specific address of the device: a MAC
	// address on Linux/Windows, a CoreBluetooth UUID on macOS (UUIDs differ
	// per machine, so names are the portable identifier). This is the value
	// to pass to WithDeviceAddress.
	AddrString string
	// Name is the advertised local name, e.g. "MeshCore-AB12"; the value to
	// pass to WithDeviceName.
	Name string
	// RSSI is the strongest signal strength seen during the scan, in dBm
	// (closer to 0 is stronger).
	RSSI int16
}

// bleScanOptions narrows down which MeshCore device to connect to when
// several are advertising. Name and Address are both optional; when both are
// set a device must match both (they identify the same device). The zero
// value accepts any MeshCore device, picking the strongest signal.
type bleScanOptions struct {
	// Name is the exact advertised local name, e.g. "MeshCore-AB12"
	// (case-insensitive). Empty = any.
	Name string
	// Address is the platform address — MAC on Linux/Windows, CoreBluetooth
	// UUID on macOS (case-insensitive). Empty = any.
	Address string
	// Timeout bounds the scan window; non-positive values use
	// defaultScanTimeout.
	Timeout time.Duration
}

// applyBLEOptions folds option callbacks into a bleScanOptions value.
func applyBLEOptions(opts []BLEOption) bleScanOptions {
	var scanOpts bleScanOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&scanOpts)
		}
	}
	return scanOpts
}

// BLEOption customises how CreateClient and ScanDevices scan for and select
// a MeshCore BLE device. The option set is closed by design: behaviour is
// configured through the With* functions of this package.
type BLEOption func(*bleScanOptions)

// WithDeviceName pins the device by its exact advertised name
// (e.g. "MeshCore-AB12"): with several MeshCore devices in range the choice
// becomes deterministic, and the scan stops as soon as the named device is
// seen. Empty (default) accepts any MeshCore device.
func WithDeviceName(name string) BLEOption {
	return func(o *bleScanOptions) { o.Name = name }
}

// WithDeviceAddress pins the device by its platform address — a MAC address
// on Linux/Windows, a CoreBluetooth UUID on macOS. Run ScanDevices to see
// the values as this platform reports them.
func WithDeviceAddress(addr string) BLEOption {
	return func(o *bleScanOptions) { o.Address = addr }
}

// WithScanTimeout bounds the BLE scan window (default 10s).
func WithScanTimeout(d time.Duration) BLEOption {
	return func(o *bleScanOptions) { o.Timeout = d }
}

// ScanDevices scans for advertising MeshCore devices and returns one entry
// per device found, strongest signal first. Use it to discover the Name and
// AddrString values to configure for CreateClient. The scan ends after
// scanSettleDelay without a new device or at the timeout
// (WithScanTimeout, default 10s) — whichever comes first. With a filter
// configured the scan stops at the first match, so the returned list may be
// partial.
func ScanDevices(opts ...BLEOption) ([]BLEDevice, error) {
	scanOpts := applyBLEOptions(opts)
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable bluetooth: %w", err)
	}
	cands, _, err := scanMeshCoreCandidates(adapter, scanOpts)
	if err != nil {
		return nil, err
	}
	return cands.list(), nil
}

// scanMeshCoreCandidates runs the BLE scan and accumulates every "MeshCore-"
// advertisement. It stops the scan when the filtered target is seen (early
// exit), after scanSettleDelay without a new device, or at the timeout —
// whichever comes first. The second return value maps each candidate's
// AddrString to the platform Address value for Connect. The adapter must
// already be enabled.
func scanMeshCoreCandidates(adapter *bluetooth.Adapter, opts bleScanOptions) (bleCandidates, map[string]bluetooth.Address, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultScanTimeout
	}

	var (
		mu       sync.Mutex
		addrs    = map[string]bluetooth.Address{} // AddrString -> platform address
		cands    bleCandidates
		lastNew  time.Time // when the latest new device was first seen
		stopOnce sync.Once
	)
	stopScan := func() { stopOnce.Do(func() { adapter.StopScan() }) }

	// Adapter.Scan blocks its caller until StopScan (every host platform
	// implementation parks on the stop signal), so it runs in its own
	// goroutine: the scan must be stoppable by the coordinator below, not
	// only by the callback's filter hit.
	scanErr := make(chan error, 1)
	go func() {
		scanErr <- adapter.Scan(func(_ *bluetooth.Adapter, result bluetooth.ScanResult) {
			name := result.LocalName()
			if !strings.HasPrefix(name, meshcoreNamePrefix) {
				return
			}
			key := result.Address.String()
			mu.Lock()
			addrs[key] = result.Address
			if cands.observe(key, name, result.RSSI) {
				lastNew = time.Now()
			}
			hit := cands.countMatches(opts) > 0
			mu.Unlock()

			if hit {
				// The filtered target is deterministic — no need to keep
				// scanning for other candidates.
				stopScan()
			}
		})
	}()

	// Coordinator: end the scan when something new has been seen and gone
	// quiet for scanSettleDelay, or at the timeout; a filter hit ends it on
	// its own (Scan returns after the callback's StopScan). Polling keeps
	// all timer state on this goroutine; a new device is picked up by the
	// next tick, so no extra signalling is needed.
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	var scanFailed error
coordinator:
	for {
		select {
		case err := <-scanErr:
			// Scan returned on its own: a filter hit stopped it, or it never
			// started (e.g. another scan is already running). Its error, if
			// any, is the result; do not StopScan here — the callback was
			// never registered, so stopping could kill a foreign scan.
			scanFailed = err
			break coordinator
		case <-tick.C:
			mu.Lock()
			seen, quietSince := len(cands.byKey), lastNew
			mu.Unlock()
			if seen > 0 && time.Since(quietSince) >= scanSettleDelay {
				stopScan()
				// Join Scan so no further callback can mutate the maps.
				scanFailed = <-scanErr
				break coordinator
			}
		case <-deadline.C:
			stopScan()
			scanFailed = <-scanErr
			break coordinator
		}
	}
	if scanFailed != nil {
		return bleCandidates{}, nil, scanFailed
	}

	// Snapshot under the lock: an already-dispatched advertisement can still
	// reach the callback after Scan has returned, and the callers below read
	// the maps without the mutex. Entries are copied by value so a late
	// observe cannot mutate them either.
	mu.Lock()
	defer mu.Unlock()
	return cands.snapshot(), maps.Clone(addrs), nil
}

// scanMeshCore scans and then selects one device per opts, returning its
// platform address and advertised name.
func scanMeshCore(adapter *bluetooth.Adapter, opts bleScanOptions) (bluetooth.Address, string, error) {
	cands, addrs, err := scanMeshCoreCandidates(adapter, opts)
	if err != nil {
		return bluetooth.Address{}, "", err
	}

	dev, ambiguous, err := cands.pick(opts)
	if err != nil {
		return bluetooth.Address{}, "", err
	}
	if ambiguous {
		log().Warn("Multiple MeshCore devices found, picking the strongest signal; pass WithDeviceName/WithDeviceAddress to select one explicitly",
			"devices", cands.listString())
	}
	if matched := cands.countMatches(opts); matched > 1 {
		// Two devices can advertise the identical name (cloned firmware);
		// without this the resolution would be silent.
		log().Warn("Several MeshCore devices match the configured filter, picking the strongest signal; add WithDeviceAddress to disambiguate",
			"matches", matched, "devices", cands.listString())
	}
	log().Info("Selected device", "name", dev.Name, "address", dev.AddrString, "rssi", dev.RSSI)
	return addrs[dev.AddrString], dev.Name, nil
}

// bleCandidates accumulates MeshCore devices seen during a scan, one entry
// per address. It is the pure (hardware-free) selection layer: observe feeds
// it from the scan callback, countMatches drives the early stop and pick
// makes the final choice. Methods are not safe for concurrent use; the scan
// coordinates access itself and hands out snapshot() copies.
type bleCandidates struct {
	byKey map[string]*BLEDevice // key: AddrString
}

// observe records one advertisement. The first sighting of a device returns
// true; later ones only refresh the entry, keeping the strongest RSSI and the
// latest non-empty name (a shortened name can precede the complete one).
func (c *bleCandidates) observe(addrString, name string, rssi int16) bool {
	if c.byKey == nil {
		c.byKey = make(map[string]*BLEDevice)
	}
	if d, ok := c.byKey[addrString]; ok {
		if rssi > d.RSSI {
			d.RSSI = rssi
		}
		if name != "" {
			d.Name = name
		}
		return false
	}
	c.byKey[addrString] = &BLEDevice{AddrString: addrString, Name: name, RSSI: rssi}
	return true
}

// snapshot copies the candidates so they can be read without the scan's
// mutex: entries are duplicated by value, so a late observe cannot mutate a
// snapshot taken earlier.
func (c bleCandidates) snapshot() bleCandidates {
	out := bleCandidates{byKey: make(map[string]*BLEDevice, len(c.byKey))}
	for key, d := range c.byKey {
		dev := *d
		out.byKey[key] = &dev
	}
	return out
}

// matches reports whether the device satisfies the configured filter. An
// empty Name/Address matches anything; set fields must all match.
func (d BLEDevice) matches(opts bleScanOptions) bool {
	if opts.Name != "" && !strings.EqualFold(d.Name, opts.Name) {
		return false
	}
	if opts.Address != "" && !strings.EqualFold(d.AddrString, opts.Address) {
		return false
	}
	return true
}

// countMatches reports how many accumulated candidates satisfy the configured
// Name/Address filter. Without a filter it returns 0: the scan then runs to
// its settle/timeout decision and pick chooses by signal strength instead.
func (c bleCandidates) countMatches(opts bleScanOptions) int {
	if opts.Name == "" && opts.Address == "" {
		return 0
	}
	n := 0
	for _, d := range c.byKey {
		if d.matches(opts) {
			n++
		}
	}
	return n
}

// pick chooses one device. With a filter configured the strongest matching
// candidate wins; without one the strongest overall candidate does and
// ambiguous is true when more than one device was seen. The error lists every
// device seen so a mistyped filter is easy to spot.
func (c bleCandidates) pick(opts bleScanOptions) (chosen BLEDevice, ambiguous bool, err error) {
	if len(c.byKey) == 0 {
		return BLEDevice{}, false, errors.New("no MeshCore devices found")
	}

	hasFilter := opts.Name != "" || opts.Address != ""
	// Iterate the sorted list so equal-RSSI candidates resolve deterministically:
	// the first matching entry is the strongest.
	for _, d := range c.list() {
		if hasFilter && !d.matches(opts) {
			continue
		}
		return d, !hasFilter && len(c.byKey) > 1, nil
	}
	return BLEDevice{}, false, fmt.Errorf("no MeshCore device matching name=%q address=%q; seen: %s",
		opts.Name, opts.Address, c.listString())
}

// list returns the candidates sorted by descending RSSI, ties broken by
// address so the order is stable across runs.
func (c bleCandidates) list() []BLEDevice {
	out := make([]BLEDevice, 0, len(c.byKey))
	for _, d := range c.byKey {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RSSI != out[j].RSSI {
			return out[i].RSSI > out[j].RSSI
		}
		return out[i].AddrString < out[j].AddrString
	})
	return out
}

// listString renders the candidates for logs and error messages, strongest
// signal first.
func (c bleCandidates) listString() string {
	parts := make([]string, 0, len(c.byKey))
	for _, d := range c.list() {
		// %q keeps a hostile advertiser's name (any device in radio range
		// controls LocalName) from forging log or terminal lines.
		parts = append(parts, fmt.Sprintf("%q (%s, %d dBm)", d.Name, d.AddrString, d.RSSI))
	}
	return strings.Join(parts, ", ")
}

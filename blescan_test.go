package meshcore

import (
	"strings"
	"testing"
	"time"
)

// device builds a candidate the way the scan callback would record it.
func device(addr, name string, rssi int16) BLEDevice {
	return BLEDevice{AddrString: addr, Name: name, RSSI: rssi}
}

// populated returns candidates with three devices at different signal levels.
func populated() bleCandidates {
	var c bleCandidates
	c.observe("aa:aa:aa:00:00:01", "MeshCore-AB12", -50)
	c.observe("aa:aa:aa:00:00:02", "MeshCore-CD34", -70)
	c.observe("aa:aa:aa:00:00:03", "MeshCore-EF56", -60)
	return c
}

func TestObserveDedupesByAddressAndKeepsStrongestRSSI(t *testing.T) {
	var c bleCandidates

	if !c.observe("aa:aa:aa:00:00:01", "MeshCore-AB12", -80) {
		t.Fatal("first observation should report a new device")
	}
	if c.observe("aa:aa:aa:00:00:01", "MeshCore-AB12", -90) {
		t.Fatal("repeat observation of the same address should not report a new device")
	}
	if c.observe("aa:aa:aa:00:00:01", "MeshCore-AB12", -60) {
		t.Fatal("repeat observation of the same address should not report a new device")
	}

	d := c.byKey["aa:aa:aa:00:00:01"]
	if d == nil {
		t.Fatal("device missing from candidates")
	}
	if d.RSSI != -60 {
		t.Errorf("RSSI = %d, want strongest seen -60", d.RSSI)
	}
	if len(c.byKey) != 1 {
		t.Errorf("len(byKey) = %d, want 1 (same address deduplicated)", len(c.byKey))
	}

	// A later advertisement carrying the complete name refreshes it.
	c.observe("aa:aa:aa:00:00:01", "MeshCore-AB12-renamed", -70)
	if got := c.byKey["aa:aa:aa:00:00:01"].Name; got != "MeshCore-AB12-renamed" {
		t.Errorf("Name = %q, want refreshed name", got)
	}
	if got := c.byKey["aa:aa:aa:00:00:01"].RSSI; got != -60 {
		t.Errorf("RSSI = %d, want -60 (name refresh must not weaken it)", got)
	}
}

func TestDeviceMatches(t *testing.T) {
	tests := []struct {
		name string
		dev  BLEDevice
		opts bleScanOptions
		want bool
	}{
		{"no filter matches anything", device("aa:11", "MeshCore-AB12", -50), bleScanOptions{}, true},
		{"name match", device("aa:11", "MeshCore-AB12", -50), bleScanOptions{Name: "MeshCore-AB12"}, true},
		{"name is case-insensitive", device("aa:11", "MeshCore-AB12", -50), bleScanOptions{Name: "meshcore-ab12"}, true},
		{"name mismatch", device("aa:11", "MeshCore-AB12", -50), bleScanOptions{Name: "MeshCore-CD34"}, false},
		{"address match", device("AA:11", "MeshCore-AB12", -50), bleScanOptions{Address: "aa:11"}, true},
		{"address mismatch", device("AA:11", "MeshCore-AB12", -50), bleScanOptions{Address: "aa:22"}, false},
		{"name and address both match", device("AA:11", "MeshCore-AB12", -50), bleScanOptions{Name: "MeshCore-AB12", Address: "aa:11"}, true},
		{"name matches but address does not", device("AA:11", "MeshCore-AB12", -50), bleScanOptions{Name: "MeshCore-AB12", Address: "aa:22"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.dev.matches(tt.opts); got != tt.want {
				t.Errorf("matches(%+v) = %v, want %v", tt.opts, got, tt.want)
			}
		})
	}
}

func TestMatchesFilter(t *testing.T) {
	var empty bleCandidates
	if empty.matchesFilter(bleScanOptions{Name: "MeshCore-AB12"}) {
		t.Error("no candidates: filter can never match")
	}
	if empty.matchesFilter(bleScanOptions{}) {
		t.Error("no candidates and no filter: must not report a hit")
	}

	c := populated()
	if c.matchesFilter(bleScanOptions{}) {
		t.Error("no filter configured: must report false so the scan runs to its settle/timeout decision")
	}
	if !c.matchesFilter(bleScanOptions{Name: "MESHCORe-cd34"}) {
		t.Error("a candidate matching the name filter exists")
	}
	if !c.matchesFilter(bleScanOptions{Address: "AA:AA:AA:00:00:03"}) {
		t.Error("a candidate matching the address filter exists")
	}
	if c.matchesFilter(bleScanOptions{Name: "MeshCore-XXXX"}) {
		t.Error("no candidate matches this name")
	}
}

func TestPickWithoutFilterSelectsStrongestSignal(t *testing.T) {
	c := populated()

	dev, ambiguous, err := c.pick(bleScanOptions{})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if dev.AddrString != "aa:aa:aa:00:00:01" {
		t.Errorf("picked %s, want strongest signal aa:aa:aa:00:00:01 (-50 dBm)", dev.AddrString)
	}
	if !ambiguous {
		t.Error("several devices without a filter must be reported ambiguous")
	}

	// A single device is not ambiguous — no warning needed.
	var single bleCandidates
	single.observe("aa:aa:aa:00:00:02", "MeshCore-CD34", -70)
	_, ambiguous, err = single.pick(bleScanOptions{})
	if err != nil {
		t.Fatalf("pick single: %v", err)
	}
	if ambiguous {
		t.Error("a single device must not be reported ambiguous")
	}
}

func TestPickWithFilterIsNeverAmbiguous(t *testing.T) {
	c := populated()

	dev, ambiguous, err := c.pick(bleScanOptions{Name: "MeshCore-EF56"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if dev.AddrString != "aa:aa:aa:00:00:03" {
		t.Errorf("picked %s, want the named device aa:aa:aa:00:00:03", dev.AddrString)
	}
	if ambiguous {
		t.Error("a filter makes the choice deterministic; ambiguity is only for the no-filter case")
	}
}

func TestPickFilterNoMatchListsSeenDevices(t *testing.T) {
	c := populated()

	_, _, err := c.pick(bleScanOptions{Name: "MeshCore-XXXX"})
	if err == nil {
		t.Fatal("pick must fail when nothing matches the filter")
	}
	for _, seen := range []string{"MeshCore-AB12", "MeshCore-CD34", "MeshCore-EF56"} {
		if !strings.Contains(err.Error(), seen) {
			t.Errorf("error %q must list seen device %s so a typo is easy to spot", err, seen)
		}
	}
}

func TestPickEmptyCandidates(t *testing.T) {
	var c bleCandidates
	_, _, err := c.pick(bleScanOptions{})
	if err == nil || !strings.Contains(err.Error(), "no MeshCore devices found") {
		t.Fatalf("pick on empty candidates = %v, want a no-devices error", err)
	}
}

func TestListAndPickAreDeterministicOnEqualRSSI(t *testing.T) {
	var c bleCandidates
	c.observe("aa:aa:aa:00:00:02", "MeshCore-CD34", -60)
	c.observe("aa:aa:aa:00:00:01", "MeshCore-AB12", -60)

	got := c.list()
	if len(got) != 2 || got[0].AddrString != "aa:aa:aa:00:00:01" || got[1].AddrString != "aa:aa:aa:00:00:02" {
		t.Errorf("list = %+v, want ties broken by address (…0001 first)", got)
	}

	dev, _, err := c.pick(bleScanOptions{})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if dev.AddrString != "aa:aa:aa:00:00:01" {
		t.Errorf("picked %s, want the address tie-break …0001", dev.AddrString)
	}
}

func TestApplyBLEOptions(t *testing.T) {
	got := applyBLEOptions(nil)
	if got.Name != "" || got.Address != "" || got.Timeout != 0 {
		t.Errorf("no options must leave the zero value: %+v", got)
	}

	got = applyBLEOptions([]BLEOption{
		WithDeviceName("MeshCore-AB12"),
		WithDeviceAddress("aa:11"),
		WithScanTimeout(3 * time.Second),
	})
	if got.Name != "MeshCore-AB12" || got.Address != "aa:11" || got.Timeout != 3*time.Second {
		t.Errorf("applyBLEOptions = %+v, want all three options applied", got)
	}
}

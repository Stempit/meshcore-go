package meshcore

import (
	"testing"
	"time"
)

// statsFrame builds a respCodeStats frame body of the given type.
func statsFrame(statsType byte, payload ...byte) []byte {
	return append([]byte{respCodeStats, statsType}, payload...)
}

// TestStatsSnapshotNotAliased pins the ownership rule: onStats and GetStats
// must hand out snapshots — a callback that keeps its argument must not see
// later frames mutate it, and the two readers must never alias the live struct.
func TestStatsSnapshotNotAliased(t *testing.T) {
	c := NewMeshCoreClient(nil)
	var cb, first *CompanionRadioStats
	c.onStats = func(s *CompanionRadioStats) {
		if first == nil {
			first = s // remember the first delivery; later frames must not touch it
		}
		cb = s
	}

	c.parseStats(statsFrame(STATS_TYPE_CORE, 0x39, 0x05, 1, 0, 0, 0, 0, 0, 3))
	if cb == nil {
		t.Fatal("onStats not called")
	}
	if got := c.GetStats(); got == nil || got.BatteryMv != 0x0539 {
		t.Fatalf("GetStats: %+v", got)
	}
	if got := c.GetStats(); got == cb {
		t.Fatal("GetStats and onStats alias the same pointer")
	}

	// A second frame must not reach into previously delivered snapshots.
	c.parseStats(statsFrame(STATS_TYPE_CORE, 0x11, 0x02, 1, 0, 0, 0, 0, 0, 4))
	if first.BatteryMv != 0x0539 {
		t.Fatalf("first callback snapshot mutated by a later frame: %+v", first)
	}
}

// TestStatsConcurrentReaders is meant to run under -race: parseStats writes
// the shared stats while GetStats/FormatStats read them.
func TestStatsConcurrentReaders(t *testing.T) {
	c := NewMeshCoreClient(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		frame := statsFrame(STATS_TYPE_RADIO, make([]byte, 12)...)
		for i := 0; i < 1000; i++ {
			c.parseStats(frame)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = c.GetStats()
		_ = c.FormatStats()
		select {
		case <-done:
			return
		case <-time.After(50 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("writer goroutine did not finish")
			}
		}
	}
}

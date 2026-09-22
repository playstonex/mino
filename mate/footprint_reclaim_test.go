package mate

import (
	"testing"
)

func TestReclaimThresholdBelowKillLine(t *testing.T) {
	// The reclaim must fire well before the 50 MB EXC_RESOURCE kill, with
	// margin for the ~19-24 MB runtime overhead that rides above mapped bytes.
	// A threshold at or above the kill line would never save the process.
	const killLine = 50 * 1024 * 1024
	if iosReclaimMappedThreshold >= killLine {
		t.Fatalf("reclaim threshold %d must be below the %d kill line", iosReclaimMappedThreshold, killLine)
	}
	// And it must be positive / non-trivial, or it would fire every tick on an
	// idle tunnel (turning a stop-the-world call into a hot loop).
	if iosReclaimMappedThreshold < 8*1024*1024 {
		t.Fatalf("reclaim threshold %d is too low; would fire on an idle tunnel", iosReclaimMappedThreshold)
	}
}

func TestReclaimIntervalIsSane(t *testing.T) {
	// The scavenger loses the race over seconds; the reclaim must be frequent
	// enough to catch the ratchet but not a millisecond-scale hot loop.
	if iosReclaimInterval <= 0 {
		t.Fatalf("reclaim interval must be positive, got %v", iosReclaimInterval)
	}
	if iosReclaimInterval.Seconds() > 10 {
		t.Fatalf("reclaim interval %v is too slow to catch the footprint ratchet", iosReclaimInterval)
	}
}

// startFootprintReclaimer is a no-op off iOS; calling it on the test host must
// not arm a goroutine or panic.
func TestStartFootprintReclaimerNoopOffIOS(t *testing.T) {
	// runtime.GOOS is the test host (not ios), so this must return without
	// arming anything. We can't observe the goroutine directly, but the call
	// must not panic and must be idempotent.
	startFootprintReclaimer()
	startFootprintReclaimer()
}

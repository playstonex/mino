package mate

import (
	"encoding/json"
	"runtime"
	"testing"
)

// The point of these tests is that a stats reader which silently returns zeros
// is worse than no stats reader at all: it would be read as "the heap is fine"
// and send the next investigation the wrong way. So they assert the values are
// actually populated, not merely that the call returns valid JSON.

func decodeStats(t *testing.T) runtimeStats {
	t.Helper()
	raw := GetRuntimeStatsJSON()
	if raw == "{}" {
		t.Fatalf("GetRuntimeStatsJSON returned the encode-failure sentinel")
	}
	var stats runtimeStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatalf("GetRuntimeStatsJSON returned undecodable JSON: %v (%s)", err, raw)
	}
	return stats
}

func TestGetRuntimeStatsReportsLiveHeap(t *testing.T) {
	stats := decodeStats(t)

	// A running Go process always has a non-zero live heap and a larger total.
	// Zero here means the runtime/metrics names stopped resolving -- exactly
	// the silent-degradation case worth failing on.
	if stats.HeapBytes == 0 {
		t.Error("HeapBytes is 0; the /memory/classes/heap/objects:bytes metric did not resolve")
	}
	if stats.TotalBytes == 0 {
		t.Error("TotalBytes is 0; the /memory/classes/total:bytes metric did not resolve")
	}
	if stats.TotalBytes < stats.HeapBytes {
		t.Errorf("TotalBytes (%d) < HeapBytes (%d); total maps everything and must be the larger figure",
			stats.TotalBytes, stats.HeapBytes)
	}
	if stats.GOMAXPROCS != runtime.GOMAXPROCS(0) {
		t.Errorf("GOMAXPROCS = %d, want %d", stats.GOMAXPROCS, runtime.GOMAXPROCS(0))
	}
}

func TestGetRuntimeStatsReportsRecordedLimits(t *testing.T) {
	// Save and restore: these are process-global, and leaving them changed
	// would silently alter what any later test in this package observes.
	originalLimit := effectiveMemoryLimit.Load()
	originalGC := effectiveGCPercent.Load()
	t.Cleanup(func() { recordRuntimeLimits(originalLimit, int(originalGC)) })

	recordRuntimeLimits(40*1024*1024, 100)
	stats := decodeStats(t)
	if stats.MemLimitBytes != 40*1024*1024 {
		t.Errorf("MemLimitBytes = %d, want %d", stats.MemLimitBytes, 40*1024*1024)
	}
	if stats.GCPercent != 100 {
		t.Errorf("GCPercent = %d, want 100", stats.GCPercent)
	}

	// 0 must survive as 0 and mean "unlimited" (the macOS case), not be
	// confused with an unset field or replaced by a default.
	recordRuntimeLimits(0, 100)
	if got := decodeStats(t).MemLimitBytes; got != 0 {
		t.Errorf("MemLimitBytes = %d for the unlimited case, want 0", got)
	}
}

// GCCPUSeconds is only meaningful as a delta, so the contract this asserts is
// that it is monotonic -- a caller sampling it twice must never see it go
// backwards, or a computed fraction would come out negative.
func TestGCCPUSecondsIsMonotonic(t *testing.T) {
	first := decodeStats(t).GCCPUSeconds
	runtime.GC()
	second := decodeStats(t).GCCPUSeconds

	if second < first {
		t.Errorf("GCCPUSeconds went backwards: %v then %v", first, second)
	}
	if first < 0 || second < 0 {
		t.Errorf("GCCPUSeconds is negative: %v, %v", first, second)
	}
}

func TestNumGCAdvancesAcrossACollection(t *testing.T) {
	before := decodeStats(t).NumGC
	runtime.GC()
	after := decodeStats(t).NumGC

	// Proves the counter is wired to the real collector rather than parked at
	// a constant, which a zero-returning metric lookup would look like.
	if after <= before {
		t.Errorf("NumGC did not advance across runtime.GC(): %d then %d", before, after)
	}
}

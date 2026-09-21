package mate

import (
	"encoding/json"
	"runtime"
	runtimeDebug "runtime/debug"
	"runtime/metrics"
	"sync/atomic"
)

// The Go runtime knobs init() (service.go) installs are recorded here because
// neither has a getter: debug.SetMemoryLimit and debug.SetGCPercent both
// return the PREVIOUS value, so asking "what is in force?" after the fact
// means setting it again. Recording them at the point they are applied is the
// only way to report them without perturbing them.
var (
	effectiveMemoryLimit atomic.Int64 // bytes; 0 = no limit on this platform
	effectiveGCPercent   atomic.Int64 // debug.SetGCPercent value in force
)

// recordRuntimeLimits is called by init() right after the knobs are applied.
func recordRuntimeLimits(memLimitBytes int64, gcPercent int) {
	effectiveMemoryLimit.Store(memLimitBytes)
	effectiveGCPercent.Store(int64(gcPercent))
}

// runtimeStats is the JSON shape GetRuntimeStatsJSON returns.
//
// This exists because the extension had NO on-device visibility into the Go
// heap whatsoever. A 2026-09-21 tunnel livelock under a saturating speedtest
// could only be characterised by attaching Xcode to the running process: the
// dump showed a 48.4 MB footprint with two GC mark workers draining
// concurrently, but nothing the device itself could export said the heap was
// pressed against its 40 MB soft limit. Re-evaluating that limit needs the
// live-heap number MEASURED. Guessing a replacement from the footprint alone
// would just move the cliff.
type runtimeStats struct {
	// HeapBytes is live heap objects -- the quantity the soft memory limit is
	// actually compared against, and therefore the one that decides whether
	// the runtime is about to start GCing continuously.
	HeapBytes uint64 `json:"heapBytes"`
	// TotalBytes is everything the Go runtime has mapped, INCLUDING spans it
	// has already handed back to the OS. On its own it therefore overstates
	// what the platform charges this process: subtract ReleasedBytes for a
	// Go-side footprint proxy. The authoritative number is the task's
	// phys_footprint, which only the Swift side can read.
	TotalBytes uint64 `json:"totalBytes"`
	// ReleasedBytes is heap memory returned to the OS but still mapped. This
	// exists because the 2026-09-21 run reported totalBytes=41.4 MiB against a
	// live heap of 10.4 MiB, and there was no way to tell how much of that
	// 31 MiB gap was actually charged to the process.
	ReleasedBytes uint64 `json:"releasedBytes"`
	// MemLimitBytes is the soft limit in force, or 0 when unlimited. A soft
	// limit is not a failure point: exceeding it makes Go GC harder, not
	// allocate-fail, which is why breaching it presents as unresponsiveness
	// rather than a crash.
	MemLimitBytes int64 `json:"memLimitBytes"`
	GCPercent     int64 `json:"gcPercent"`
	NumGC         uint64 `json:"numGC"`
	// GCCPUSeconds is MONOTONIC total CPU time spent in GC since process
	// start. A single reading is meaningless -- sample it twice and divide the
	// delta by (elapsed wall time * GOMAXPROCS) to get the fraction of the
	// CPU budget GC is consuming. That fraction is the death-spiral signal.
	GCCPUSeconds float64 `json:"gcCpuSeconds"`
	GOMAXPROCS   int     `json:"gomaxprocs"`
	// Goroutines is included because a saturating transfer that leaks or
	// blocks goroutines raises the stack footprint without raising the live
	// heap, which would otherwise look like "memory is fine".
	Goroutines uint64 `json:"goroutines"`
}

// GetRuntimeStatsJSON reports Go runtime memory and GC state as JSON.
//
// Deliberately built on runtime/metrics rather than runtime.ReadMemStats:
// ReadMemStats stops the world, and this is called on a periodic timer in a
// process whose problem is already that it cannot get scheduled. An
// unsupported metric is reported as 0 rather than failing the whole call, so
// a Go toolchain change degrades this to partial data instead of breaking it.
func GetRuntimeStatsJSON() string {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/sched/goroutines:goroutines"},
	}
	metrics.Read(samples)

	stats := runtimeStats{
		HeapBytes:     uint64Value(samples[0]),
		TotalBytes:    uint64Value(samples[1]),
		MemLimitBytes: effectiveMemoryLimit.Load(),
		GCPercent:     effectiveGCPercent.Load(),
		NumGC:         uint64Value(samples[2]),
		GCCPUSeconds:  float64Value(samples[3]),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		ReleasedBytes: uint64Value(samples[4]),
		Goroutines:    uint64Value(samples[5]),
	}

	encoded, err := json.Marshal(stats)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// ReleaseOSMemoryJSON forces a collection and hands unused spans back to the
// OS, returning the mapped total before and after so the caller can log what it
// achieved instead of assuming.
//
// This is the lever the soft memory limit cannot pull. iOS killed the extension
// with `Terminated due to memory issue` while the live heap sat at 5-15 MB:
// SetMemoryLimit is compared against live heap, so it never intervened, while
// total MAPPED memory climbed (9.6 -> 18.7 -> 26.5 MB in 90 s) and took the
// process footprint to the kill line with it. Under packet churn the background
// scavenger paces itself to about 1% of CPU and simply loses that race.
//
// It works on this platform specifically because Go's darwin `sysUnusedOS` uses
// MADV_FREE_REUSABLE, which -- unlike plain MADV_FREE -- propagates the
// accounting to `task_info`. So released spans actually leave `phys_footprint`,
// which is the number Jetsam compares against the limit. On a platform using
// plain MADV_FREE this call would lower Go's own figures and change nothing the
// kernel charges for.
//
// FreeOSMemory stops the world, so this is for a caller that has decided the
// footprint is heading for a kill -- not for a timer.
func ReleaseOSMemoryJSON() string {
	before := mappedBytes()
	runtimeDebug.FreeOSMemory()
	after := mappedBytes()

	payload := struct {
		BeforeBytes uint64 `json:"beforeBytes"`
		AfterBytes  uint64 `json:"afterBytes"`
	}{BeforeBytes: before, AfterBytes: after}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// mappedBytes is total mapped minus spans already returned to the OS -- the
// Go-side share of what the platform charges, and therefore the figure that
// should move when spans are released.
func mappedBytes() uint64 {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(samples)

	total := uint64Value(samples[0])
	released := uint64Value(samples[1])
	if released > total {
		return 0
	}
	return total - released
}

func uint64Value(s metrics.Sample) uint64 {
	if s.Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s.Value.Uint64()
}

func float64Value(s metrics.Sample) float64 {
	if s.Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return s.Value.Float64()
}

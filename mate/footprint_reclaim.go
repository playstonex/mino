package mate

import (
	"runtime"
	runtimeDebug "runtime/debug"
	"sync"
	"time"

	"github.com/metacubex/mihomo/log"
)

// Periodic footprint reclaimer for the iOS Network Extension.
//
// Two heap profiles taken during a saturating hysteria2 download on 2026-09-22
// (early / late, diffed) settled what four rounds of per-buffer caps could not:
// the process is NOT killed on live heap. LIVE inuse_space was 10.6 MB early and
// 16.6 MB late -- nowhere near the 50 MB EXC_RESOURCE limit -- yet the kernel
// SIGKILLed the extension at 50 MB. The killer is the gap between the live heap
// and phys_footprint: freed spans the Go runtime has not returned to the OS.
//
// The churn is what drives it. The dominant term, quic-go's StreamFrame pool
// (wire.init.0.func1), grew 1.14 -> 6.64 MB across the two profiles and
// sync.Pool.Get was 75% of the late heap -- a pool that is Get/Put'd tens of
// thousands of times a second. Every miss maps a fresh MaxPacketBufferSize span;
// releases pile up as MADV-able but still-mapped pages. Go's background
// scavenger paces itself to ~1% CPU and loses the race against that churn, so
// mapped memory ratchets up and takes phys_footprint to the kill line while the
// live heap sits flat.
//
// debug.FreeOSMemory() is the lever the soft memory limit cannot pull: it works
// on darwin specifically because Go's sysUnusedOS uses MADV_FREE_REUSABLE, which
// (unlike plain MADV_FREE) propagates to task_info, so released spans actually
// leave phys_footprint -- the number Jetsam compares. The existing helper
// documented it as "for a caller that has decided the footprint is heading for a
// kill -- not for a timer"; the profiles are that decision, made from data: the
// footprint IS heading for the kill on every saturating transfer, and a timer
// gated on mapped bytes is exactly the caller that catches it before Jetsam does.
//
// It is gated, not unconditional: the reclaim only fires when mapped bytes cross
// a threshold well below the 50 MB kill line, so an idle tunnel pays nothing and
// a busy one pays a stop-the-world FreeOSMemory only when it is actually
// accumulating unreturned spans. This does NOT cost throughput: it returns
// memory that is already free, never anything in flight, and measured GC CPU on
// this path is 0.0-0.5%, so the stop-the-world pause has ample headroom.
const (
	// Fire the reclaim when the Go-side mapped total crosses this. Chosen
	// below the ~40 MB point where footprint has historically approached the
	// 50 MB kill, with margin for the ~19-24 MB runtime overhead that rides
	// above mapped, so the reclaim lands before Jetsam does.
	iosReclaimMappedThreshold = 30 * 1024 * 1024
	// The scavenger loses the race over seconds, not milliseconds; 1s catches
	// the ratchet without turning a stop-the-world call into a hot loop.
	iosReclaimInterval = 1 * time.Second
)

var startReclaimerOnce sync.Once

// startFootprintReclaimer launches the periodic reclaimer once, on iOS only.
// It is safe to call from multiple service starts; only the first arms it.
func startFootprintReclaimer() {
	if runtime.GOOS != "ios" {
		return
	}
	startReclaimerOnce.Do(func() {
		go reclaimLoop(iosReclaimInterval, iosReclaimMappedThreshold)
	})
}

// reclaimLoop is the testable core: every interval, if mapped bytes exceed the
// threshold, force spans back to the OS and log only when the reclaim actually
// recovered something worth noting.
func reclaimLoop(interval time.Duration, threshold uint64) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Only a reclaim that frees at least this much is worth a log line. A
	// sustained large download keeps mapped bytes hovering at the threshold, so
	// an unconditional Info log here would print at 1 Hz for the whole transfer
	// and drown the tunnel log; gating on a real recovery keeps the signal.
	const logMinRecovered = 1 * 1024 * 1024
	for range ticker.C {
		before := mappedBytes()
		if before < threshold {
			continue
		}
		runtimeDebug.FreeOSMemory()
		after := mappedBytes()
		if before > after && before-after >= logMinRecovered {
			log.Infoln("[GoHeap] periodic reclaim: mapped %.1fMiB -> %.1fMiB (threshold %.1fMiB)",
				float64(before)/1024/1024, float64(after)/1024/1024, float64(threshold)/1024/1024)
		}
	}
}

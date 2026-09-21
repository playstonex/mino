package outbound

import (
	"runtime"

	"github.com/metacubex/quic-go"
)

// QUIC receive-window ceilings for memory-capped platforms.
//
// quic-go's own defaults are sized for hosts with gigabytes available:
// MaxConnectionReceiveWindow is 15 MB and MaxStreamReceiveWindow is 6 MB. An
// iOS Network Extension gets roughly 50 MB for EVERYTHING -- the Go heap, the
// userspace TCP stack, the Swift/ObjC runtime and every thread stack -- so the
// connection window alone is licensed to take a third of the process.
//
// This is not theoretical. On 2026-09-21 the extension was SIGKILLed with
// `Terminated due to memory issue` during a speedtest over a hysteria2 proxy in
// Japan, and the instrumented run caught the climb: LIVE heap 2.8 -> 6.7 ->
// 22.5 -> 26.0 MB with footprint tracking it to 48.8 MB, at which point the
// kernel killed it. Forcing a collection at the top of that climb recovered
// nothing (mapped 38.9 -> 39.0 MB), which is what says the memory was live and
// in use rather than garbage awaiting a sweep. hysteria2 multiplexes every
// proxied TCP connection onto ONE QUIC connection, so a single saturating
// transfer is enough to grow these windows to their permitted maximum.
//
// The values are derived from bandwidth-delay product rather than picked for
// roundness: a window has to hold one round trip of data in flight to avoid
// capping throughput, and BDP at 180 Mbps over a ~150 ms trans-Pacific path is
// 180e6 / 8 * 0.15 = 3.4 MB. 4 MB covers that with margin while costing less
// than a third of what the default permitted. The stream ceiling is half the
// connection ceiling, preserving quic-go's own ratio.
//
// Initial windows are left alone on purpose. They are small (512 KB) and they
// are what auto-tuning grows FROM -- lowering them would slow ramp-up on every
// connection to address a problem that only exists at the ceiling.
const (
	mobileMaxConnectionReceiveWindow = 4 * 1024 * 1024
	mobileMaxStreamReceiveWindow     = 2 * 1024 * 1024
)

// applyPlatformQUICWindowCeiling bounds the receive windows on platforms with a
// hard process memory budget, leaving every other platform on quic-go's
// defaults.
//
// An explicitly configured window is always honoured, including one larger than
// the ceiling: the user asking for a specific window is a deliberate act, and
// silently overriding it would make the config lie about what is in force. Only
// the unset case -- where the alternative is quic-go's 15 MB -- is filled in.
func applyPlatformQUICWindowCeiling(config *quic.Config) {
	applyQUICWindowCeilingForGOOS(config, runtime.GOOS)
}

// applyQUICWindowCeilingForGOOS is the testable form. runtime.GOOS is a
// compile-time constant, so the iOS branch is unreachable from a test running on
// any other platform unless the target is a parameter.
func applyQUICWindowCeilingForGOOS(config *quic.Config, goos string) {
	if !isMemoryCappedGOOS(goos) {
		return
	}
	if config.MaxConnectionReceiveWindow == 0 {
		config.MaxConnectionReceiveWindow = mobileMaxConnectionReceiveWindow
	}
	if config.MaxStreamReceiveWindow == 0 {
		config.MaxStreamReceiveWindow = mobileMaxStreamReceiveWindow
	}
}

// isMemoryCappedGOOS reports whether the platform's process budget is small
// enough for quic-go's defaults to be a material share of it.
//
// iOS only. Android's VpnService has no comparable per-process ceiling, and
// macOS Network Extensions are not constrained this way -- mate/service.go's
// darwin branch deliberately sets no memory limit at all.
func isMemoryCappedGOOS(goos string) bool {
	return goos == "ios"
}

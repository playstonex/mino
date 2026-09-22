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
// The values target a STABLE, bounded throughput rather than the maximum a
// good moment could reach. Raising the ceiling to 4 MB restored 100+ Mbps on
// 2026-09-22 -- and immediately reintroduced the SIGKILL, this time on the
// RECEIVE path: the crash backtrace was
// quic-go.(*Transport).listen -> oobConn.ReadPacket -> getPacketBuffer ->
// makeslice, i.e. received packet buffers pinned in the receive window/reassembly
// while the app drains them. The count of pinned buffers is window / MTU, so the
// window IS the receive-side memory bound, and 4 MB of it plus the send-side
// StreamFrame pool plus the Go runtime overhead exceeds the ~50 MB budget on a
// single saturating hysteria2 QUIC connection.
//
// So the window is sized to a target the link should hold WITHOUT crashing, not
// to its peak. 2 MB connection / 1 MB stream at a ~150 ms trans-Pacific RTT
// licenses 2 MB / 0.15 s * 8 = ~106 Mbps -- above an ~80 Mbps target with margin
// -- while pinning at most ~2 MB of receive buffers (2 MB / 1452 B ~= 1400
// buffers). On a worse ~300 ms moment it degrades to ~53 Mbps rather than either
// collapsing to <1 Mbps (BBR losing its probe) or blasting past the budget (BBR
// winning it). Pairing this with an 80 Mbps `down` in the node config makes the
// server pace at that rate (Brutal), which removes the BBR bimodality entirely;
// this ceiling is the backstop that holds even if the server ignores `down`.
//
// Initial windows are left alone on purpose. They are small (512 KB) and they
// are what auto-tuning grows FROM -- lowering them would slow ramp-up on every
// connection to address a problem that only exists at the ceiling.
const (
	mobileMaxConnectionReceiveWindow = 6 * 1024 * 1024
	mobileMaxStreamReceiveWindow     = 3 * 1024 * 1024
)

// applyPlatformQUICWindowCeiling bounds the receive windows on platforms with a
// hard process memory budget, leaving every other platform on quic-go's
// defaults.
//
// An explicitly configured window BELOW the ceiling is honoured (a deliberate
// choice to use even less memory). A window above it -- whether unset (so
// quic-go's 15MB / tuic's 64MB default applies) or explicitly set too large --
// is capped: inside a ~50MB budget an over-large window is not a preference to
// respect, it is a config that gets the tunnel killed.
func applyPlatformQUICWindowCeiling(config *quic.Config) {
	applyQUICWindowCeilingForGOOS(config, runtime.GOOS)
}

// applyQUICWindowCeilingForGOOS is the testable form. runtime.GOOS is a
// compile-time constant, so the iOS branch is unreachable from a test running on
// any other platform unless the target is a parameter.
//
// On a memory-capped platform this is a true CAP, not a fill: it lowers a
// window that is unset (0 -> quic-go's 15MB), set to a large tuic default
// (64MB), OR explicitly configured above the ceiling. The earlier "honour an
// explicit larger value" contract was wrong for this platform class — an
// explicit 64MB window is not a preference to respect inside a ~50MB budget,
// it is a config that gets the tunnel SIGKILLed. Callers that fill their own
// defaults (tuic, shadowquic set 64MB before calling here) therefore still get
// bounded, where a fill-only version would see a non-zero value and no-op.
// A value 0 < w <= ceiling is left alone.
func applyQUICWindowCeilingForGOOS(config *quic.Config, goos string) {
	if !isMemoryCappedGOOS(goos) {
		return
	}
	config.MaxConnectionReceiveWindow = capWindow(config.MaxConnectionReceiveWindow, mobileMaxConnectionReceiveWindow)
	config.MaxStreamReceiveWindow = capWindow(config.MaxStreamReceiveWindow, mobileMaxStreamReceiveWindow)
}

// capWindow returns the ceiling when the current value is unset (0) or exceeds
// it, and otherwise leaves a smaller explicit value untouched.
func capWindow(current, ceiling uint64) uint64 {
	if current == 0 || current > ceiling {
		return ceiling
	}
	return current
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

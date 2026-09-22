package tunnel

import (
	"runtime"
	"sync"
	"time"

	"github.com/metacubex/mihomo/log"
)

// Bounded concurrency for proxied TCP connections on memory-capped platforms.
//
// Every prior round of the iOS Network Extension memory work capped a
// PER-CONNECTION term: the gVisor receive/send buffer (512 KB each, auto-tuned
// up under load), the QUIC StreamFrame pool (1452 B per in-flight packet), the
// congestion window (2048 packets), the relay buffers (2x16 KB). Each cap was
// correct and each one was defeated the same way -- by the MULTIPLIER none of
// them touched.
//
// A proxied TCP connection is dispatched straight to handleTCPConn with no
// admission control: HandleTCPConn calls it synchronously, TCPIn's fan-in does
// `go handleTCPConn` per item, and the `tcpQueue` capacity (64) is dead code for
// TCP -- it bounds nothing that blocks a new connection. So the number of live
// connections has no ceiling, and aggregate memory is (per_conn x N) with N
// unbounded. A speedtest opens dozens of parallel streams; each one is licensed
// to grow its gVisor buffers toward 512 KB + 512 KB, so ~40 saturating streams
// alone approach the whole ~50 MB Network Extension budget, and the process is
// SIGKILLed by EXC_RESOURCE (RESOURCE_TYPE_MEMORY, limit=50 MB) -- observed
// 2026-09-22 the moment the per-connection window ceiling was lifted enough to
// restore 100+ Mbps.
//
// This is the one bound that makes every per-connection cap hold at once:
// with a ceiling of N concurrent connections, worst-case aggregate is
// N x per_conn regardless of which term dominates. It does NOT shrink any single
// connection -- so it does not cost throughput on a link that runs a handful of
// streams -- it only refuses to let the connection count multiply the budget
// away.
//
// The value is a compromise, not a BDP figure: a saturating connection's
// moderated gVisor buffers plus relay plus its QUIC share is order ~0.5-1 MB,
// so 128 concurrent leaves headroom under the gVisor+relay slice of the budget
// while comfortably admitting a speedtest's parallel streams and normal
// browsing fan-out. When the ceiling is reached a new connection BLOCKS until a
// slot frees rather than being dropped -- backpressure, not failure -- which is
// the right behaviour for a saturating transfer (the sender simply paces to the
// slots available) and invisible to normal use that never reaches the ceiling.
const iOSMaxConcurrentTCPConns = 128

// tcpConnSem is nil on platforms without a hard process memory budget, so the
// acquire/release become no-ops and non-iOS behaviour is byte-for-byte
// unchanged.
var tcpConnSem = newTCPConnSem(runtime.GOOS)

func newTCPConnSem(goos string) chan struct{} {
	if goos == "ios" {
		return make(chan struct{}, iOSMaxConcurrentTCPConns)
	}
	return nil
}

var logTCPConnCeilingOnce sync.Once

// acquireTCPConnSlot blocks until a connection slot is free on a memory-capped
// platform, and returns a release func plus ok=true. On every other platform it
// is a no-op returning a no-op release and ok=true.
//
// The wait is BOUNDED. A slot is held for the whole connection lifetime, and
// iOS accumulates many idle-but-open long-lived connections (push, chat
// heartbeats, backgrounded keep-alives) that occupy slots while moving no data.
// If all 128 were held by such connections, an unbounded `sem <- struct{}{}`
// would make a fresh foreground request block forever -- the user sees a hang,
// not backpressure. So the acquire waits at most acquireTimeout; on timeout it
// returns ok=false and the caller closes the connection rather than dialing an
// outbound for a request that has been waiting too long. Under normal load the
// slot is free immediately and the timeout never arms.
const acquireTimeout = 10 * time.Second

func acquireTCPConnSlot() (release func(), ok bool) {
	sem := tcpConnSem
	if sem == nil {
		return func() {}, true
	}
	logTCPConnCeilingOnce.Do(func() {
		log.Infoln("[TCP] concurrent proxied-connection ceiling active: %d "+
			"(bounds the multiplier every per-connection memory cap shares, "+
			"inside the ~50MB Network Extension budget)", iOSMaxConcurrentTCPConns)
	})
	// Fast path: a free slot is taken without arming a timer.
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
	}
	timer := time.NewTimer(acquireTimeout)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-timer.C:
		return func() {}, false
	}
}

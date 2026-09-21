package congestion

import (
	"runtime"
	"sync"

	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/quic-go/congestion"
)

// The congestion window is the ONLY thing bounding aggregate QUIC in-flight
// bytes, and on iOS that makes it a memory ceiling rather than just a
// throughput one.
//
// quic-go's default is MaxCongestionWindowPackets = 20000. Multiplied by the
// initial datagram size (~1350 B) that authorises roughly 27 MB in flight on a
// single connection -- inside a Network Extension whose whole phys_footprint
// budget is ~50 MB (confirmed: EXC_RESOURCE RESOURCE_TYPE_MEMORY, limit=50 MB).
//
// The cost is not hypothetical. A heap profile taken while footprint was
// climbing (early 31.8 MB vs late 47.9 MB) attributed 13.02 MB -- 58.9% of the
// late heap -- to quic-go's StreamFrame pool, reached through
// Conn.sendPackets -> packetPacker -> framer.getNextStreamFrame ->
// SendStream.popNewStreamFrame -> wire.GetStreamFrame. Each pooled frame
// reserves protocol.MaxPacketBufferSize (1452 B) of Data capacity whatever it
// actually carries, and frames stay live until the packet is acked or declared
// lost, so live frames == packets in flight. 13.02 MB / 1452 B is ~9000
// packets: BBR had grown the window there and the 20000-packet cap never bound
// it. The same profile shows 1.38 MB in BBR's own
// packetNumberIndexedQueue/RingBuffer, which is sized by that same in-flight
// count -- one cap, two terms.
//
// This is deliberately NOT the mistake the TCP receive window made earlier in
// the same investigation, where a 20 KB value was picked to hit a memory number
// and throttled the link to 3.45 Mbps. This ceiling is derived from bandwidth-
// delay product and then checked against the measured best throughput:
//
//	2048 packets * 1452 B    = 2.97 MB in flight
//	2.97 MB / 150 ms RTT     = ~158 Mbps   (the Japan path measured 120-180)
//	2.97 MB /  75 ms RTT     = ~317 Mbps
//
// So it sits above the throughput this link has ever reached while cutting the
// dominant heap term from 13 MB to ~3 MB. A path with both a very high
// bandwidth and a very high RTT could reach it; that trade is accepted, because
// exceeding it is what gets the tunnel killed, and a killed tunnel drops every
// connection while a bounded window only slows them.
const iOSMaxCongestionWindowPackets congestion.ByteCount = 2048

var logCWNDCeilingOnce sync.Once

// maxCongestionWindowPacketsForGOOS is the pure, platform-parameterized policy,
// separated from the caller so it can be unit tested on any build host rather
// than only on an iOS build. It returns the cap in PACKETS.
func maxCongestionWindowPacketsForGOOS(goos string) congestion.ByteCount {
	if goos == "ios" {
		return iOSMaxCongestionWindowPackets
	}
	return congestion.MaxCongestionWindowPackets
}

// congestionWindowPacketsForGOOS returns the (initial, max) window pair in
// packets, guaranteeing initial <= max.
//
// The initial value has to be clamped alongside the max: it comes from the
// user's `cwnd:` option, so a configuration asking for more than the ceiling
// would otherwise construct a sender whose starting window already exceeds its
// own maximum -- an incoherent state rather than a conservative one.
func congestionWindowPacketsForGOOS(goos string, initial congestion.ByteCount) (congestion.ByteCount, congestion.ByteCount) {
	max := maxCongestionWindowPacketsForGOOS(goos)
	if initial > max {
		initial = max
	}
	return initial, max
}

// congestionWindowPackets applies the policy on the running platform.
//
// The notice is emitted once per process, not once per dial: it is worth
// knowing the ceiling is in force when reading a log, and this path runs on
// every single outbound QUIC connection.
func congestionWindowPackets(initial congestion.ByteCount) (congestion.ByteCount, congestion.ByteCount) {
	clampedInitial, max := congestionWindowPacketsForGOOS(runtime.GOOS, initial)
	if max != congestion.MaxCongestionWindowPackets {
		logCWNDCeilingOnce.Do(func() {
			log.Infoln("[QUIC] congestion window capped at %d packets (default %d) to bound aggregate "+
				"in-flight bytes inside the ~50MB Network Extension budget; each in-flight packet "+
				"retains a %d-byte pooled StreamFrame", max, congestion.MaxCongestionWindowPackets, 1452)
		})
	}
	return clampedInitial, max
}

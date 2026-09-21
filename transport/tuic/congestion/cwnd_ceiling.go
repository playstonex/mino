package congestion

import (
	"runtime"
	"sync"

	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/quic-go/congestion"
)

// This mirrors transport/tuic/congestion_v2/cwnd_ceiling.go for the v1
// controllers (cubic, new_reno, bbr_meta_v1). The two congestion packages
// cannot import each other or a shared helper without an import cycle
// (transport/tuic/common already depends on BOTH), so the small policy is
// duplicated rather than shared. The value and the reasoning are identical;
// see the v2 file for the full derivation.
//
// Why this file exists at all: the memory kill was first fixed only on the
// bbr (v2) path, because that is what the reporting user runs. But cubic,
// new_reno and bbr_meta_v1 reach an even larger unbounded window here --
// MaxCongestionWindowPackets is 20000 and DefaultBBRMaxCongestionWindow is
// 10000 -- so a config with `congestion-controller: cubic` (or tuic, whose
// default cc is cubic) would reproduce the exact SIGKILL the v2 clamp closed.
// Bounding one controller and leaving its siblings unbounded is the same
// "cap one term, the unbounded one moves next door" trap this whole
// investigation kept falling into.
const iOSMaxCongestionWindowPackets congestion.ByteCount = 2048

var logCWNDCeilingOnce sync.Once

// maxCongestionWindowPacketsForGOOS returns the per-platform cap in PACKETS,
// given the controller's own default (cubic uses 20000, bbr_meta_v1 uses
// 10000). On iOS both collapse to the same 2048-packet ceiling; elsewhere the
// controller's own default is returned untouched.
func maxCongestionWindowPacketsForGOOS(goos string, controllerDefault congestion.ByteCount) congestion.ByteCount {
	if goos == "ios" && iOSMaxCongestionWindowPackets < controllerDefault {
		return iOSMaxCongestionWindowPackets
	}
	return controllerDefault
}

// cappedMaxCongestionWindow applies the policy on the running platform and
// converts packets to bytes, logging once per process when the cap is in force.
func cappedMaxCongestionWindow(controllerDefaultPackets, maxDatagramSize congestion.ByteCount) congestion.ByteCount {
	capped := maxCongestionWindowPacketsForGOOS(runtime.GOOS, controllerDefaultPackets)
	if capped != controllerDefaultPackets {
		logCWNDCeilingOnce.Do(func() {
			log.Infoln("[QUIC] congestion window capped at %d packets (controller default %d) to bound "+
				"aggregate in-flight bytes inside the ~50MB Network Extension budget",
				capped, controllerDefaultPackets)
		})
	}
	return capped * maxDatagramSize
}

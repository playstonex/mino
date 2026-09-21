package congestion

import (
	"testing"
)

// Pinned by value, same rationale as the v2 test: an earlier round wrote window
// assertions against the constant, so raising the constant kept the suite green
// while the regression shipped.
func TestV1IOSCeilingIsPinned(t *testing.T) {
	// cubic default 20000 and bbr_meta_v1 default 10000 both collapse to 2048.
	if got := maxCongestionWindowPacketsForGOOS("ios", MaxCongestionWindowPackets); got != 2048 {
		t.Fatalf("cubic path iOS ceiling = %d, want 2048", got)
	}
	if got := maxCongestionWindowPacketsForGOOS("ios", DefaultBBRMaxCongestionWindow); got != 2048 {
		t.Fatalf("bbr_meta_v1 path iOS ceiling = %d, want 2048", got)
	}
}

func TestV1NonIOSKeepsControllerDefault(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows", "android"} {
		if got := maxCongestionWindowPacketsForGOOS(goos, MaxCongestionWindowPackets); got != MaxCongestionWindowPackets {
			t.Errorf("%s cubic ceiling = %d, want %d", goos, got, MaxCongestionWindowPackets)
		}
		if got := maxCongestionWindowPacketsForGOOS(goos, DefaultBBRMaxCongestionWindow); got != DefaultBBRMaxCongestionWindow {
			t.Errorf("%s bbr_meta_v1 ceiling = %d, want %d", goos, got, DefaultBBRMaxCongestionWindow)
		}
	}
}

// The ceiling must be strictly below BOTH controller defaults, or one of them
// is left unbounded and reads as fixed while it is not.
func TestV1CeilingBelowBothDefaults(t *testing.T) {
	if iOSMaxCongestionWindowPackets >= MaxCongestionWindowPackets {
		t.Fatalf("ceiling %d not below cubic default %d", iOSMaxCongestionWindowPackets, MaxCongestionWindowPackets)
	}
	if iOSMaxCongestionWindowPackets >= DefaultBBRMaxCongestionWindow {
		t.Fatalf("ceiling %d not below bbr_meta_v1 default %d", iOSMaxCongestionWindowPackets, DefaultBBRMaxCongestionWindow)
	}
}

// The cap must only ever LOWER: a controller default already below the ceiling
// must be returned untouched, never raised to it.
func TestV1CapOnlyLowers(t *testing.T) {
	if got := maxCongestionWindowPacketsForGOOS("ios", 1000); got != 1000 {
		t.Fatalf("a 1000-packet default was raised to %d; the cap must only lower", got)
	}
}

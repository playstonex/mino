package congestion

import (
	"testing"

	"github.com/metacubex/quic-go/congestion"
)

// The ceiling is pinned BY VALUE, not compared against the constant it is
// defined as. An earlier round of this work wrote every window assertion
// against the constant, so raising the constant kept the whole suite green and
// the regression shipped. Raising this ceiling must now be a visible test edit.
func TestIOSCongestionWindowCeilingIsPinned(t *testing.T) {
	if got := maxCongestionWindowPacketsForGOOS("ios"); got != 2048 {
		t.Fatalf("iOS congestion window ceiling = %d packets, want 2048; raising it "+
			"multiplies in-flight pooled StreamFrames (1452 B each) inside a ~50MB budget", got)
	}
}

func TestNonIOSKeepsUpstreamCeiling(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows", "android"} {
		if got := maxCongestionWindowPacketsForGOOS(goos); got != congestion.MaxCongestionWindowPackets {
			t.Errorf("%s ceiling = %d, want upstream default %d",
				goos, got, congestion.MaxCongestionWindowPackets)
		}
	}
}

// The ceiling must actually be BELOW the default, or the clamp is a no-op that
// reads as a fix.
func TestIOSCeilingIsBelowDefault(t *testing.T) {
	ios := maxCongestionWindowPacketsForGOOS("ios")
	if ios >= congestion.MaxCongestionWindowPackets {
		t.Fatalf("iOS ceiling %d is not below the default %d; the clamp would bound nothing",
			ios, congestion.MaxCongestionWindowPackets)
	}
}

// A user asking for a starting window larger than the ceiling must not produce a
// sender whose initial window exceeds its own maximum.
func TestInitialWindowNeverExceedsMax(t *testing.T) {
	cases := []struct {
		goos        string
		initial     congestion.ByteCount
		wantInitial congestion.ByteCount
	}{
		{"ios", 32, 32},          // the default cwnd, well under the ceiling
		{"ios", 2048, 2048},      // exactly at the ceiling
		{"ios", 100000, 2048},    // a configuration far above it
		{"linux", 100000, 20000}, // clamped to the upstream default, not to iOS's
	}
	for _, c := range cases {
		initial, max := congestionWindowPacketsForGOOS(c.goos, c.initial)
		if initial != c.wantInitial {
			t.Errorf("%s initial=%d: got %d, want %d", c.goos, c.initial, initial, c.wantInitial)
		}
		if initial > max {
			t.Errorf("%s initial=%d: initial %d exceeds max %d", c.goos, c.initial, initial, max)
		}
	}
}

// The ceiling has to stay large enough to carry the throughput this link has
// actually reached, or it repeats the 20KB-TCP-window mistake of buying memory
// with an unusable connection.
func TestCeilingCarriesMeasuredThroughput(t *testing.T) {
	const (
		frameBytes  = 1452 // protocol.MaxPacketBufferSize, reserved per in-flight frame
		rttMillis   = 150  // measured RTT to the Japan endpoint
		wantMinMbps = 120  // the low end of the 120-180 Mbps this path has reached
	)
	inFlight := float64(maxCongestionWindowPacketsForGOOS("ios")) * frameBytes
	mbps := inFlight * 8 / (float64(rttMillis) / 1000) / 1e6
	if mbps < wantMinMbps {
		t.Fatalf("ceiling supports only %.0f Mbps at %d ms RTT, below the %d Mbps measured "+
			"on this path; it would throttle the link rather than bound memory", mbps, rttMillis, wantMinMbps)
	}
}

// And it has to be small enough to matter: the whole point is that the
// in-flight term fits the budget with room for the rest of the process.
func TestCeilingFitsMemoryBudget(t *testing.T) {
	const (
		frameBytes = 1452
		budgetMiB  = 4.0 // the term's share; the late profile measured it at 13.02 MB
	)
	inFlightMiB := float64(maxCongestionWindowPacketsForGOOS("ios")) * frameBytes / (1 << 20)
	if inFlightMiB > budgetMiB {
		t.Fatalf("ceiling authorises %.2f MiB of in-flight frames, above the %.1f MiB "+
			"this term is allowed", inFlightMiB, budgetMiB)
	}
}

package sing_tun

import "testing"

// processorsPerChannelForGOOS pins iOS to a single gVisor packet-processor to
// hold the Network Extension inside its ~50MB budget, and keeps the configured
// value everywhere else.
func TestProcessorsPerChannelForGOOS(t *testing.T) {
	if got := processorsPerChannelForGOOS("ios", 4); got != 1 {
		t.Fatalf("ios processors-per-channel = %d, want 1 (extra processors cost memory the NE cannot spare)", got)
	}
	for _, goos := range []string{"darwin", "linux", "windows", "android"} {
		for _, configured := range []int{0, 1, 4} {
			if got := processorsPerChannelForGOOS(goos, configured); got != configured {
				t.Errorf("%s processors-per-channel = %d, want configured %d", goos, got, configured)
			}
		}
	}
}

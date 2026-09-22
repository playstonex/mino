package sing_tun

import "testing"

// processorsPerChannelForGOOS pins iOS to a single gVisor packet-processor to
// hold the Network Extension inside its ~50MB budget, and returns 0 ("use
// sing-tun's default") everywhere else so no desktop platform loses throughput.
func TestProcessorsPerChannelForGOOS(t *testing.T) {
	if got := processorsPerChannelForGOOS("ios"); got != 1 {
		t.Fatalf("ios processors-per-channel = %d, want 1 (extra processors cost memory the NE cannot spare)", got)
	}
	for _, goos := range []string{"darwin", "linux", "windows", "android"} {
		if got := processorsPerChannelForGOOS(goos); got != 0 {
			t.Errorf("%s processors-per-channel = %d, want 0 (sing-tun default; only iOS has the budget to warrant pinning)", goos, got)
		}
	}
}

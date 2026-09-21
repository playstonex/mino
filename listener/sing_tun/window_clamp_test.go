package sing_tun

import "testing"

// TestClampTCPWindowBytesForGOOS proves the real incident scenario -- a
// 131072-byte window on iOS, the value used in the real-device crash
// reproduced 2026-09-20 (docs/TUN_STACK_OPTIMIZATION.md step 3) -- gets
// lowered to the safety ceiling, while the same value on a desktop platform
// (no comparable Network Extension memory budget) and the "use sing-tun's
// built-in default" sentinel (0) both pass through untouched.
func TestClampTCPWindowBytesForGOOS(t *testing.T) {
	cases := []struct {
		name string
		goos string
		in   int
		want int
	}{
		{"ios zero sentinel passes through", "ios", 0, 0},
		{"ios negative passes through", "ios", -1, -1},
		{"ios below ceiling passes through", "ios", 16 * 1024, 16 * 1024},
		{"ios at ceiling passes through", "ios", iOSMaxTCPWindowBytes, iOSMaxTCPWindowBytes},
		{"ios above ceiling is clamped", "ios", 131072, iOSMaxTCPWindowBytes},
		{"ios the exact crash-reproducing value is clamped", "ios", 131253, iOSMaxTCPWindowBytes},
		{"darwin above the ios ceiling passes through unclamped", "darwin", 131072, 131072},
		{"linux above the ios ceiling passes through unclamped", "linux", 131072, 131072},
		{"windows above the ios ceiling passes through unclamped", "windows", 131072, 131072},
		// 32 KB was the previous ceiling, picked from an estimate of 150
		// connections before the device could count them. Measurement found 208
		// concurrent connections at the kill, where 32 KB costs 13.6 MB against
		// 20 KB's 8.5 MB -- roughly 5 MB inside a budget that had about 3 MB of
		// headroom left. This case is what fails if that raise comes back.
		{"ios the previous 32KB ceiling is now clamped", "ios", 32 * 1024, iOSMaxTCPWindowBytes},
	}

	// Pinned numerically as well as symbolically: every case above compares
	// against the constant, so they would all still pass if the ceiling were
	// raised again. Raising it has to be a deliberate edit here.
	if iOSMaxTCPWindowBytes != 20*1024 {
		t.Fatalf("iOSMaxTCPWindowBytes = %d, want 20480 (gVisor's stock value). "+
			"Raising it spends ~2 KB * 2 * concurrent-connections of a ~50 MB process budget; "+
			"the last raise to 32 KB contributed to a SIGKILL at 47.2 MB footprint.",
			iOSMaxTCPWindowBytes)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clampTCPWindowBytesForGOOS(c.goos, c.in)
			if got != c.want {
				t.Fatalf("clampTCPWindowBytesForGOOS(%q, %d) = %d, want %d", c.goos, c.in, got, c.want)
			}
		})
	}
}

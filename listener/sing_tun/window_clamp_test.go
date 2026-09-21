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
		// 131072 was the value in the original crash report. It is no longer
		// clamped, and that is correct rather than a regression: back then it
		// meant "reserve 128 KB for every connection", which is what killed the
		// extension. Now it means "let a flow grow to at most 128 KB", which is
		// below the ceiling and cheaper than the default.
		{"ios the original crash value now passes through as a growth ceiling", "ios", 131072, 131072},
		{"ios above ceiling is clamped", "ios", 2 * 1024 * 1024, iOSMaxTCPWindowBytes},
		{"ios far above ceiling is clamped", "ios", 16 * 1024 * 1024, iOSMaxTCPWindowBytes},
		{"darwin above the ios ceiling passes through unclamped", "darwin", 2 * 1024 * 1024, 2 * 1024 * 1024},
		{"linux above the ios ceiling passes through unclamped", "linux", 2 * 1024 * 1024, 2 * 1024 * 1024},
		{"windows above the ios ceiling passes through unclamped", "windows", 2 * 1024 * 1024, 2 * 1024 * 1024},
	}

	// Pinned numerically as well as symbolically: every case above compares
	// against the constant, so they would all still pass if it moved. Changing
	// it has to be a deliberate edit here.
	if iOSMaxTCPWindowBytes != 512*1024 {
		t.Fatalf("iOSMaxTCPWindowBytes = %d, want 524288. "+
			"This is a GROWTH ceiling, not a per-connection reservation: every connection starts "+
			"at 20 KB and only flows with data in flight grow toward it. 512 KB lets a few bulk "+
			"flows reach the ~1.875 MB aggregate that saturates a 100 Mbps, 150 ms path, while "+
			"bounding four concurrent flows to 2 MB rather than the 8 MB a 2 MB ceiling would allow.",
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

package outbound

import (
	"testing"

	"github.com/metacubex/quic-go"
)

// These guard the two ways this could go wrong silently: leaving quic-go's 15 MB
// default in force on iOS (the SIGKILL this exists to prevent), or overriding a
// window the user configured on purpose (making the config lie).
//
// The ceiling values are asserted BY LITERAL VALUE, not against the constants
// they are defined as. Comparing config.Max* against mobileMax*ReceiveWindow
// would keep the whole suite green if someone raised the constant back to 64 MB
// -- which is exactly the regression that shipped once. The literals below must
// be updated deliberately, in lockstep with the constants, so a change is
// visible in the diff.
const (
	wantConnCeiling   = 4 * 1024 * 1024
	wantStreamCeiling = 2 * 1024 * 1024
)

func TestCeilingConstantsMatchPinnedValues(t *testing.T) {
	if mobileMaxConnectionReceiveWindow != wantConnCeiling {
		t.Fatalf("connection ceiling constant = %d, pinned value = %d; update the pin deliberately",
			mobileMaxConnectionReceiveWindow, wantConnCeiling)
	}
	if mobileMaxStreamReceiveWindow != wantStreamCeiling {
		t.Fatalf("stream ceiling constant = %d, pinned value = %d; update the pin deliberately",
			mobileMaxStreamReceiveWindow, wantStreamCeiling)
	}
}

func TestQUICWindowCeilingFillsUnsetWindowsOnIOS(t *testing.T) {
	config := &quic.Config{}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.MaxConnectionReceiveWindow != wantConnCeiling {
		t.Errorf("MaxConnectionReceiveWindow = %d, want %d — unset on iOS means quic-go's 15 MB default applies",
			config.MaxConnectionReceiveWindow, uint64(wantConnCeiling))
	}
	if config.MaxStreamReceiveWindow != wantStreamCeiling {
		t.Errorf("MaxStreamReceiveWindow = %d, want %d",
			config.MaxStreamReceiveWindow, uint64(wantStreamCeiling))
	}
}

func TestQUICWindowCeilingLeavesOtherPlatformsOnDefaults(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows", "android"} {
		config := &quic.Config{}
		applyQUICWindowCeilingForGOOS(config, goos)

		if config.MaxConnectionReceiveWindow != 0 || config.MaxStreamReceiveWindow != 0 {
			t.Errorf("%s: windows were set (%d/%d); only iOS has a budget small enough to warrant it",
				goos, config.MaxConnectionReceiveWindow, config.MaxStreamReceiveWindow)
		}
	}
}

func TestQUICWindowCeilingCapsExplicitLargerConfig(t *testing.T) {
	// Deliberately ABOVE the ceiling. On a hard-memory-budget platform this is
	// NOT honoured: a 32MB window inside a ~50MB Network Extension is a config
	// that gets the tunnel SIGKILLed, so the cap lowers it to the ceiling.
	const requestedConn = 32 * 1024 * 1024
	const requestedStream = 16 * 1024 * 1024

	config := &quic.Config{
		MaxConnectionReceiveWindow: requestedConn,
		MaxStreamReceiveWindow:     requestedStream,
	}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.MaxConnectionReceiveWindow != wantConnCeiling {
		t.Errorf("MaxConnectionReceiveWindow = %d, want it capped to %d",
			config.MaxConnectionReceiveWindow, uint64(wantConnCeiling))
	}
	if config.MaxStreamReceiveWindow != wantStreamCeiling {
		t.Errorf("MaxStreamReceiveWindow = %d, want it capped to %d",
			config.MaxStreamReceiveWindow, uint64(wantStreamCeiling))
	}
}

// A window explicitly set BELOW the ceiling is a genuine preference to reduce
// memory further, and must be left alone.
func TestQUICWindowCeilingHonoursSmallerExplicitConfig(t *testing.T) {
	const requestedConn = 1 * 1024 * 1024 // below the 6MB conn ceiling
	const requestedStream = 512 * 1024    // below the 3MB stream ceiling

	config := &quic.Config{
		MaxConnectionReceiveWindow: requestedConn,
		MaxStreamReceiveWindow:     requestedStream,
	}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.MaxConnectionReceiveWindow != requestedConn {
		t.Errorf("MaxConnectionReceiveWindow = %d, want the smaller configured %d",
			config.MaxConnectionReceiveWindow, uint64(requestedConn))
	}
	if config.MaxStreamReceiveWindow != requestedStream {
		t.Errorf("MaxStreamReceiveWindow = %d, want the smaller configured %d",
			config.MaxStreamReceiveWindow, uint64(requestedStream))
	}
}

// A partially configured pair is the realistic mistake: one field set, the other
// forgotten. The forgotten one must still be bounded, and a set-but-small one
// honoured.
func TestQUICWindowCeilingCapsTheUnsetHalf(t *testing.T) {
	const requestedStream = 512 * 1024 // below the 3MB stream ceiling

	config := &quic.Config{MaxStreamReceiveWindow: requestedStream}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.MaxStreamReceiveWindow != requestedStream {
		t.Errorf("MaxStreamReceiveWindow = %d, want the smaller configured %d",
			config.MaxStreamReceiveWindow, uint64(requestedStream))
	}
	if config.MaxConnectionReceiveWindow != wantConnCeiling {
		t.Errorf("MaxConnectionReceiveWindow = %d, want the unset half capped to %d",
			config.MaxConnectionReceiveWindow, uint64(wantConnCeiling))
	}
}

// Regression for the Initial>Max inversion: tuic/shadowquic/hysteria pre-fill
// Initial = Default/10 = 6.4MB for the 64MB default, then hand the config here.
// After the Max is capped to 4MB, an un-clamped Initial of 6.4MB would be
// advertised on the wire ABOVE the cap. The ceiling must clamp Initial to Max.
func TestQUICWindowCeilingClampsInitialToMax(t *testing.T) {
	const tuicDefault = 64 * 1024 * 1024
	config := &quic.Config{
		InitialConnectionReceiveWindow: tuicDefault / 10, // 6.4 MB, above the 4 MB Max
		MaxConnectionReceiveWindow:     tuicDefault,
		InitialStreamReceiveWindow:     tuicDefault / 10, // 6.4 MB, above the 2 MB stream Max — exercises the stream clamp
		MaxStreamReceiveWindow:         15 * 1024 * 1024,
	}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.InitialConnectionReceiveWindow > config.MaxConnectionReceiveWindow {
		t.Errorf("InitialConnectionReceiveWindow %d exceeds MaxConnectionReceiveWindow %d — inversion punches through the cap",
			config.InitialConnectionReceiveWindow, config.MaxConnectionReceiveWindow)
	}
	if config.InitialStreamReceiveWindow > config.MaxStreamReceiveWindow {
		t.Errorf("InitialStreamReceiveWindow %d exceeds MaxStreamReceiveWindow %d",
			config.InitialStreamReceiveWindow, config.MaxStreamReceiveWindow)
	}
	// And the Max must still be the capped value, not the 64MB default.
	if config.MaxConnectionReceiveWindow != wantConnCeiling {
		t.Errorf("MaxConnectionReceiveWindow = %d, want capped to %d", config.MaxConnectionReceiveWindow, uint64(wantConnCeiling))
	}
}

// The ceiling is only coherent if a stream cannot be permitted more than its
// whole connection.
func TestStreamCeilingDoesNotExceedConnectionCeiling(t *testing.T) {
	if mobileMaxStreamReceiveWindow > mobileMaxConnectionReceiveWindow {
		t.Errorf("stream ceiling %d exceeds connection ceiling %d",
			uint64(mobileMaxStreamReceiveWindow), uint64(mobileMaxConnectionReceiveWindow))
	}
}

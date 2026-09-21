package outbound

import (
	"testing"

	"github.com/metacubex/quic-go"
)

// These guard the two ways this could go wrong silently: leaving quic-go's 15 MB
// default in force on iOS (the SIGKILL this exists to prevent), or overriding a
// window the user configured on purpose (making the config lie).

func TestQUICWindowCeilingFillsUnsetWindowsOnIOS(t *testing.T) {
	config := &quic.Config{}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.MaxConnectionReceiveWindow != mobileMaxConnectionReceiveWindow {
		t.Errorf("MaxConnectionReceiveWindow = %d, want %d — unset on iOS means quic-go's 15 MB default applies",
			config.MaxConnectionReceiveWindow, uint64(mobileMaxConnectionReceiveWindow))
	}
	if config.MaxStreamReceiveWindow != mobileMaxStreamReceiveWindow {
		t.Errorf("MaxStreamReceiveWindow = %d, want %d",
			config.MaxStreamReceiveWindow, uint64(mobileMaxStreamReceiveWindow))
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

	if config.MaxConnectionReceiveWindow != mobileMaxConnectionReceiveWindow {
		t.Errorf("MaxConnectionReceiveWindow = %d, want it capped to %d",
			config.MaxConnectionReceiveWindow, uint64(mobileMaxConnectionReceiveWindow))
	}
	if config.MaxStreamReceiveWindow != mobileMaxStreamReceiveWindow {
		t.Errorf("MaxStreamReceiveWindow = %d, want it capped to %d",
			config.MaxStreamReceiveWindow, uint64(mobileMaxStreamReceiveWindow))
	}
}

// A window explicitly set BELOW the ceiling is a genuine preference to reduce
// memory further, and must be left alone.
func TestQUICWindowCeilingHonoursSmallerExplicitConfig(t *testing.T) {
	const requestedConn = 1 * 1024 * 1024  // below the 4MB ceiling
	const requestedStream = 512 * 1024     // below the 2MB ceiling

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
	const requestedStream = 1 * 1024 * 1024 // below the 2MB stream ceiling

	config := &quic.Config{MaxStreamReceiveWindow: requestedStream}
	applyQUICWindowCeilingForGOOS(config, "ios")

	if config.MaxStreamReceiveWindow != requestedStream {
		t.Errorf("MaxStreamReceiveWindow = %d, want the smaller configured %d",
			config.MaxStreamReceiveWindow, uint64(requestedStream))
	}
	if config.MaxConnectionReceiveWindow != mobileMaxConnectionReceiveWindow {
		t.Errorf("MaxConnectionReceiveWindow = %d, want the unset half capped to %d",
			config.MaxConnectionReceiveWindow, uint64(mobileMaxConnectionReceiveWindow))
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

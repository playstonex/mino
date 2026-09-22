package tunnel

import (
	"testing"
)

func TestNewTCPConnSemIsIOSOnly(t *testing.T) {
	if sem := newTCPConnSem("darwin"); sem != nil {
		t.Fatalf("expected nil semaphore on darwin (no cap), got cap=%d", cap(sem))
	}
	if sem := newTCPConnSem("linux"); sem != nil {
		t.Fatalf("expected nil semaphore on linux (no cap), got cap=%d", cap(sem))
	}
	if sem := newTCPConnSem("android"); sem != nil {
		t.Fatalf("expected nil semaphore on android (VpnService has no comparable ceiling), got cap=%d", cap(sem))
	}
	sem := newTCPConnSem("ios")
	if sem == nil {
		t.Fatal("expected a bounded semaphore on ios, got nil")
	}
	if cap(sem) != iOSMaxConcurrentTCPConns {
		t.Fatalf("ios semaphore cap = %d, want %d", cap(sem), iOSMaxConcurrentTCPConns)
	}
}

func TestTCPConnSemCeilingIsBoundedNotUnlimited(t *testing.T) {
	// The whole point is a FINITE ceiling. A zero or absurdly large value would
	// silently reintroduce the unbounded-multiplier bug this file exists to fix.
	if iOSMaxConcurrentTCPConns <= 0 {
		t.Fatalf("ceiling must be positive, got %d", iOSMaxConcurrentTCPConns)
	}
	if iOSMaxConcurrentTCPConns > 1024 {
		t.Fatalf("ceiling %d is too large to bound the ~50MB budget; a saturating "+
			"connection is order ~0.5-1MB", iOSMaxConcurrentTCPConns)
	}
}

func TestTCPConnSemFillsToCapacityThenReleaseUnblocks(t *testing.T) {
	sem := newTCPConnSem("ios")
	// Fill every slot.
	for i := 0; i < iOSMaxConcurrentTCPConns; i++ {
		select {
		case sem <- struct{}{}:
		default:
			t.Fatalf("slot %d should have been free but acquire would block", i)
		}
	}
	// The next acquire must block (backpressure, not drop).
	select {
	case sem <- struct{}{}:
		t.Fatal("acquiring past the ceiling should block, but it succeeded")
	default:
	}
	// Releasing one frees exactly one slot.
	<-sem
	select {
	case sem <- struct{}{}:
	default:
		t.Fatal("after releasing a slot a new acquire should succeed")
	}
}

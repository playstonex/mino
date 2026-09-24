package mate

import (
	"testing"
	"time"
)

// unreachableRelay is in RFC 5737 TEST-NET-1, guaranteed not to be routed.
// Using a literal IP (not a hostname) keeps the test off DNS: p2p.NewRelayClient
// "connects" the UDP socket instantly and then blocks in register() waiting for
// an ack until its 5s read deadline expires. That stall is precisely the window
// these tests care about.
const (
	unreachableRelay    = "192.0.2.1:9091"
	unreachableRelayAlt = "192.0.2.2:9091"
	// NewRelayClient parses the device id as a UUID before it ever reaches the
	// network, so an arbitrary string would fail fast and the test would never
	// enter the blocking window it is meant to cover.
	testDeviceUUID = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	// How long the stub platform stalls inside the dial. Long enough that a lock
	// held across it is unmistakable, short enough to keep the test quick.
	dialStall = 1500 * time.Millisecond
)

// setRelayConfig installs a relay config WITHOUT going through Configure(), which
// would also spawn a background dial and make these tests non-deterministic.
// It mirrors exactly what Configure does to the guarded fields.
func (m *overlayTransportManager) setRelayConfig(endpoint, token, deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relayEndpoint = endpoint
	m.accessToken = token
	m.localDeviceID = deviceID
	m.configEpoch++
	m.relayFailCount = 0
	m.relayLastFail = time.Time{}
}

func (m *overlayTransportManager) currentConfigEpoch() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.configEpoch
}

// TestEnsureRelayDoesNotHoldLockAcrossDial is the regression guard for the
// self-deadlock introduced by commit 2c4fe8b1 and for the lock starvation it
// mutated into afterwards.
//
// The original bug: ensureRelay held m.mu.Lock() (write) and, inside that
// critical section, called m.buildSocketProtector(), which takes m.mu.RLock().
// Go's sync.RWMutex is NOT reentrant, so that self-deadlocked FOREVER — the
// overlay's Start() never returned, MateStartOverlay never returned to Swift,
// startTunnel's completionHandler(nil) was never called, and NEVPNStatus sat at
// "Connecting" until the app timed out and tore the tunnel down.
//
// Moving the dial to a background goroutine (without fixing the lock scope) only
// changed the symptom: the deadlocked goroutine still owned m.mu, so
// SetPacketHandler() — the last call in wireP2PCallbacks() — blocked instead,
// hanging Start() in the same place.
//
// The invariant: while ensureRelay is dialing, every other m.mu user must stay
// responsive. If this test hangs or fails, a blocking call has been pulled back
// inside the lock.
func TestEnsureRelayDoesNotHoldLockAcrossDial(t *testing.T) {
	m := newOverlayTransportManager()
	// A platform must be set, otherwise buildSocketProtector returns early and
	// would not reproduce the original reentrant RLock at all. This one also
	// stalls inside the dial (SocketProtect is invoked by NewRelayClient), so the
	// blocking window is deterministic and needs no reachable network.
	m.SetPlatform(stallingPlatform{})
	m.setRelayConfig(unreachableRelay, "test-token", testDeviceUUID)

	dialStarted := time.Now()
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		// Expected to fail (nothing answers), but it MUST return.
		_, _ = m.ensureRelay()
	}()

	// Let the dial reach the stall inside SocketProtect.
	time.Sleep(300 * time.Millisecond)

	calls := []struct {
		name string
		fn   func()
	}{
		{"SetPacketHandler", func() { m.SetPacketHandler(func(string, []byte) {}) }},
		{"RegisterPeer", func() { _ = m.RegisterPeer("peer-a") }},
		{"HasPacketConn", func() { _ = m.HasPacketConn("peer-a") }},
		{"SetPlatform", func() { m.SetPlatform(stallingPlatform{}) }},
		{"logf", func() { m.logf("probe") }},
	}
	for _, c := range calls {
		returned := make(chan struct{})
		go func(fn func()) {
			fn()
			close(returned)
		}(c.fn)

		select {
		case <-returned:
		case <-time.After(1 * time.Second):
			t.Fatalf("%s blocked while ensureRelay was dialing: m.mu is held across the "+
				"blocking p2p.NewRelayClient call (regression of 2c4fe8b1)", c.name)
		}
	}

	// Guard the guard: if the dial had already finished, the probes above proved
	// nothing. They must have run inside the stall window.
	select {
	case <-dialDone:
		t.Fatalf("the dial finished in %v, before the probes ran — the test never "+
			"entered the blocking window it is meant to cover", time.Since(dialStarted))
	default:
	}

	select {
	case <-dialDone:
	case <-time.After(20 * time.Second):
		t.Fatal("ensureRelay never returned: the dial is deadlocked, not merely slow")
	}
}

// TestEnsureRelayDiscardsStaleClientAfterReconfigure guards the stale-write race
// the unlocked dial opens up: the dial takes seconds, and the relay may be
// re-pointed (background multi-region selection) or torn down (Reset) while it is
// in flight. A late result must not overwrite the newer config.
func TestEnsureRelayDiscardsStaleClientAfterReconfigure(t *testing.T) {
	m := newOverlayTransportManager()
	m.SetPlatform(stallingPlatform{})
	m.setRelayConfig(unreachableRelay, "test-token", testDeviceUUID)

	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		_, _ = m.ensureRelay()
	}()

	time.Sleep(300 * time.Millisecond)

	before := m.currentConfigEpoch()
	m.setRelayConfig(unreachableRelayAlt, "test-token", testDeviceUUID)
	if after := m.currentConfigEpoch(); after == before {
		t.Fatalf("reconfigure did not bump configEpoch (still %d)", after)
	}

	select {
	case <-dialDone:
	case <-time.After(20 * time.Second):
		t.Fatal("ensureRelay never returned")
	}

	m.mu.RLock()
	stored := m.relayClient
	endpoint := m.relayEndpoint
	m.mu.RUnlock()
	if stored != nil {
		t.Fatal("a relay client belonging to the superseded config was installed")
	}
	if endpoint != unreachableRelayAlt {
		t.Fatalf("relayEndpoint was clobbered by the stale dial: got %q, want %q", endpoint, unreachableRelayAlt)
	}
}

// TestConfigureBumpsEpochOnChange pins the contract setRelayConfig models: a
// Configure that changes the endpoint must invalidate in-flight dials, and one
// that changes nothing must not.
func TestConfigureBumpsEpochOnChange(t *testing.T) {
	m := newOverlayTransportManager()

	// Empty config: Configure returns before spawning any background dial.
	if err := m.Configure("", "", ""); err != nil {
		t.Fatalf("Configure(empty): %v", err)
	}
	first := m.currentConfigEpoch()

	if err := m.Configure("", "", ""); err != nil {
		t.Fatalf("Configure(empty, repeat): %v", err)
	}
	if again := m.currentConfigEpoch(); again != first {
		t.Fatalf("an unchanged Configure bumped configEpoch (%d -> %d)", first, again)
	}
}

// TestResetBumpsConfigEpoch ensures teardown also invalidates in-flight dials, so
// a late client cannot resurrect a dispatch path into a destroyed handler.
func TestResetBumpsConfigEpoch(t *testing.T) {
	m := newOverlayTransportManager()
	m.setRelayConfig(unreachableRelay, "test-token", "test-device")
	before := m.currentConfigEpoch()
	m.Reset()
	if after := m.currentConfigEpoch(); after == before {
		t.Fatalf("Reset did not bump configEpoch (still %d)", after)
	}
}

// noopPlatform is a do-nothing PlatformInterface. Only SocketProtect and WriteLog
// are exercised; the rest exist to satisfy the interface.
type noopPlatform struct{}

func (noopPlatform) UsePlatformAutoDetectInterfaceControl() bool { return false }
func (noopPlatform) AutoDetectInterfaceControl(int32) error      { return nil }
func (noopPlatform) OpenTun(TunOptions) (int32, error)           { return -1, nil }
func (noopPlatform) WriteLog(string)                             {}
func (noopPlatform) UseProcFS() bool                             { return false }
func (noopPlatform) FindConnectionOwner(int32, string, int32, string, int32) (int32, error) {
	return -1, nil
}
func (noopPlatform) PackageNameByUid(int32) (string, error)                     { return "", nil }
func (noopPlatform) UIDByPackageName(string) (int32, error)                     { return -1, nil }
func (noopPlatform) StartDefaultInterfaceMonitor(InterfaceUpdateListener) error { return nil }
func (noopPlatform) CloseDefaultInterfaceMonitor(InterfaceUpdateListener) error { return nil }
func (noopPlatform) GetInterfaces() (NetworkInterfaceIterator, error)           { return nil, nil }
func (noopPlatform) UnderNetworkExtension() bool                                { return false }
func (noopPlatform) IncludeAllNetworks() bool                                   { return false }
func (noopPlatform) ReadWIFIState() *WIFIState                                  { return nil }
func (noopPlatform) SystemCertificates() StringIterator                         { return nil }
func (noopPlatform) ClearDNSCache()                                             {}
func (noopPlatform) SendNotification(*Notification) error                       { return nil }
func (noopPlatform) SocketProtect(int32) bool                                   { return true }
func (noopPlatform) AcquireProtectedSocket() int32                              { return -1 }
func (noopPlatform) OnOverlayStateChange(string)                                {}

// stallingPlatform blocks inside SocketProtect, which NewRelayClient invokes on
// the socket it just created. That makes the dial deterministically slow with no
// network dependency, so the "no lock held across the dial" assertions have a
// real window to run in instead of racing a dial that already returned.
type stallingPlatform struct{ noopPlatform }

func (stallingPlatform) SocketProtect(int32) bool {
	time.Sleep(dialStall)
	return true
}

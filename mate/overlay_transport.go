package mate

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/metacubex/mihomo/transport/p2p"
)

var globalOverlayTransport = newOverlayTransportManager()

type overlayTransportManager struct {
	mu sync.RWMutex

	// relayCreateMu serializes relay-client creation so a slow, blocking
	// NewRelayClient dial (DNS + 5s register deadline) runs without holding
	// `mu`. Holding `mu` across that dial blocked SetPacketHandler/RegisterPeer
	// during overlay startup and left NEVPNStatus stuck at "Connecting".
	relayCreateMu sync.Mutex

	// configEpoch is bumped by every Configure() that changes the relay config
	// and by Reset(). ensureRelay() snapshots it before its unlocked dial and
	// re-checks it before storing the result, so a slow dial (up to ~5s) cannot
	// resurrect a relay client for a superseded endpoint or for an already
	// torn-down manager.
	configEpoch uint64

	platform      PlatformInterface
	relayEndpoint string
	accessToken   string
	localDeviceID string

	relayClient *p2p.RelayClient
	peers       map[string]struct{}

	packetConns map[string]net.PacketConn

	// packetHandler is called by dispatchPacket when set.
	// The overlay manager sets this to handle decrypt + TUN write internally,
	// replacing the old Swift callback (OnOverlayPacket).
	packetHandler func(peerID string, payload []byte)

	// Relay backoff tracking (H2)
	relayFailCount int
	relayLastFail  time.Time

	// readLoop cancellation (H3)
	readLoopCancel map[string]chan struct{}
}

func newOverlayTransportManager() *overlayTransportManager {
	return &overlayTransportManager{
		peers:          make(map[string]struct{}),
		packetConns:    make(map[string]net.PacketConn),
		readLoopCancel: make(map[string]chan struct{}),
	}
}

func (m *overlayTransportManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel all readLoop goroutines before closing conns (H3)
	for peerID, cancel := range m.readLoopCancel {
		close(cancel)
		delete(m.readLoopCancel, peerID)
	}

	for peerID, conn := range m.packetConns {
		_ = conn.Close()
		delete(m.packetConns, peerID)
	}

	if m.relayClient != nil {
		_ = m.relayClient.Close()
		m.relayClient = nil
	}

	m.platform = nil
	m.relayEndpoint = ""
	m.accessToken = ""
	m.localDeviceID = ""
	m.configEpoch++
	m.peers = make(map[string]struct{})
	m.packetHandler = nil
	m.relayFailCount = 0
	m.relayLastFail = time.Time{}
	m.readLoopCancel = make(map[string]chan struct{})
}

func (m *overlayTransportManager) SetPlatform(platform PlatformInterface) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.platform = platform
}

// buildSocketProtector returns a function that protects a socket fd via the
// platform (Android VpnService.protect) so relay/control traffic bypasses the
// TUN. Returns nil when no platform is available (overlay-only mode on Android
// does not need this since the TUN only routes the overlay subnet).
func (m *overlayTransportManager) buildSocketProtector() func(fd int) error {
	m.mu.RLock()
	platform := m.platform
	m.mu.RUnlock()
	if platform == nil {
		return nil
	}
	return func(fd int) error {
		if !platform.SocketProtect(int32(fd)) {
			return fmt.Errorf("SocketProtect returned false for fd %d", fd)
		}
		return nil
	}
}

// SetPacketHandler sets the callback for handling received overlay packets.
func (m *overlayTransportManager) SetPacketHandler(handler func(peerID string, payload []byte)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packetHandler = handler
}

func (m *overlayTransportManager) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

	m.mu.RLock()
	platform := m.platform
	m.mu.RUnlock()

	if platform != nil {
		platform.WriteLog(msg)
		return
	}

	fmt.Println(msg)
}

func (m *overlayTransportManager) Configure(relayEndpoint string, accessToken string, localDeviceID string) error {
	m.mu.Lock()

	configChanged := m.relayEndpoint != relayEndpoint ||
		m.accessToken != accessToken ||
		m.localDeviceID != localDeviceID

	m.relayEndpoint = relayEndpoint
	m.accessToken = accessToken
	m.localDeviceID = localDeviceID

	if configChanged && m.relayClient != nil {
		_ = m.relayClient.Close()
		m.relayClient = nil
	}
	if configChanged {
		// Invalidate any dial already in flight for the previous endpoint so it
		// cannot overwrite this config when it eventually returns.
		m.configEpoch++
		m.relayFailCount = 0
		m.relayLastFail = time.Time{}
	}

	m.mu.Unlock()

	if relayEndpoint == "" || accessToken == "" || localDeviceID == "" {
		return nil
	}

	// Do NOT synchronously establish the relay here. p2p.NewRelayClient dials
	// and handshakes the relay endpoint, and on networks where that endpoint is
	// unreachable it blocks — and because Configure() is on the synchronous
	// MateStartOverlay -> Start() path, that stall kept the NE from ever
	// returning from startTunnel, so NEVPNStatus was stuck at "Connecting"
	// forever even though the overlay TUN was already up. The relay is only
	// needed as a FALLBACK transport for actual peer traffic; Send()/ensureRelay()
	// create it lazily on first use (with backoff). Establish it in the
	// background so a reachable relay is ready ahead of time, without blocking
	// startup on an unreachable one.
	go func() {
		if _, err := m.ensureRelay(); err != nil {
			m.logf("[OverlayTransport] background relay setup deferred: %v", err)
			return
		}
		m.logf("[OverlayTransport] relay ready for local device %s via %s", localDeviceID, relayEndpoint)
	}()
	return nil
}

func (m *overlayTransportManager) RegisterPeer(peerID string) error {
	m.mu.Lock()
	m.peers[peerID] = struct{}{}
	relayClient := m.relayClient
	m.mu.Unlock()

	if relayClient != nil {
		if err := relayClient.AddPeer(peerID); err != nil {
			return err
		}
	}
	m.logf("[OverlayTransport] registered peer %s for relay fallback", peerID)
	return nil
}

func (m *overlayTransportManager) AttachPeerPacketConn(peerID string, conn net.PacketConn) {
	_ = m.RegisterPeer(peerID)

	m.mu.Lock()
	// Cancel existing readLoop for this peer (H3)
	if cancel, ok := m.readLoopCancel[peerID]; ok {
		close(cancel)
		delete(m.readLoopCancel, peerID)
	}
	if existing := m.packetConns[peerID]; existing != nil && existing != conn {
		_ = existing.Close()
	}
	m.packetConns[peerID] = conn
	cancel := make(chan struct{})
	m.readLoopCancel[peerID] = cancel
	m.mu.Unlock()

	m.logf("[OverlayTransport] attached direct packet channel for peer %s", peerID)
	go m.readLoop(peerID, conn, cancel)
}

// HasPacketConn returns true if a direct packet channel is registered for the peer.
func (m *overlayTransportManager) HasPacketConn(peerID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.packetConns[peerID]
	return ok
}

func (m *overlayTransportManager) Send(peerID string, payload []byte) error {
	m.mu.RLock()
	_, knownPeer := m.peers[peerID]
	m.mu.RUnlock()
	if !knownPeer {
		if err := m.RegisterPeer(peerID); err != nil {
			return err
		}
	}

	// Take a reference to the direct packet channel and RELEASE the lock before
	// writing. An earlier "C1 fix" held RLock across WriteTo so readLoop could not
	// close the conn between lookup and write — but that put network I/O inside the
	// critical section, which is the same anti-pattern that deadlocked overlay
	// startup (a lock held across a blocking call starves every writer:
	// SetPacketHandler, Configure, Reset). It is also unnecessary: writing to a
	// conn another goroutine just closed returns an error, it does not panic, and
	// that error is exactly the signal to fall through to the relay below.
	m.mu.RLock()
	conn := m.packetConns[peerID]
	m.mu.RUnlock()
	if conn != nil {
		// addr is ignored: every conn attached here is a *p2p.PacketDataChannelConn
		// whose WriteTo sends on the WebRTC DataChannel and discards the address.
		if _, err := conn.WriteTo(payload, nil); err == nil {
			return nil
		}
		// direct send failed (closed conn, DataChannel down) — fall through to relay
	}

	relayClient, err := m.ensureRelay()
	if err != nil {
		m.logf("[OverlayTransport] relay fallback failed for %s: %v", peerID, err)
		return err
	}

	return relayClient.SendToPeer(peerID, payload)
}

func (m *overlayTransportManager) ensureRelay() (*p2p.RelayClient, error) {
	// Fast path: relay already exists
	m.mu.RLock()
	if m.relayClient != nil {
		relayClient := m.relayClient
		m.mu.RUnlock()
		return relayClient, nil
	}
	m.mu.RUnlock()

	// Slow path. p2p.NewRelayClient does a DNS resolve + a register() with a
	// 5s read deadline, so it can block for seconds when the relay endpoint is
	// unreachable. We must NOT hold m.mu across it: SetPacketHandler(),
	// RegisterPeer(), logf() and Send() all take m.mu, and blocking them for
	// 5s during startup was exactly what stalled wireP2PCallbacks ->
	// SetPacketHandler, kept MateStartOverlay from returning, and left
	// NEVPNStatus stuck at "Connecting". Serialize creation with a dedicated
	// mutex instead, and only touch m.mu for the brief snapshot and store.
	m.relayCreateMu.Lock()
	defer m.relayCreateMu.Unlock()

	// Double-check: another creator may have finished while we waited.
	m.mu.RLock()
	if m.relayClient != nil {
		relayClient := m.relayClient
		m.mu.RUnlock()
		return relayClient, nil
	}
	relayEndpoint := m.relayEndpoint
	accessToken := m.accessToken
	localDeviceID := m.localDeviceID
	epoch := m.configEpoch
	failCount := m.relayFailCount
	lastFail := m.relayLastFail
	peerIDs := make([]string, 0, len(m.peers))
	for peerID := range m.peers {
		peerIDs = append(peerIDs, peerID)
	}
	m.mu.RUnlock()

	// H2: Exponential backoff on relay creation failures
	if failCount > 0 && !lastFail.IsZero() {
		backoff := time.Duration(1<<min(failCount-1, 6)) * time.Second // 1s, 2s, 4s, ... 64s
		if time.Since(lastFail) < backoff {
			return nil, fmt.Errorf("relay creation in backoff (%v remaining)", backoff-time.Since(lastFail))
		}
	}

	if relayEndpoint == "" || accessToken == "" || localDeviceID == "" {
		return nil, fmt.Errorf("overlay relay is not configured")
	}

	// Blocking dial + register happens WITHOUT m.mu held.
	relayClient, err := p2p.NewRelayClient(relayEndpoint, accessToken, localDeviceID, m.buildSocketProtector())
	if err != nil {
		m.mu.Lock()
		// Only record the failure against the config it actually belongs to.
		if m.configEpoch == epoch {
			m.relayFailCount++
			m.relayLastFail = time.Now()
		}
		m.mu.Unlock()
		return nil, err
	}

	relayClient.SetReceiveHandler(func(peerID string, data []byte) {
		m.dispatchPacket(peerID, data)
	})

	for _, peerID := range peerIDs {
		if err := relayClient.AddPeer(peerID); err != nil {
			_ = relayClient.Close()
			m.mu.Lock()
			if m.configEpoch == epoch {
				m.relayFailCount++
				m.relayLastFail = time.Now()
			}
			m.mu.Unlock()
			return nil, err
		}
	}

	m.mu.Lock()
	// The dial ran unlocked and may have taken seconds. If Configure() re-pointed
	// the relay or Reset() tore the manager down meanwhile, this client belongs to
	// a config that no longer exists: discard it instead of resurrecting a stale
	// endpoint (and, after Reset, a dispatch path into a destroyed handler).
	if m.configEpoch != epoch {
		m.mu.Unlock()
		_ = relayClient.Close()
		return nil, fmt.Errorf("overlay relay config changed during dial; discarded stale client for %s", relayEndpoint)
	}
	// Another creator winning the race is not possible here (relayCreateMu is
	// still held), but Reset() could have nil'd the field without bumping past
	// our epoch check — keep the store unconditional and epoch-gated.
	m.relayClient = relayClient
	m.relayFailCount = 0
	m.relayLastFail = time.Time{}
	m.mu.Unlock()
	return relayClient, nil
}

func (m *overlayTransportManager) readLoop(peerID string, conn net.PacketConn, cancel <-chan struct{}) {
	buf := make([]byte, 65535)
	for {
		select {
		case <-cancel:
			return
		default:
		}
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		m.dispatchPacket(peerID, append([]byte(nil), buf[:n]...))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.packetConns[peerID]; current == conn {
		delete(m.packetConns, peerID)
	}
	delete(m.readLoopCancel, peerID)
}

func (m *overlayTransportManager) dispatchPacket(peerID string, payload []byte) {
	m.mu.RLock()
	handler := m.packetHandler
	m.mu.RUnlock()

	if handler != nil {
		handler(peerID, payload)
	}
}

func ConfigureOverlayTransport(relayEndpoint string, accessToken string, localDeviceID string) error {
	return globalOverlayTransport.Configure(relayEndpoint, accessToken, localDeviceID)
}

func RegisterOverlayPeer(peerID string) error {
	return globalOverlayTransport.RegisterPeer(peerID)
}

func SendOverlayPacket(peerID string, payload []byte) error {
	return globalOverlayTransport.Send(peerID, payload)
}

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

	m.mu.Unlock()

	if relayEndpoint == "" || accessToken == "" || localDeviceID == "" {
		return nil
	}

	if _, err := m.ensureRelay(); err != nil {
		return err
	}

	m.logf("[OverlayTransport] relay ready for local device %s via %s", localDeviceID, relayEndpoint)
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
	if err := m.RegisterPeer(peerID); err != nil {
		return err
	}

	// C1 fix: hold RLock during both packetConn lookup AND WriteTo to prevent
	// readLoop from closing the conn between lookup and write.
	m.mu.RLock()
	conn := m.packetConns[peerID]
	if conn != nil {
		_, err := conn.WriteTo(payload, &net.UDPAddr{})
		m.mu.RUnlock()
		if err == nil {
			return nil
		}
		// direct send failed, fall through to relay
	} else {
		m.mu.RUnlock()
	}

	relayClient, err := m.ensureRelay()
	if err != nil {
		m.logf("[OverlayTransport] relay fallback failed for %s: %v", peerID, err)
		return err
	}

	return relayClient.SendToPeer(peerID, payload)
}

// packetConn is no longer used — Send() now holds RLock during lookup+write (C1).
// Kept as unexported for potential future use.

func (m *overlayTransportManager) ensureRelay() (*p2p.RelayClient, error) {
	// Fast path: relay already exists
	m.mu.RLock()
	if m.relayClient != nil {
		relayClient := m.relayClient
		m.mu.RUnlock()
		return relayClient, nil
	}
	m.mu.RUnlock()

	// Slow path: need to create relay. Use write lock for the entire creation
	// to prevent concurrent Send() calls from creating duplicate clients (C1).
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if m.relayClient != nil {
		return m.relayClient, nil
	}

	// H2: Exponential backoff on relay creation failures
	if m.relayFailCount > 0 && !m.relayLastFail.IsZero() {
		backoff := time.Duration(1<<min(m.relayFailCount-1, 6)) * time.Second // 1s, 2s, 4s, ... 64s
		if time.Since(m.relayLastFail) < backoff {
			return nil, fmt.Errorf("relay creation in backoff (%v remaining)", backoff-time.Since(m.relayLastFail))
		}
	}

	relayEndpoint := m.relayEndpoint
	accessToken := m.accessToken
	localDeviceID := m.localDeviceID
	peerIDs := make([]string, 0, len(m.peers))
	for peerID := range m.peers {
		peerIDs = append(peerIDs, peerID)
	}

	if relayEndpoint == "" || accessToken == "" || localDeviceID == "" {
		return nil, fmt.Errorf("overlay relay is not configured")
	}

	relayClient, err := p2p.NewRelayClient(relayEndpoint, accessToken, localDeviceID)
	if err != nil {
		m.relayFailCount++
		m.relayLastFail = time.Now()
		return nil, err
	}

	relayClient.SetReceiveHandler(func(peerID string, data []byte) {
		m.dispatchPacket(peerID, data)
	})

	for _, peerID := range peerIDs {
		if err := relayClient.AddPeer(peerID); err != nil {
			_ = relayClient.Close()
			m.relayFailCount++
			m.relayLastFail = time.Now()
			return nil, err
		}
	}

	m.relayClient = relayClient
	m.relayFailCount = 0
	m.relayLastFail = time.Time{}
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

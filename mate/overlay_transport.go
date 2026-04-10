package mate

import (
	"fmt"
	"net"
	"sync"

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
}

func newOverlayTransportManager() *overlayTransportManager {
	return &overlayTransportManager{
		peers:       make(map[string]struct{}),
		packetConns: make(map[string]net.PacketConn),
	}
}

func (m *overlayTransportManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

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
	if existing := m.packetConns[peerID]; existing != nil && existing != conn {
		_ = existing.Close()
	}
	m.packetConns[peerID] = conn
	m.mu.Unlock()

	m.logf("[OverlayTransport] attached direct packet channel for peer %s", peerID)
	go m.readLoop(peerID, conn)
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

	if conn := m.packetConn(peerID); conn != nil {
		if _, err := conn.WriteTo(payload, &net.UDPAddr{}); err == nil {
			return nil
		}
		// direct send failed, fall through to relay
	}

	relayClient, err := m.ensureRelay()
	if err != nil {
		return err
	}

	return relayClient.SendToPeer(peerID, payload)
}

func (m *overlayTransportManager) packetConn(peerID string) net.PacketConn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.packetConns[peerID]
}

func (m *overlayTransportManager) ensureRelay() (*p2p.RelayClient, error) {
	m.mu.RLock()
	if m.relayClient != nil {
		relayClient := m.relayClient
		m.mu.RUnlock()
		return relayClient, nil
	}

	relayEndpoint := m.relayEndpoint
	accessToken := m.accessToken
	localDeviceID := m.localDeviceID
	peerIDs := make([]string, 0, len(m.peers))
	for peerID := range m.peers {
		peerIDs = append(peerIDs, peerID)
	}
	m.mu.RUnlock()

	if relayEndpoint == "" || accessToken == "" || localDeviceID == "" {
		return nil, fmt.Errorf("overlay relay is not configured")
	}

	relayClient, err := p2p.NewRelayClient(relayEndpoint, accessToken, localDeviceID)
	if err != nil {
		return nil, err
	}

	relayClient.SetReceiveHandler(func(peerID string, data []byte) {
		m.dispatchPacket(peerID, data)
	})

	for _, peerID := range peerIDs {
		if err := relayClient.AddPeer(peerID); err != nil {
			_ = relayClient.Close()
			return nil, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.relayClient != nil {
		_ = relayClient.Close()
		return m.relayClient, nil
	}
	m.relayClient = relayClient
	return relayClient, nil
}

func (m *overlayTransportManager) readLoop(peerID string, conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
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

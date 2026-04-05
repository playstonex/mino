package p2p

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

var (
	manager *Manager
	once    sync.Once
)

type Manager struct {
	Mu    sync.RWMutex
	Peers map[string]*webrtc.PeerConnection
	Conns map[string]net.Conn

	// PacketConns for UDP-style communication (net.PacketConn)
	PacketConns map[string]net.PacketConn

	// ICE servers for WebRTC connections
	iceServers []webrtc.ICEServer

	// Signaling callbacks (set by Swift/Mate)
	OnLocalDescription func(peerID string, sdp string, sdpType string)
	OnLocalCandidate   func(peerID string, candidate string)

	// Connection state callback
	OnConnectionStateChange func(peerID string, state string)
}

// packetMsg carries a datagram read from a DataChannel.
type packetMsg struct {
	data []byte
	addr net.Addr
}

func GetManager() *Manager {
	once.Do(func() {
		manager = &Manager{
			Peers:       make(map[string]*webrtc.PeerConnection),
			Conns:       make(map[string]net.Conn),
			PacketConns: make(map[string]net.PacketConn),
		}
	})
	return manager
}

func (m *Manager) SetICEServers(urls []string) {
	servers := make([]webrtc.ICEServer, len(urls))
	for i, u := range urls {
		servers[i] = webrtc.ICEServer{URLs: []string{u}}
	}
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.iceServers = servers
}

func (m *Manager) NewPeer(peerID string) (*webrtc.PeerConnection, error) {
	// Close any existing PeerConnection for this peer to prevent leaks
	// when retrying after a failure.
	m.RemovePeer(peerID)

	m.Mu.RLock()
	servers := make([]webrtc.ICEServer, len(m.iceServers))
	copy(servers, m.iceServers)
	m.Mu.RUnlock()

	if len(servers) == 0 {
		servers = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.miwifi.com:3478"}},
			{URLs: []string{"stun:stun.qq.com:3478"}},
			{URLs: []string{"stun:stun.aliyun.com:3478"}},
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		}
	}

	config := webrtc.Configuration{
		ICEServers: servers,
	}

	// Exclude TUN interfaces from ICE candidate gathering.
	// Both devices get the same TUN address (e.g. 198.18.0.1, fdfe:dcba:9876::1),
	// so TUN host candidates cause loopback connection attempts that mihomo rejects.
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetInterfaceFilter(func(iface string) bool {
		return !strings.HasPrefix(iface, "utun") &&
			!strings.HasPrefix(iface, "tun") &&
			iface != "Meta" &&
			iface != "Meta-tun"
	})
	settingEngine.DetachDataChannels()

	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		if m.OnLocalCandidate != nil {
			m.OnLocalCandidate(peerID, c.ToJSON().Candidate)
		}
	})

	pc.OnICEGatheringStateChange(func(s webrtc.ICEGatheringState) {
		// Log ICE gathering state changes for debugging
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		stateStr := s.String()
		if m.OnConnectionStateChange != nil {
			m.OnConnectionStateChange(peerID, stateStr)
		}
		if s == webrtc.PeerConnectionStateFailed {
			// Only remove this specific PeerConnection. If a new PC was already
			// created for the same peerID (retry), we must not close it.
			m.closePeerIfCurrent(peerID, pc)
		}
	})

	m.Mu.Lock()
	m.Peers[peerID] = pc
	m.Mu.Unlock()

	return pc, nil
}

func (m *Manager) AddPeer(peerID string, pc *webrtc.PeerConnection) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.Peers[peerID] = pc
}

func (m *Manager) GetPeer(peerID string) *webrtc.PeerConnection {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	return m.Peers[peerID]
}

func (m *Manager) RemovePeer(peerID string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if pc, ok := m.Peers[peerID]; ok {
		pc.Close()
		delete(m.Peers, peerID)
	}
	if conn, ok := m.Conns[peerID]; ok {
		conn.Close()
		delete(m.Conns, peerID)
	}
	if pc, ok := m.PacketConns[peerID]; ok {
		pc.Close()
		delete(m.PacketConns, peerID)
	}
}

// Reset closes all peer connections and clears all state.
// Called when the Mate core is restarted (Close+Start cycle) to ensure
// stale PeerConnections from a previous session don't leak.
func (m *Manager) Reset() {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	for id, pc := range m.Peers {
		pc.Close()
		delete(m.Peers, id)
	}
	for id, conn := range m.Conns {
		conn.Close()
		delete(m.Conns, id)
	}
	for id, pc := range m.PacketConns {
		pc.Close()
		delete(m.PacketConns, id)
	}
	m.OnLocalDescription = nil
	m.OnLocalCandidate = nil
	m.OnConnectionStateChange = nil
}

// closePeerIfCurrent removes the peer only if the PeerConnection in the map
// is still the same instance that triggered the callback. This prevents a
// late-arriving "failed" callback from an old PC from closing a newly
// created replacement PC.
func (m *Manager) closePeerIfCurrent(peerID string, pc *webrtc.PeerConnection) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if current, ok := m.Peers[peerID]; ok && current == pc {
		pc.Close()
		delete(m.Peers, peerID)
		if conn, ok := m.Conns[peerID]; ok {
			conn.Close()
			delete(m.Conns, peerID)
		}
		if pconn, ok := m.PacketConns[peerID]; ok {
			pconn.Close()
			delete(m.PacketConns, peerID)
		}
	}
}

func (m *Manager) RegisterConn(peerID string, conn net.Conn) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.Conns[peerID] = conn
}

func (m *Manager) RegisterPacketConn(peerID string, conn net.PacketConn) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.PacketConns[peerID] = conn
}

func (m *Manager) GetConn(ctx context.Context, peerID string) (net.Conn, error) {
	m.Mu.RLock()
	conn, ok := m.Conns[peerID]
	m.Mu.RUnlock()

	if ok && conn != nil {
		return conn, nil
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			m.Mu.RLock()
			conn, ok = m.Conns[peerID]
			m.Mu.RUnlock()
			if ok && conn != nil {
				return conn, nil
			}
		}
	}
}

// GetPacketConn returns the PacketConn for a peer, waiting up to 30s if not ready.
func (m *Manager) GetPacketConn(ctx context.Context, peerID string) (net.PacketConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	m.Mu.RLock()
	conn, ok := m.PacketConns[peerID]
	m.Mu.RUnlock()

	if ok && conn != nil {
		return conn, nil
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			m.Mu.RLock()
			conn, ok = m.PacketConns[peerID]
			m.Mu.RUnlock()
			if ok && conn != nil {
				return conn, nil
			}
		}
	}
}

// DataChannelConn implements net.Conn over a WebRTC DataChannel (stream mode).
type DataChannelConn struct {
	dc        *webrtc.DataChannel
	readCh    chan []byte
	buf       []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func NewDataChannelConn(dc *webrtc.DataChannel) *DataChannelConn {
	conn := &DataChannelConn{
		dc:     dc,
		readCh: make(chan []byte, 1024),
		closed: make(chan struct{}),
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := make([]byte, len(msg.Data))
		copy(data, msg.Data)
		select {
		case <-conn.closed:
			// Safely ignore incoming data if conn is already terminating
		default:
			select {
			case <-conn.closed:
			case conn.readCh <- data:
			default:
				// If buffer overflows, drop.
			}
		}
	})

	closeFunc := func() {
		conn.closeOnce.Do(func() {
			close(conn.closed)
			// DO NOT close(readCh) here to prevent "send on closed channel" PANICS from concurrent OnMessage!
			// readCh will be garbage collected safely.
		})
	}
	dc.OnClose(closeFunc)
	dc.OnError(func(err error) { closeFunc() })

	return conn
}

func (c *DataChannelConn) Read(b []byte) (n int, err error) {
	if len(c.buf) > 0 {
		n = copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	select {
	case <-c.closed:
		// Drain any remaining buffered data before returning EOF
		select {
		case msg := <-c.readCh:
			n = copy(b, msg)
			if n < len(msg) {
				c.buf = msg[n:]
			}
			return n, nil
		default:
			return 0, io.EOF
		}
	case msg := <-c.readCh:
		n = copy(b, msg)
		if n < len(msg) {
			c.buf = msg[n:]
		}
		return n, nil
	}
}

func (c *DataChannelConn) Write(b []byte) (n int, err error) {
	err = c.dc.Send(b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *DataChannelConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return c.dc.Close()
}

func (c *DataChannelConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0}
}
func (c *DataChannelConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0}
}
func (c *DataChannelConn) SetDeadline(t time.Time) error      { return nil }
func (c *DataChannelConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *DataChannelConn) SetWriteDeadline(t time.Time) error { return nil }

// PacketDataChannelConn implements net.PacketConn over a WebRTC DataChannel (datagram mode).
// Suitable for transporting IP packets where each DataChannel message is one packet.
type PacketDataChannelConn struct {
	dc        *webrtc.DataChannel
	msgCh     chan packetMsg
	lAddr     net.Addr
	rAddr     net.Addr
	closed    chan struct{}
	closeOnce sync.Once
}

func NewPacketDataChannelConn(dc *webrtc.DataChannel) *PacketDataChannelConn {
	conn := &PacketDataChannelConn{
		dc:     dc,
		msgCh:  make(chan packetMsg, 256),
		lAddr:  &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0},
		rAddr:  &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0},
		closed: make(chan struct{}),
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Copy data since msg.Data may be reused by pion
		data := make([]byte, len(msg.Data))
		copy(data, msg.Data)
		select {
		case <-conn.closed:
		default:
			select {
			case <-conn.closed:
			case conn.msgCh <- packetMsg{data: data, addr: conn.rAddr}:
			default:
				// Channel full — drop packet
			}
		}
	})

	closeFunc := func() {
		conn.closeOnce.Do(func() {
			close(conn.closed)
		})
	}

	dc.OnClose(closeFunc)
	dc.OnError(func(err error) { closeFunc() })

	return conn
}

func (c *PacketDataChannelConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	select {
	case <-c.closed:
		select {
		case msg := <-c.msgCh:
			n = copy(b, msg.data)
			return n, msg.addr, nil
		default:
			return 0, nil, io.EOF
		}
	case msg := <-c.msgCh:
		n = copy(b, msg.data)
		return n, msg.addr, nil
	}
}

func (c *PacketDataChannelConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	err = c.dc.Send(b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *PacketDataChannelConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return c.dc.Close()
}

func (c *PacketDataChannelConn) LocalAddr() net.Addr  { return c.lAddr }
func (c *PacketDataChannelConn) RemoteAddr() net.Addr { return c.rAddr }

func (c *PacketDataChannelConn) SetDeadline(t time.Time) error      { return nil }
func (c *PacketDataChannelConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *PacketDataChannelConn) SetWriteDeadline(t time.Time) error { return nil }

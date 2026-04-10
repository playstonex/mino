package outbound

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/transport/p2p"
	"github.com/pion/webrtc/v4"
)

type P2POption struct {
	BasicOption
	Name          string `proxy:"name"`
	PeerID        string `proxy:"peer-id"`
	LocalDeviceID string `proxy:"local-device-id,omitempty"`
	RelayEndpoint string `proxy:"relay-endpoint,omitempty"`
	AccessToken   string `proxy:"access-token,omitempty"`
}

type P2P struct {
	*Base
	peerID        string
	localDeviceID string
	relayEndpoint string
	accessToken   string
}

// Global shared relay client for this device.
var (
	relayMu     sync.Mutex
	relayClient *p2p.RelayClient
	relayOnce   sync.Once // ensures only one goroutine creates the relay

	// relayReceivers maps peerID -> list of active relayPacketConns waiting
	// for inbound data. When the relay's receive handler fires, it fans out
	// to all registered receivers for that peer.
	relayReceivers   = make(map[string][]*relayPacketConn)
	relayReceiversMu sync.Mutex
)

// StreamConn is used when the proxy wraps another connection.
func (p *P2P) StreamConn(c net.Conn, metadata *C.Metadata) (net.Conn, error) {
	return nil, C.ErrNotSupport
}

func (p *P2P) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	pc := p2p.GetManager().GetPeer(p.peerID)
	if pc == nil {
		// Fallback to relay if WebRTC is not connected
		rp, err := p.getRelayPacketConn(ctx)
		if err != nil {
			return nil, fmt.Errorf("P2P dial: %s: %w", p.peerID, err)
		}
		rAddr := &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0}
		conn := N.NewBindPacketConn(rp, rAddr)
		return NewConn(conn, p), nil
	}

	ordered := true
	label := fmt.Sprintf("tcp|%s:%d", metadata.DstIP.String(), metadata.DstPort)
	dc, err := pc.CreateDataChannel(label, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, fmt.Errorf("P2P create channel: %s: %w", p.peerID, err)
	}

	conn := p2p.NewDataChannelConn(dc)

	openCh := make(chan struct{})
	dc.OnOpen(func() { close(openCh) })

	select {
	case <-ctx.Done():
		dc.Close()
		return nil, ctx.Err()
	case <-openCh:
	}

	return NewConn(conn, p), nil
}

func (p *P2P) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	pc := p2p.GetManager().GetPeer(p.peerID)
	if pc == nil {
		rp, err := p.getRelayPacketConn(ctx)
		if err != nil {
			return nil, fmt.Errorf("P2P packet: %s: %w", p.peerID, err)
		}
		return newPacketConn(rp, p), nil
	}

	ordered := false
	maxRetransmits := uint16(0)
	label := fmt.Sprintf("udp|%s:%d", metadata.DstIP.String(), metadata.DstPort)
	dc, err := pc.CreateDataChannel(label, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		return nil, fmt.Errorf("P2P create udp channel: %s: %w", p.peerID, err)
	}

	conn := p2p.NewPacketDataChannelConn(dc)

	openCh := make(chan struct{})
	dc.OnOpen(func() { close(openCh) })

	select {
	case <-ctx.Done():
		dc.Close()
		return nil, ctx.Err()
	case <-openCh:
	}

	// We need to store the target destination so WriteTo knows where to send if we don't encode it per-packet
	return newPacketConn(conn, p), nil
}

func (p *P2P) SupportUOT() bool {
	return true
}

// getPacketConn tries WebRTC first, falls back to relay.
func (p *P2P) getPacketConn(ctx context.Context) (net.PacketConn, error) {
	// Try WebRTC first (with a shorter timeout to fail fast).
	p2pCtx, p2pCancel := context.WithTimeout(ctx, 5*time.Second)
	defer p2pCancel()

	manager := p2p.GetManager()
	pc, err := manager.GetPacketConn(p2pCtx, p.peerID)
	if err == nil {
		return pc, nil
	}

	// WebRTC failed — use relay fallback.
	return p.getRelayPacketConn(ctx)
}

func (p *P2P) getRelayPacketConn(ctx context.Context) (net.PacketConn, error) {
	relayMu.Lock()

	// If relay already connected, return a new receiver conn.
	if relayClient != nil {
		relayMu.Unlock()
		relayClient.AddPeer(p.peerID)
		return newRelayPacketConn(p.peerID), nil
	}

	// If relay not yet initialized, create it.
	if p.relayEndpoint == "" || p.accessToken == "" || p.localDeviceID == "" {
		relayMu.Unlock()
		return nil, fmt.Errorf("P2P relay fallback: not configured (endpoint=%q, token=%v, deviceID=%v)",
			p.relayEndpoint, p.accessToken != "", p.localDeviceID != "")
	}

	// Release lock during potentially slow network operations.
	relayMu.Unlock()

	rc, err := p2p.NewRelayClient(p.relayEndpoint, p.accessToken, p.localDeviceID)
	if err != nil {
		return nil, fmt.Errorf("P2P relay connect: %w", err)
	}

	// Re-acquire lock to set global relayClient.
	relayMu.Lock()
	if relayClient == nil {
		// Wire up the receive handler to fan out to registered receivers.
		rc.SetReceiveHandler(func(peerID string, data []byte) {
			relayReceiversMu.Lock()
			receivers := relayReceivers[peerID]
			relayReceiversMu.Unlock()
			for _, r := range receivers {
				// Non-blocking send — drop if receiver is full.
				dataCopy := make([]byte, len(data))
				copy(dataCopy, data)
				select {
				case r.recvCh <- dataCopy:
				default:
				}
			}
		})
		relayClient = rc
	} else {
		rc.Close()
	}
	relayClient.AddPeer(p.peerID)
	relayMu.Unlock()

	return newRelayPacketConn(p.peerID), nil
}

func (p *P2P) Close() error {
	relayMu.Lock()
	defer relayMu.Unlock()
	if relayClient != nil {
		relayClient.Close()
		relayClient = nil
		relayOnce = sync.Once{} // allow re-creation on next Start
	}
	return nil
}

func NewP2P(option P2POption) (*P2P, error) {
	// Do NOT eagerly create the relay client here. The network may not be ready yet.
	// Relay connection will be established on-demand when getRelayPacketConn is called.
	relayMu.Lock()
	if relayClient != nil && option.PeerID != "" {
		relayClient.AddPeer(option.PeerID)
	}
	relayMu.Unlock()

	p := &P2P{
		Base: &Base{
			name:   option.Name,
			tp:     C.P2P,
			pdName: option.ProviderName,
			udp:    true,
			tfo:    option.TFO,
			mpTcp:  option.MPTCP,
			iface:  option.Interface,
			rmark:  option.RoutingMark,
			prefer: option.IPVersion,
			dialer: option.BasicOption.NewDialer(nil),
		},
		peerID:        option.PeerID,
		localDeviceID: option.LocalDeviceID,
		relayEndpoint: option.RelayEndpoint,
		accessToken:   option.AccessToken,
	}

	// Proactively connect relay once (not per-peer) to eliminate wait times
	// if WebRTC (ICE) hole punching takes too long or fails.
	if p.relayEndpoint != "" && p.accessToken != "" && p.localDeviceID != "" {
		go func() {
			relayOnce.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = p.getRelayPacketConn(ctx)
			})
			// Register this peer even if relay was already created by another P2P instance.
			relayMu.Lock()
			if relayClient != nil {
				relayClient.AddPeer(p.peerID)
			}
			relayMu.Unlock()
		}()
	}

	return p, nil
}

// relayPacketConn wraps the shared RelayClient as net.PacketConn for a specific peer.
// Each instance has its own receive channel so ReadFrom actually returns data.
type relayPacketConn struct {
	peerID    string
	recvCh    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newRelayPacketConn(peerID string) *relayPacketConn {
	r := &relayPacketConn{
		peerID: peerID,
		recvCh: make(chan []byte, 256),
		closed: make(chan struct{}),
	}
	// Register this conn to receive relay data for this peer.
	relayReceiversMu.Lock()
	relayReceivers[peerID] = append(relayReceivers[peerID], r)
	relayReceiversMu.Unlock()
	return r
}

func (r *relayPacketConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	select {
	case <-r.closed:
		return 0, nil, io.EOF
	case data := <-r.recvCh:
		n = copy(b, data)
		return n, &net.UDPAddr{}, nil
	}
}

func (r *relayPacketConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	relayMu.Lock()
	rc := relayClient
	relayMu.Unlock()
	if rc == nil {
		return 0, fmt.Errorf("relay not connected")
	}
	if err := rc.SendToPeer(r.peerID, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (r *relayPacketConn) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		// Unregister from receivers.
		relayReceiversMu.Lock()
		receivers := relayReceivers[r.peerID]
		for i, recv := range receivers {
			if recv == r {
				relayReceivers[r.peerID] = append(receivers[:i], receivers[i+1:]...)
				break
			}
		}
		if len(relayReceivers[r.peerID]) == 0 {
			delete(relayReceivers, r.peerID)
		}
		relayReceiversMu.Unlock()
	})
	return nil
}

func (r *relayPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0}
}

func (r *relayPacketConn) SetDeadline(t time.Time) error      { return nil }
func (r *relayPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (r *relayPacketConn) SetWriteDeadline(t time.Time) error { return nil }

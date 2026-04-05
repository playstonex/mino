package outbound

import (
	"context"
	"fmt"
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

	// If relay already connected, return the wrapped conn.
	if relayClient != nil {
		relayMu.Unlock()
		// Ensure this peer is registered.
		relayClient.AddPeer(p.peerID)
		return &relayPacketConn{peerID: p.peerID}, nil
	}

	// If relay not yet initialized, create it.
	if p.relayEndpoint == "" || p.accessToken == "" || p.localDeviceID == "" {
		relayMu.Unlock()
		return nil, fmt.Errorf("P2P relay fallback: not configured (endpoint=%q, token=%v, deviceID=%v)",
			p.relayEndpoint, p.accessToken != "", p.localDeviceID != "")
	}

	// Release lock during potentially slow network operations.
	relayMu.Unlock()

	fmt.Printf("[P2P Relay] Attempting relay connection for device %s to %s\n", p.localDeviceID, p.relayEndpoint)
	rc, err := p2p.NewRelayClient(p.relayEndpoint, p.accessToken, p.localDeviceID)
	if err != nil {
		fmt.Printf("[P2P Relay] Relay connection failed for device %s: %v\n", p.localDeviceID, err)
		return nil, fmt.Errorf("P2P relay connect: %w", err)
	}

	fmt.Printf("[P2P Relay] Successfully connected relay for device %s\n", p.localDeviceID)

	// Re-acquire lock to set global relayClient.
	relayMu.Lock()
	// Double-check: another goroutine might have initialized it while we were connecting.
	if relayClient == nil {
		relayClient = rc
	} else {
		// Another goroutine already set it, close our duplicate connection.
		rc.Close()
	}
	relayClient.AddPeer(p.peerID)
	relayMu.Unlock()

	return &relayPacketConn{peerID: p.peerID}, nil
}

func (p *P2P) Close() error {
	relayMu.Lock()
	defer relayMu.Unlock()
	if relayClient != nil {
		relayClient.Close()
		relayClient = nil
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

	// Background goroutine: Proactively connect relay to eliminate wait times
	// if WebRTC (ICE) hole punching takes too long or fails.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = p.getRelayPacketConn(ctx)
	}()

	return p, nil
}

// relayPacketConn wraps the shared RelayClient as net.PacketConn for a specific peer.
type relayPacketConn struct {
	peerID string
	lAddr  net.Addr
}

func (r *relayPacketConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	// Inbound packets are delivered via the relay's receive handler directly
	// to the tunnel. This ReadFrom blocks since the relay handles delivery.
	select {}
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
	return nil // Shared relay is closed by P2P.Close()
}

func (r *relayPacketConn) LocalAddr() net.Addr {
	if r.lAddr != nil {
		return r.lAddr
	}
	return &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0}
}

func (r *relayPacketConn) SetDeadline(t time.Time) error      { return nil }
func (r *relayPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (r *relayPacketConn) SetWriteDeadline(t time.Time) error { return nil }

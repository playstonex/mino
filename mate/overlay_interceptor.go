package mate

import (
	"net/netip"

	tun "github.com/playstonex/sing-tun"
)

var _ tun.PacketInterceptor = (*OverlayManager)(nil)
var _ tun.PacketInterceptMatcher = (*OverlayManager)(nil)

var overlayIPv4Prefix = netip.MustParsePrefix("100.96.0.0/12")

func (m *OverlayManager) ShouldInterceptPacket(destination netip.Addr) bool {
	if !destination.Is4() || !overlayIPv4Prefix.Contains(destination) {
		return false
	}

	m.mu.RLock()
	cfg := m.config
	localIP := m.overlayIP
	m.mu.RUnlock()

	return cfg != nil && cfg.Mode == "hybrid" && destination.String() != localIP
}

// InterceptPacket routes hybrid-mode overlay packets before they enter gVisor's
// TCP/UDP stack. It consumes every non-local packet in the overlay CIDR so
// unknown peers cannot leak into the normal proxy path.
func (m *OverlayManager) InterceptPacket(destination netip.Addr, packet []byte) bool {
	if !m.ShouldInterceptPacket(destination) {
		return false
	}

	m.mu.RLock()
	peerID, hasRoute := m.routes[destination.String()]
	peerCipher, hasCipher := m.peerCiphers[peerID]
	m.mu.RUnlock()

	if !m.running.Load() {
		return true
	}
	if !hasRoute || !hasCipher {
		if m.debugPacketLog.Load() {
			m.logf("[Overlay-Go] Dropping hybrid packet for unknown overlay destination %s", destination)
		}
		return true
	}

	encrypted, err := m.encryptPacket(packet, peerCipher)
	if err != nil {
		m.logf("[Overlay-Go] Failed to encrypt hybrid packet for %s: %v", destination, err)
		return true
	}
	if err := globalOverlayTransport.Send(peerID, encrypted); err != nil && m.debugPacketLog.Load() {
		m.logf("[Overlay-Go] Failed to send hybrid packet to %s: %v", destination, err)
	}
	return true
}

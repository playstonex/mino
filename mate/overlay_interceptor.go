package mate

import (
	"encoding/binary"
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
	localIP := m.overlayIP
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

	packet = rewriteHybridOverlaySource(packet, localIP)

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

// rewriteHybridOverlaySource makes hybrid-mode overlay traffic look like it
// originated from the device's overlay IP, not from mihomo's 198.18.0.1 TUN
// address. iOS/macOS may choose the first TUN address as the IPv4 source when
// multiple addresses are configured; remote peers then reply to 198.18.0.1 and
// TCP sessions such as SSH never complete. Rewriting at the IP boundary keeps
// the overlay path symmetric without changing normal proxy traffic.
func rewriteHybridOverlaySource(packet []byte, localIP string) []byte {
	localAddr, err := netip.ParseAddr(localIP)
	if err != nil || !localAddr.Is4() || len(packet) < 20 {
		return packet
	}
	if version := packet[0] >> 4; version != 4 {
		return packet
	}

	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || len(packet) < headerLen {
		return packet
	}

	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen == 0 || totalLen > len(packet) {
		totalLen = len(packet)
	}
	if totalLen < headerLen {
		return packet
	}

	local4 := localAddr.As4()
	if packet[12] == local4[0] &&
		packet[13] == local4[1] &&
		packet[14] == local4[2] &&
		packet[15] == local4[3] {
		return packet
	}

	oldSource := [4]byte{packet[12], packet[13], packet[14], packet[15]}
	copy(packet[12:16], local4[:])

	packet[10], packet[11] = 0, 0
	binary.BigEndian.PutUint16(packet[10:12], internetChecksum(packet[:headerLen]))

	fragmentField := binary.BigEndian.Uint16(packet[6:8])
	fragmentOffset := fragmentField & 0x1fff
	moreFragments := fragmentField&0x2000 != 0
	if fragmentOffset != 0 {
		return packet
	}

	switch packet[9] {
	case 6: // TCP
		if totalLen >= headerLen+20 {
			checksumOffset := headerLen + 16
			if moreFragments {
				oldChecksum := binary.BigEndian.Uint16(packet[checksumOffset : checksumOffset+2])
				binary.BigEndian.PutUint16(
					packet[checksumOffset:checksumOffset+2],
					replaceChecksumIPv4Source(oldChecksum, oldSource, local4),
				)
			} else {
				packet[checksumOffset], packet[checksumOffset+1] = 0, 0
				binary.BigEndian.PutUint16(
					packet[checksumOffset:checksumOffset+2],
					ipv4TransportChecksum(6, packet[12:16], packet[16:20], packet[headerLen:totalLen]),
				)
			}
		}
	case 17: // UDP
		if totalLen >= headerLen+8 {
			checksumOffset := headerLen + 6
			oldChecksum := binary.BigEndian.Uint16(packet[checksumOffset : checksumOffset+2])
			if oldChecksum != 0 {
				var newChecksum uint16
				if moreFragments {
					newChecksum = replaceChecksumIPv4Source(oldChecksum, oldSource, local4)
				} else {
					packet[checksumOffset], packet[checksumOffset+1] = 0, 0
					newChecksum = ipv4TransportChecksum(17, packet[12:16], packet[16:20], packet[headerLen:totalLen])
				}
				if newChecksum == 0 {
					newChecksum = 0xffff
				}
				binary.BigEndian.PutUint16(packet[checksumOffset:checksumOffset+2], newChecksum)
			}
		}
	}

	return packet
}

func replaceChecksumIPv4Source(checksum uint16, oldSource [4]byte, newSource [4]byte) uint16 {
	sum := uint32(^checksum) & 0xffff
	for i := 0; i < 4; i += 2 {
		oldWord := binary.BigEndian.Uint16(oldSource[i : i+2])
		newWord := binary.BigEndian.Uint16(newSource[i : i+2])
		sum += uint32(^oldWord)&0xffff + uint32(newWord)
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func internetChecksum(data []byte) uint16 {
	return finishChecksum(checksumAddBytes(0, data))
}

func ipv4TransportChecksum(protocol byte, source []byte, destination []byte, segment []byte) uint16 {
	sum := checksumAddBytes(0, source)
	sum = checksumAddBytes(sum, destination)
	sum += uint32(protocol)
	sum += uint32(len(segment))
	sum = checksumAddBytes(sum, segment)
	return finishChecksum(sum)
}

func checksumAddBytes(sum uint32, data []byte) uint32 {
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data[:2]))
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	return sum
}

func finishChecksum(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

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
	packet = clampTCPMSS(packet, overlayMaxTCPMSS)

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

// rewriteHybridOverlayDestination rewrites the destination IP of an inbound
// overlay packet from the local overlay IP (100.96.x.y) to the mihomo TUN
// address (198.18.0.1). In hybrid mode the TUN has two addresses:
//   - 198.18.0.1 (primary — apps bind to this via gVisor)
//   - 100.96.x.y (overlay IP)
//
// When an app (e.g. SSH) connects to a remote overlay peer, the OS picks
// 198.18.0.1 as the source (first TUN address). rewriteHybridOverlaySource
// rewrites outbound source to 100.96.x.y so the remote peer replies to the
// correct overlay IP. But when the reply arrives, its destination is
// 100.96.x.y — the TCP stack won't match it to the socket bound on
// 198.18.0.1. This function completes the NAT by rewriting the inbound
// destination back to 198.18.0.1, making the connection symmetric.
func rewriteHybridOverlayDestination(packet []byte, localOverlayIP string) []byte {
	localAddr, err := netip.ParseAddr(localOverlayIP)
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

	// Only rewrite if destination matches our overlay IP
	local4 := localAddr.As4()
	if packet[16] != local4[0] ||
		packet[17] != local4[1] ||
		packet[18] != local4[2] ||
		packet[19] != local4[3] {
		return packet
	}

	// Rewrite destination to 198.18.0.1 (mihomo TUN primary address)
	mihomoTUN := [4]byte{198, 18, 0, 1}
	oldDest := [4]byte{packet[16], packet[17], packet[18], packet[19]}
	copy(packet[16:20], mihomoTUN[:])

	// Recompute IP header checksum
	packet[10], packet[11] = 0, 0
	binary.BigEndian.PutUint16(packet[10:12], internetChecksum(packet[:headerLen]))

	// For non-first fragments, we can only fix the IP header (no transport header accessible)
	fragmentField := binary.BigEndian.Uint16(packet[6:8])
	fragmentOffset := fragmentField & 0x1fff
	moreFragments := fragmentField&0x2000 != 0
	if fragmentOffset != 0 {
		return packet
	}

	// Fix transport layer checksum (pseudo-header includes destination IP)
	switch packet[9] {
	case 6: // TCP
		if totalLen >= headerLen+20 {
			checksumOffset := headerLen + 16
			if moreFragments {
				oldChecksum := binary.BigEndian.Uint16(packet[checksumOffset : checksumOffset+2])
				binary.BigEndian.PutUint16(
					packet[checksumOffset:checksumOffset+2],
					replaceChecksumIPv4Dest(oldChecksum, oldDest, mihomoTUN),
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
					newChecksum = replaceChecksumIPv4Dest(oldChecksum, oldDest, mihomoTUN)
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

// replaceChecksumIPv4Dest performs incremental checksum update when only the
// destination IP changes (RFC 1624).
func replaceChecksumIPv4Dest(checksum uint16, oldDest [4]byte, newDest [4]byte) uint16 {
	sum := uint32(^checksum) & 0xffff
	for i := 0; i < 4; i += 2 {
		oldWord := binary.BigEndian.Uint16(oldDest[i : i+2])
		newWord := binary.BigEndian.Uint16(newDest[i : i+2])
		sum += uint32(^oldWord)&0xffff + uint32(newWord)
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
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

// overlayMaxTCPMSS caps the TCP MSS negotiated across the overlay so that a
// full-size segment still fits the relay path after encryption and framing:
// 1160 MSS + 40 IP/TCP + 28 AES-GCM + 34 relay header = 1262 bytes of UDP
// payload, well under both the relay client read buffer and a 1500-byte
// public-internet path MTU (no IP fragmentation, which Chinese carriers/NATs
// often drop). Direct WebRTC paths fragment on their own via SCTP, but relay
// does not, so hybrid mode's 1500 TUN MTU otherwise breaks SSH KEX over relay.
const overlayMaxTCPMSS = 1160

// clampTCPMSS lowers the MSS option of an IPv4 TCP SYN / SYN-ACK to maxMSS and
// recomputes the TCP checksum. Any other packet is returned untouched. It is
// applied to both outbound and inbound overlay packets, so a single upgraded
// endpoint is enough to keep both directions under the cap.
func clampTCPMSS(packet []byte, maxMSS uint16) []byte {
	if len(packet) < 20 || packet[0]>>4 != 4 || packet[9] != 6 {
		return packet
	}
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || len(packet) < headerLen+20 {
		return packet
	}
	// SYNs are never fragmented in practice; skip any fragment rather than
	// computing a checksum over a partial segment.
	if binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return packet
	}
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen == 0 || totalLen > len(packet) {
		totalLen = len(packet)
	}
	tcp := packet[headerLen:totalLen]
	if len(tcp) < 20 || tcp[13]&0x02 == 0 { // SYN flag
		return packet
	}
	dataOffset := int(tcp[12]>>4) * 4
	if dataOffset <= 20 || dataOffset > len(tcp) {
		return packet
	}

	opts := tcp[20:dataOffset]
	changed := false
	for i := 0; i < len(opts); {
		kind := opts[i]
		if kind == 0 { // end of option list
			break
		}
		if kind == 1 { // NOP
			i++
			continue
		}
		if i+1 >= len(opts) {
			break
		}
		optLen := int(opts[i+1])
		if optLen < 2 || i+optLen > len(opts) {
			break
		}
		if kind == 2 && optLen == 4 { // MSS
			if binary.BigEndian.Uint16(opts[i+2:i+4]) > maxMSS {
				binary.BigEndian.PutUint16(opts[i+2:i+4], maxMSS)
				changed = true
			}
		}
		i += optLen
	}
	if !changed {
		return packet
	}

	tcp[16], tcp[17] = 0, 0
	binary.BigEndian.PutUint16(tcp[16:18], ipv4TransportChecksum(6, packet[12:16], packet[16:20], tcp))
	return packet
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

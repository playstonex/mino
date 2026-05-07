package mate

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/metacubex/gopacket"
	"github.com/metacubex/gopacket/layers"
)

const (
	ipProtoICMP = 1
	ipProtoTCP  = 6
	ipProtoUDP  = 17

	tcpFlagFIN = 0x01
	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04
	tcpFlagPSH = 0x08
	tcpFlagACK = 0x10

	hybridRelayTCPIdleTimeout = 2 * time.Minute
	hybridRelayUDPIdleTimeout = 30 * time.Second
)

type hybridRelay struct {
	mu       sync.Mutex
	tcpConns map[hybridConnKey]*hybridTCPConn
	udpConns map[hybridConnKey]*hybridUDPConn
}

type hybridConnKey struct {
	srcIP   [4]byte
	srcPort uint16
	dstPort uint16
	proto   uint8
}

type ipv4PacketInfo struct {
	srcIP    [4]byte
	dstIP    [4]byte
	protocol uint8
	payload  []byte
}

type tcpPacketInfo struct {
	srcPort uint16
	dstPort uint16
	seq     uint32
	ack     uint32
	flags   uint8
	payload []byte
}

type udpPacketInfo struct {
	srcPort uint16
	dstPort uint16
	payload []byte
}

type hybridTCPConn struct {
	key       hybridConnKey
	peerID    string
	cipher    cipher.AEAD
	manager   *OverlayManager
	localConn net.Conn
	closeOnce sync.Once
	closeCh   chan struct{}
	mu        sync.Mutex

	srcIP    [4]byte
	dstIP    [4]byte
	ourSeq   uint32
	theirSeq uint32
	lastSeen time.Time
}

type hybridUDPConn struct {
	key       hybridConnKey
	peerID    string
	cipher    cipher.AEAD
	manager   *OverlayManager
	localConn *net.UDPConn
	closeOnce sync.Once
	closeCh   chan struct{}
	mu        sync.Mutex

	srcIP    [4]byte
	dstIP    [4]byte
	lastSeen time.Time
}

func newHybridRelay() *hybridRelay {
	return &hybridRelay{
		tcpConns: make(map[hybridConnKey]*hybridTCPConn),
		udpConns: make(map[hybridConnKey]*hybridUDPConn),
	}
}

func (r *hybridRelay) close() {
	r.mu.Lock()
	tcpConns := make([]*hybridTCPConn, 0, len(r.tcpConns))
	for _, conn := range r.tcpConns {
		tcpConns = append(tcpConns, conn)
	}
	udpConns := make([]*hybridUDPConn, 0, len(r.udpConns))
	for _, conn := range r.udpConns {
		udpConns = append(udpConns, conn)
	}
	r.tcpConns = make(map[hybridConnKey]*hybridTCPConn)
	r.udpConns = make(map[hybridConnKey]*hybridUDPConn)
	r.mu.Unlock()

	for _, conn := range tcpConns {
		conn.close()
	}
	for _, conn := range udpConns {
		conn.close()
	}
}

func (r *hybridRelay) relayPacket(peerID string, packet []byte, manager *OverlayManager, peerCipher cipher.AEAD) {
	ip, ok := parseIPv4Packet(packet)
	if !ok {
		return
	}
	if !manager.isLocalOverlayIPv4(ip.dstIP) {
		if manager.debugPacketLog.Load() {
			manager.logf("[HybridRelay] Dropping packet for non-local overlay IP %s", ipv4String(ip.dstIP))
		}
		return
	}

	switch ip.protocol {
	case ipProtoICMP:
		r.relayICMP(peerID, ip, manager, peerCipher)
	case ipProtoTCP:
		tcp, ok := parseTCPPacket(ip.payload)
		if !ok {
			return
		}
		r.relayTCP(peerID, ip, tcp, manager, peerCipher)
	case ipProtoUDP:
		udp, ok := parseUDPPacket(ip.payload)
		if !ok {
			return
		}
		r.relayUDP(peerID, ip, udp, manager, peerCipher)
	default:
		manager.logf("[HybridRelay] Unsupported inbound IP protocol %d from %s", ip.protocol, peerID)
	}
}

func (r *hybridRelay) relayICMP(peerID string, ip ipv4PacketInfo, manager *OverlayManager, peerCipher cipher.AEAD) {
	if len(ip.payload) < 8 {
		return
	}
	icmpType := ip.payload[0]
	icmpCode := ip.payload[1]
	if icmpType != layers.ICMPv4TypeEchoRequest || icmpCode != 0 {
		return
	}

	packet, err := buildICMPEchoReplyPacket(ip.dstIP, ip.srcIP, ip.payload)
	if err != nil {
		manager.logf("[HybridRelay] build ICMP echo reply failed: %v", err)
		return
	}
	encrypted, err := manager.encryptPacket(packet, peerCipher)
	if err != nil {
		manager.logf("[HybridRelay] encrypt ICMP echo reply failed: %v", err)
		return
	}
	if err := globalOverlayTransport.Send(peerID, encrypted); err != nil {
		manager.logf("[HybridRelay] send ICMP echo reply failed: %v", err)
	}
}

func (r *hybridRelay) relayTCP(
	peerID string,
	ip ipv4PacketInfo,
	tcp tcpPacketInfo,
	manager *OverlayManager,
	peerCipher cipher.AEAD,
) {
	key := hybridConnKey{
		srcIP:   ip.srcIP,
		srcPort: tcp.srcPort,
		dstPort: tcp.dstPort,
		proto:   ipProtoTCP,
	}

	r.mu.Lock()
	conn := r.tcpConns[key]
	r.mu.Unlock()

	if tcp.flags&tcpFlagRST != 0 {
		if conn != nil {
			r.removeTCP(key)
			conn.close()
		}
		return
	}

	if tcp.flags&tcpFlagSYN != 0 {
		if conn != nil {
			r.removeTCP(key)
			conn.close()
		}
		conn = r.createTCPConn(key, peerID, ip, tcp, manager, peerCipher)
		return
	}

	if conn == nil {
		return
	}

	conn.touch()

	if len(tcp.payload) > 0 {
		conn.mu.Lock()
		conn.theirSeq = tcp.seq + uint32(len(tcp.payload))
		conn.mu.Unlock()
		if _, err := conn.localConn.Write(tcp.payload); err != nil {
			r.removeTCP(key)
			conn.close()
			return
		}
		conn.sendTCP(tcpFlagACK, nil)
	}

	if tcp.flags&tcpFlagFIN != 0 {
		conn.mu.Lock()
		conn.theirSeq = tcp.seq + 1
		conn.mu.Unlock()
		conn.sendTCP(tcpFlagFIN|tcpFlagACK, nil)
		r.removeTCP(key)
		conn.close()
		return
	}
}

func (r *hybridRelay) createTCPConn(
	key hybridConnKey,
	peerID string,
	ip ipv4PacketInfo,
	tcp tcpPacketInfo,
	manager *OverlayManager,
	peerCipher cipher.AEAD,
) *hybridTCPConn {
	localConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", tcp.dstPort), 5*time.Second)
	if err != nil {
		manager.logf("[HybridRelay] TCP dial 127.0.0.1:%d failed: %v", tcp.dstPort, err)
		sendHybridTCPReset(peerID, ip, tcp, manager, peerCipher)
		return nil
	}

	conn := &hybridTCPConn{
		key:       key,
		peerID:    peerID,
		cipher:    peerCipher,
		manager:   manager,
		localConn: localConn,
		closeCh:   make(chan struct{}),
		srcIP:     ip.srcIP,
		dstIP:     ip.dstIP,
		ourSeq:    randomUint32(),
		theirSeq:  tcp.seq + 1,
		lastSeen:  time.Now(),
	}

	r.mu.Lock()
	r.tcpConns[key] = conn
	r.mu.Unlock()

	conn.sendTCP(tcpFlagSYN|tcpFlagACK, nil)
	go r.readTCPResponses(conn)
	go r.expireTCPConn(conn)
	return conn
}

func (r *hybridRelay) readTCPResponses(conn *hybridTCPConn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.localConn.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			conn.sendTCP(tcpFlagPSH|tcpFlagACK, payload)
		}
		if err != nil {
			if err != io.EOF {
				conn.manager.logf("[HybridRelay] TCP local read failed: %v", err)
			}
			conn.sendTCP(tcpFlagFIN|tcpFlagACK, nil)
			r.removeTCP(conn.key)
			conn.close()
			return
		}
	}
}

func (r *hybridRelay) expireTCPConn(conn *hybridTCPConn) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-conn.closeCh:
			return
		case <-ticker.C:
			if conn.idleFor() < hybridRelayTCPIdleTimeout {
				continue
			}
			r.removeTCP(conn.key)
			conn.close()
			return
		}
	}
}

func (r *hybridRelay) removeTCP(key hybridConnKey) {
	r.mu.Lock()
	delete(r.tcpConns, key)
	r.mu.Unlock()
}

func (c *hybridTCPConn) sendTCP(flags uint8, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	packet, err := buildTCPPacket(c.dstIP, c.srcIP, c.key.dstPort, c.key.srcPort, c.ourSeq, c.theirSeq, flags, payload)
	if err != nil {
		c.manager.logf("[HybridRelay] build TCP packet failed: %v", err)
		return
	}
	if err := c.send(packet); err != nil {
		c.manager.logf("[HybridRelay] send TCP packet failed: %v", err)
		return
	}

	if flags&tcpFlagSYN != 0 || flags&tcpFlagFIN != 0 {
		c.ourSeq++
	}
	c.ourSeq += uint32(len(payload))
}

func (c *hybridTCPConn) send(packet []byte) error {
	encrypted, err := c.manager.encryptPacket(packet, c.cipher)
	if err != nil {
		return err
	}
	return globalOverlayTransport.Send(c.peerID, encrypted)
}

func (c *hybridTCPConn) close() {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		_ = c.localConn.Close()
	})
}

func (c *hybridTCPConn) touch() {
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *hybridTCPConn) idleFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.lastSeen)
}

func (r *hybridRelay) relayUDP(
	peerID string,
	ip ipv4PacketInfo,
	udp udpPacketInfo,
	manager *OverlayManager,
	peerCipher cipher.AEAD,
) {
	key := hybridConnKey{
		srcIP:   ip.srcIP,
		srcPort: udp.srcPort,
		dstPort: udp.dstPort,
		proto:   ipProtoUDP,
	}

	r.mu.Lock()
	conn := r.udpConns[key]
	r.mu.Unlock()
	if conn == nil {
		conn = r.createUDPConn(key, peerID, ip, manager, peerCipher)
		if conn == nil {
			return
		}
	}

	conn.touch()
	if _, err := conn.localConn.Write(udp.payload); err != nil {
		r.removeUDP(key)
		conn.close()
	}
}

func (r *hybridRelay) createUDPConn(
	key hybridConnKey,
	peerID string,
	ip ipv4PacketInfo,
	manager *OverlayManager,
	peerCipher cipher.AEAD,
) *hybridUDPConn {
	remoteAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(key.dstPort)}
	localConn, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		manager.logf("[HybridRelay] UDP dial 127.0.0.1:%d failed: %v", key.dstPort, err)
		return nil
	}

	conn := &hybridUDPConn{
		key:       key,
		peerID:    peerID,
		cipher:    peerCipher,
		manager:   manager,
		localConn: localConn,
		closeCh:   make(chan struct{}),
		srcIP:     ip.srcIP,
		dstIP:     ip.dstIP,
		lastSeen:  time.Now(),
	}

	r.mu.Lock()
	r.udpConns[key] = conn
	r.mu.Unlock()

	go r.readUDPResponses(conn)
	go r.expireUDPConn(conn)
	return conn
}

func (r *hybridRelay) readUDPResponses(conn *hybridUDPConn) {
	buf := make([]byte, 65535)
	for {
		n, err := conn.localConn.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			packet, buildErr := buildUDPPacket(conn.dstIP, conn.srcIP, conn.key.dstPort, conn.key.srcPort, payload)
			if buildErr != nil {
				conn.manager.logf("[HybridRelay] build UDP packet failed: %v", buildErr)
				continue
			}
			if err := conn.send(packet); err != nil {
				conn.manager.logf("[HybridRelay] send UDP packet failed: %v", err)
			}
		}
		if err != nil {
			r.removeUDP(conn.key)
			conn.close()
			return
		}
	}
}

func (r *hybridRelay) expireUDPConn(conn *hybridUDPConn) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-conn.closeCh:
			return
		case <-ticker.C:
			if conn.idleFor() < hybridRelayUDPIdleTimeout {
				continue
			}
			r.removeUDP(conn.key)
			conn.close()
			return
		}
	}
}

func (r *hybridRelay) removeUDP(key hybridConnKey) {
	r.mu.Lock()
	delete(r.udpConns, key)
	r.mu.Unlock()
}

func (c *hybridUDPConn) send(packet []byte) error {
	encrypted, err := c.manager.encryptPacket(packet, c.cipher)
	if err != nil {
		return err
	}
	return globalOverlayTransport.Send(c.peerID, encrypted)
}

func (c *hybridUDPConn) close() {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		_ = c.localConn.Close()
	})
}

func (c *hybridUDPConn) touch() {
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *hybridUDPConn) idleFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.lastSeen)
}

func parseIPv4Packet(packet []byte) (ipv4PacketInfo, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return ipv4PacketInfo{}, false
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl {
		return ipv4PacketInfo{}, false
	}
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen < ihl || totalLen > len(packet) {
		return ipv4PacketInfo{}, false
	}
	var srcIP [4]byte
	var dstIP [4]byte
	copy(srcIP[:], packet[12:16])
	copy(dstIP[:], packet[16:20])
	return ipv4PacketInfo{
		srcIP:    srcIP,
		dstIP:    dstIP,
		protocol: packet[9],
		payload:  packet[ihl:totalLen],
	}, true
}

func parseTCPPacket(payload []byte) (tcpPacketInfo, bool) {
	if len(payload) < 20 {
		return tcpPacketInfo{}, false
	}
	dataOffset := int(payload[12]>>4) * 4
	if dataOffset < 20 || len(payload) < dataOffset {
		return tcpPacketInfo{}, false
	}
	return tcpPacketInfo{
		srcPort: binary.BigEndian.Uint16(payload[0:2]),
		dstPort: binary.BigEndian.Uint16(payload[2:4]),
		seq:     binary.BigEndian.Uint32(payload[4:8]),
		ack:     binary.BigEndian.Uint32(payload[8:12]),
		flags:   payload[13],
		payload: payload[dataOffset:],
	}, true
}

func parseUDPPacket(payload []byte) (udpPacketInfo, bool) {
	if len(payload) < 8 {
		return udpPacketInfo{}, false
	}
	udpLen := int(binary.BigEndian.Uint16(payload[4:6]))
	if udpLen < 8 || udpLen > len(payload) {
		return udpPacketInfo{}, false
	}
	return udpPacketInfo{
		srcPort: binary.BigEndian.Uint16(payload[0:2]),
		dstPort: binary.BigEndian.Uint16(payload[2:4]),
		payload: payload[8:udpLen],
	}, true
}

func buildTCPPacket(
	srcIP [4]byte,
	dstIP [4]byte,
	srcPort uint16,
	dstPort uint16,
	seq uint32,
	ack uint32,
	flags uint8,
	payload []byte,
) ([]byte, error) {
	ip4 := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    net.IP(srcIP[:]),
		DstIP:    net.IP(dstIP[:]),
	}
	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(srcPort),
		DstPort: layers.TCPPort(dstPort),
		Seq:     seq,
		Ack:     ack,
		Window:  math.MaxUint16,
		FIN:     flags&tcpFlagFIN != 0,
		SYN:     flags&tcpFlagSYN != 0,
		RST:     flags&tcpFlagRST != 0,
		PSH:     flags&tcpFlagPSH != 0,
		ACK:     flags&tcpFlagACK != 0,
	}
	if err := tcp.SetNetworkLayerForChecksum(ip4); err != nil {
		return nil, err
	}

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ip4, tcp, gopacket.Payload(payload)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildUDPPacket(srcIP [4]byte, dstIP [4]byte, srcPort uint16, dstPort uint16, payload []byte) ([]byte, error) {
	ip4 := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Protocol: layers.IPProtocolUDP,
		SrcIP:    net.IP(srcIP[:]),
		DstIP:    net.IP(dstIP[:]),
	}
	udp := &layers.UDP{
		SrcPort: layers.UDPPort(srcPort),
		DstPort: layers.UDPPort(dstPort),
	}
	if err := udp.SetNetworkLayerForChecksum(ip4); err != nil {
		return nil, err
	}

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ip4, udp, gopacket.Payload(payload)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildICMPEchoReplyPacket(srcIP [4]byte, dstIP [4]byte, requestPayload []byte) ([]byte, error) {
	if len(requestPayload) < 8 {
		return nil, fmt.Errorf("ICMP echo payload too short: %d", len(requestPayload))
	}

	ip4 := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Protocol: layers.IPProtocolICMPv4,
		SrcIP:    net.IP(srcIP[:]),
		DstIP:    net.IP(dstIP[:]),
	}
	icmp := &layers.ICMPv4{
		TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoReply, 0),
		Id:       binary.BigEndian.Uint16(requestPayload[4:6]),
		Seq:      binary.BigEndian.Uint16(requestPayload[6:8]),
	}

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ip4, icmp, gopacket.Payload(requestPayload[8:])); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sendHybridTCPReset(
	peerID string,
	ip ipv4PacketInfo,
	tcp tcpPacketInfo,
	manager *OverlayManager,
	peerCipher cipher.AEAD,
) {
	ack := tcp.seq
	if tcp.flags&tcpFlagSYN != 0 || len(tcp.payload) > 0 {
		ack += uint32(len(tcp.payload))
		if tcp.flags&tcpFlagSYN != 0 {
			ack++
		}
	}
	packet, err := buildTCPPacket(ip.dstIP, ip.srcIP, tcp.dstPort, tcp.srcPort, 0, ack, tcpFlagRST|tcpFlagACK, nil)
	if err != nil {
		manager.logf("[HybridRelay] build TCP reset failed: %v", err)
		return
	}
	encrypted, err := manager.encryptPacket(packet, peerCipher)
	if err != nil {
		manager.logf("[HybridRelay] encrypt TCP reset failed: %v", err)
		return
	}
	if err := globalOverlayTransport.Send(peerID, encrypted); err != nil {
		manager.logf("[HybridRelay] send TCP reset failed: %v", err)
	}
}

func randomUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

func (m *OverlayManager) isLocalOverlayIPv4(ip [4]byte) bool {
	m.mu.RLock()
	overlayIP := m.overlayIP
	m.mu.RUnlock()

	local := net.ParseIP(overlayIP).To4()
	if local == nil {
		m.logf("[HybridRelay] Local overlay IPv4 is not ready; dropping packet for %s", ipv4String(ip))
		return false
	}
	return ip == [4]byte{local[0], local[1], local[2], local[3]}
}

func ipv4String(ip [4]byte) string {
	return net.IPv4(ip[0], ip[1], ip[2], ip[3]).String()
}

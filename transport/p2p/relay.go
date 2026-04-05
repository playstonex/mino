package p2p

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	relayProtocolVersion  byte = 0x01
	relayTypeData         byte = 0x01
	relayTypeKeepalive    byte = 0x02
	relayTypeClose        byte = 0x03
	relayTypeRegister     byte = 0x10
	relayTypeRegisterAck  byte = 0x11
	relayTypeKeepaliveAck byte = 0x12
	relayTypeRegisterNack byte = 0x13
	relayHeaderSize            = 34
	relayDeviceIDSize          = 16
)

// RelayClient connects to the VPS relay for P2P fallback.
// One relay client per device, shared across all peer connections.
type RelayClient struct {
	mu              sync.Mutex
	conn            *net.UDPConn
	remoteAddr      net.Addr
	localDeviceID   [relayDeviceIDSize]byte
	localDeviceUUID string
	token           string
	peers           map[string][relayDeviceIDSize]byte // peerID -> deviceID bytes

	onReceive func(peerID string, data []byte)

	stopCh chan struct{}
	done   chan struct{}
}

// NewRelayClient creates a new relay client.
func NewRelayClient(endpoint string, token string, deviceUUID string) (*RelayClient, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("relay resolve endpoint: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("relay connect: %w", err)
	}

	rc := &RelayClient{
		conn:            conn,
		remoteAddr:      udpAddr,
		token:           token,
		localDeviceUUID: deviceUUID,
		peers:           make(map[string][relayDeviceIDSize]byte),
		stopCh:          make(chan struct{}),
		done:            make(chan struct{}),
	}

	id, err := uuidToBytes(deviceUUID)
	if err != nil {
		conn.Close()
		return nil, err
	}
	rc.localDeviceID = id

	// Send registration.
	if err := rc.register(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay register: %w", err)
	}

	// Start receive loop.
	go rc.receiveLoop()

	// Start keepalive loop.
	go rc.keepaliveLoop()

	return rc, nil
}

// SetReceiveHandler sets the callback for incoming packets.
func (r *RelayClient) SetReceiveHandler(fn func(peerID string, data []byte)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onReceive = fn
}

// AddPeer registers a known peer for relay routing.
func (r *RelayClient) AddPeer(peerID string) error {
	id, err := uuidToBytes(peerID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.peers[peerID] = id
	r.mu.Unlock()
	return nil
}

// SendToPeer sends data to a peer via the relay.
func (r *RelayClient) SendToPeer(peerID string, data []byte) error {
	r.mu.Lock()
	dstID, ok := r.peers[peerID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("relay: unknown peer %s", peerID)
	}

	pkt := r.buildPacket(relayTypeData, r.localDeviceID, dstID, data)
	_, err := r.conn.Write(pkt)
	return err
}

// LocalDeviceUUID returns this device's UUID.
func (r *RelayClient) LocalDeviceUUID() string {
	return r.localDeviceUUID
}

// Close shuts down the relay connection.
func (r *RelayClient) Close() error {
	select {
	case <-r.stopCh:
		return nil
	default:
	}
	close(r.stopCh)

	// Send close packet.
	pkt := r.buildPacket(relayTypeClose, r.localDeviceID, r.localDeviceID, nil)
	r.conn.Write(pkt)

	err := r.conn.Close()
	<-r.done
	return err
}

func (r *RelayClient) register() error {
	tokenBytes := []byte(r.token)
	payload := make([]byte, 2+len(tokenBytes))
	binary.BigEndian.PutUint16(payload[0:2], uint16(len(tokenBytes)))
	copy(payload[2:], tokenBytes)

	emptyDst := [relayDeviceIDSize]byte{}
	pkt := r.buildPacket(relayTypeRegister, r.localDeviceID, emptyDst, payload)

	fmt.Printf("[Relay] Sending registration for device %s to %s\n", r.localDeviceUUID, r.remoteAddr.String())

	// Set read deadline for registration response.
	r.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer r.conn.SetReadDeadline(time.Time{})

	if _, err := r.conn.Write(pkt); err != nil {
		fmt.Printf("[Relay] Failed to send registration packet: %v\n", err)
		return err
	}

	// Wait for register ack.
	buf := make([]byte, 1500)
	for {
		n, _, err := r.conn.ReadFrom(buf)
		if err != nil {
			fmt.Printf("[Relay] Registration timeout/error waiting for ack: %v\n", err)
			return fmt.Errorf("register ack: %w", err)
		}
		if n < relayHeaderSize {
			continue
		}
		if buf[1] == relayTypeRegisterAck {
			fmt.Printf("[Relay] Registration successful for device %s\n", r.localDeviceUUID)
			return nil
		}
		if buf[1] == relayTypeRegisterNack {
			nackMsg := string(buf[relayHeaderSize:n])
			fmt.Printf("[Relay] Registration rejected for device %s: %s\n", r.localDeviceUUID, nackMsg)
			return fmt.Errorf("relay registration rejected: %s", nackMsg)
		}
	}
}

func (r *RelayClient) receiveLoop() {
	defer close(r.done)
	buf := make([]byte, 1500)
	for {
		n, _, err := r.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-r.stopCh:
				return
			default:
			}
			continue
		}
		if n < relayHeaderSize {
			continue
		}
		if buf[0] != relayProtocolVersion {
			continue
		}
		switch buf[1] {
		case relayTypeData:
			var srcID [relayDeviceIDSize]byte
			copy(srcID[:], buf[2:18])
			peerID := bytesToUUID(srcID[:])
			payload := buf[relayHeaderSize:n]

			r.mu.Lock()
			fn := r.onReceive
			r.mu.Unlock()
			if fn != nil {
				fn(peerID, payload)
			}
		case relayTypeKeepaliveAck:
			// Server is alive.
		}
	}
}

func (r *RelayClient) keepaliveLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			pkt := r.buildPacket(relayTypeKeepalive, r.localDeviceID, r.localDeviceID, nil)
			r.conn.Write(pkt)
		}
	}
}

func (r *RelayClient) buildPacket(pktType byte, srcID, dstID [relayDeviceIDSize]byte, payload []byte) []byte {
	pkt := make([]byte, relayHeaderSize+len(payload))
	pkt[0] = relayProtocolVersion
	pkt[1] = pktType
	copy(pkt[2:18], srcID[:])
	copy(pkt[18:34], dstID[:])
	copy(pkt[relayHeaderSize:], payload)
	return pkt
}

func uuidToBytes(s string) ([relayDeviceIDSize]byte, error) {
	var id [relayDeviceIDSize]byte
	hex := make([]byte, 0, 32)
	for _, c := range []byte(s) {
		if c == '-' {
			continue
		}
		hex = append(hex, c)
	}
	if len(hex) != 32 {
		return id, fmt.Errorf("invalid UUID length: %d", len(hex))
	}
	for i := 0; i < 16; i++ {
		hi := hexVal(hex[i*2])
		lo := hexVal(hex[i*2+1])
		if hi < 0 || lo < 0 {
			return id, fmt.Errorf("invalid hex in UUID")
		}
		id[i] = byte(hi<<4 | lo)
	}
	return id, nil
}

func bytesToUUID(b []byte) string {
	if len(b) < 16 {
		return "00000000-0000-0000-0000-000000000000"
	}
	const hexChars = "0123456789abcdef"
	buf := make([]byte, 36)
	pos := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[pos] = '-'
			pos++
		}
		buf[pos] = hexChars[b[i]>>4]
		buf[pos+1] = hexChars[b[i]&0x0f]
		pos += 2
	}
	return string(buf)
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	default:
		return -1
	}
}

package mate

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/transport/p2p"
	"github.com/metacubex/mihomo/tunnel"
	"github.com/pion/webrtc/v4"
)

// P2PSignalingMessage defines the structure for exchange
type P2PSignalingMessage struct {
	Type    string `json:"type"` // "sdp" or "candidate"
	Payload string `json:"payload"`
	SDPType string `json:"sdpType,omitempty"` // "offer" or "answer"
}

// AddP2PPeer allows Swift to inject peer configuration for WebRTC
func AddP2PPeer(peerID string, signalingJSON string) error {
	var msg P2PSignalingMessage
	if err := json.Unmarshal([]byte(signalingJSON), &msg); err != nil {
		return fmt.Errorf("failed to unmarshal signaling: %w", err)
	}

	m := p2p.GetManager()

	switch msg.Type {
	case "sdp":
		sdpType := webrtc.SDPTypeOffer
		if msg.SDPType == "answer" {
			sdpType = webrtc.SDPTypeAnswer
		}

		m.Mu.RLock()
		pc, ok := m.Peers[peerID]
		m.Mu.RUnlock()

		if sdpType == webrtc.SDPTypeAnswer {
			// We are the offerer and received an answer from the remote peer.
			// The existing PeerConnection MUST be reused — it holds the local
			// offer. Destroying it and creating a new one would put the new PC
			// in "stable" state with no local offer, making
			// SetRemoteDescription(answer) illegal.
			if !ok || pc == nil {
				return fmt.Errorf("no PeerConnection for peer %s to apply answer", peerID)
			}
		} else {
			// We are the answerer and received an offer.
			// Create a fresh PeerConnection (destroying old one if any).
			if ok {
				m.RemovePeer(peerID)
			}
			var err error
			pc, err = m.NewPeer(peerID)
			if err != nil {
				return err
			}

			// Answerer must register OnDataChannel before SetRemoteDescription
			setupDataChannelListener(peerID, pc)
		}

		// Ensure SDP ends with \r\n (pion's SDP parser requires it;
		// TrimSpace on the server may have stripped the trailing newline).
		sdp := strings.TrimRight(msg.Payload, "\r\n") + "\r\n"

		err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: sdpType,
			SDP:  sdp,
		})
		if err != nil {
			// Clean up failed PeerConnection so it can be retried
			m.RemovePeer(peerID)
			return err
		}

		if sdpType == webrtc.SDPTypeOffer {
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return err
			}
			err = pc.SetLocalDescription(answer)
			if err != nil {
				return err
			}

			if m.OnLocalDescription != nil {
				m.OnLocalDescription(peerID, answer.SDP, "answer")
			}
		}

	case "candidate":
		// ICE candidates must be added to an existing PeerConnection that
		// already has a remote description set. Never create a new PC here.
		m.Mu.RLock()
		pc, ok := m.Peers[peerID]
		m.Mu.RUnlock()

		if !ok || pc == nil {
			return fmt.Errorf("no PeerConnection for peer %s to add ICE candidate", peerID)
		}

		err := pc.AddICECandidate(webrtc.ICECandidateInit{
			Candidate: msg.Payload,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

type p2pUDPPacket struct {
	data []byte
	addr net.Addr
	pc   net.PacketConn
}

func (p *p2pUDPPacket) Data() []byte {
	return p.data
}
func (p *p2pUDPPacket) WriteBack(b []byte, addr net.Addr) (n int, err error) {
	return p.pc.WriteTo(b, addr)
}
func (p *p2pUDPPacket) Drop() {
	p.data = nil
}
func (p *p2pUDPPacket) LocalAddr() net.Addr {
	return p.addr
}

// StartP2POffer initiates a P2P connection as an offerer
func StartP2POffer(peerID string) error {
	m := p2p.GetManager()
	pc, err := m.NewPeer(peerID)
	if err != nil {
		return err
	}

	dc, err := pc.CreateDataChannel("mate-overlay", &webrtc.DataChannelInit{})
	if err != nil {
		return err
	}

	setupDataChannelListener(peerID, pc)

	packetConn := p2p.NewPacketDataChannelConn(dc)
	dc.OnOpen(func() {
		m.RegisterPacketConn(peerID, packetConn)
		globalOverlayTransport.AttachPeerPacketConn(peerID, packetConn)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}

	err = pc.SetLocalDescription(offer)
	if err != nil {
		return err
	}

	if m.OnLocalDescription != nil {
		m.OnLocalDescription(peerID, offer.SDP, "offer")
	}

	return nil
}

// SetICEServers configures ICE servers for WebRTC connections
func SetICEServers(serverURLs []string) {
	p2p.GetManager().SetICEServers(serverURLs)
}

// SetICEServersJSON configures ICE servers (including TURN with credentials) from a JSON string.
// Gomobile can export string parameters but not []string, so this is the Swift-callable version.
// JSON format: [{"urls":["stun:host:port"]}, {"urls":["turn:host:port"], "username":"u", "credential":"p"}]
func SetICEServersJSON(jsonStr string) error {
	return p2p.GetManager().SetICEServersFromJSON(jsonStr)
}

// RemoveP2PPeer removes a P2P peer connection to allow retry
func RemoveP2PPeer(peerID string) {
	p2p.GetManager().RemovePeer(peerID)
}

func setupDataChannelListener(peerID string, pc *webrtc.PeerConnection) {
	m := p2p.GetManager()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		label := dc.Label()
		if label == "mate-overlay" {
			packetConn := p2p.NewPacketDataChannelConn(dc)
			dc.OnOpen(func() {
				m.RegisterPacketConn(peerID, packetConn)
				globalOverlayTransport.AttachPeerPacketConn(peerID, packetConn)
			})
			return
		}

		// Handle dynamic multi-channel muxing natively
		parts := strings.SplitN(label, "|", 2)
		if len(parts) != 2 {
			return
		}
		netType := parts[0]
		hostPort := parts[1]

		if netType == "tcp" {
			// Register OnMessage BEFORE OnOpen to prevent race conditions dropping the first packet!
			conn := p2p.NewDataChannelConn(dc)
			dc.OnOpen(func() {
				host, port, err := net.SplitHostPort(hostPort)
				if err != nil {
					return
				}
				portInt, _ := strconv.Atoi(port)
				ipAddr, _ := netip.ParseAddr(host)

				if ipAddr.IsPrivate() || strings.HasPrefix(ipAddr.String(), "100.") {
					// Target is a local service on this peer.
					// gVisor's netstack cannot route 127.0.0.1, so we
					// bypass gVisor entirely by dialing via raw syscall.
					go relayToLocalTCP(conn, "127.0.0.1", portInt)
					return
				}

				metadata := &C.Metadata{
					NetWork: C.TCP,
					Type:    C.INNER,
					DstIP:   ipAddr,
					DstPort: uint16(portInt),
				}
				ctx := icontext.NewConnContext(conn, metadata)
				tunnel.TCPIn() <- ctx
			})
		} else if netType == "udp" {
			packetConn := p2p.NewPacketDataChannelConn(dc)
			dc.OnOpen(func() {
				host, port, err := net.SplitHostPort(hostPort)
				if err != nil {
					return
				}
				portInt, _ := strconv.Atoi(port)
				ipAddr, _ := netip.ParseAddr(host)

				if ipAddr.IsPrivate() || strings.HasPrefix(ipAddr.String(), "100.") {
					ipAddr = netip.MustParseAddr("127.0.0.1")
				}

				metadata := &C.Metadata{
					NetWork: C.UDP,
					Type:    C.INNER,
					DstIP:   ipAddr,
					DstPort: uint16(portInt),
				}
				go func() {
					defer packetConn.Close()
					buf := make([]byte, 65535)
					for {
						n, _, err := packetConn.ReadFrom(buf)
						if err != nil {
							break
						}
						// Inject UDP packet into tun
						pkt := &p2pUDPPacket{
							data: append([]byte(nil), buf[:n]...),
							addr: packetConn.LocalAddr(),
							pc:   packetConn,
						}
						tunnel.Tunnel.HandleUDPPacket(pkt, metadata.Clone())
					}
				}()
			})
		}
	})
}

// relayToLocalTCP creates a raw OS socket to 127.0.0.1, bypassing
// gVisor's netstack (which cannot route loopback), and relays data
// bidirectionally between the DataChannel and the local TCP service.
func relayToLocalTCP(dcConn *p2p.DataChannelConn, host string, port int) {
	fmt.Fprintf(os.Stderr, "[relayToLocalTCP] connecting to %s:%d\n", host, port)
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[relayToLocalTCP] syscall.Socket failed: %v\n", err)
		dcConn.Close()
		return
	}
	defer syscall.Close(fd)

	ip4 := net.ParseIP(host).To4()
	addr := &syscall.SockaddrInet4{Port: port}
	copy(addr.Addr[:], ip4)

	err = syscall.Connect(fd, addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[relayToLocalTCP] syscall.Connect to %s:%d failed: %v\n", host, port, err)
		dcConn.Close()
		return
	}
	fmt.Fprintf(os.Stderr, "[relayToLocalTCP] connected to %s:%d, starting relay\n", host, port)

	// net.FileConn dups the fd so we get a standard net.Conn.
	file := os.NewFile(uintptr(fd), "")
	tcpConn, err := net.FileConn(file)
	file.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[relayToLocalTCP] net.FileConn failed: %v\n", err)
		dcConn.Close()
		return
	}
	defer tcpConn.Close()

	// Bidirectional relay: DataChannel <-> local TCP socket
	go func() {
		n, err := io.Copy(dcConn, tcpConn)
		fmt.Fprintf(os.Stderr, "[relayToLocalTCP] dcConn<-tcpConn done: n=%d err=%v\n", n, err)
		dcConn.Close()
	}()
	n, err := io.Copy(tcpConn, dcConn)
	fmt.Fprintf(os.Stderr, "[relayToLocalTCP] tcpConn<-dcConn done: n=%d err=%v\n", n, err)
}

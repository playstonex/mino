package mate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/metacubex/mihomo/transport/p2p"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// ---------------------------------------------------------------------------
// Buffer pools — reuse allocations on hot paths (TUN read/write, crypto)
// ---------------------------------------------------------------------------

// tunReadPool provides 65535-byte buffers for TUN fd reads.
var tunReadPool = sync.Pool{
	New: func() any { b := make([]byte, 65535); return &b },
}

// tunWritePool provides buffers for TUN fd writes (4-byte header + MTU).
var tunWritePool = sync.Pool{
	New: func() any { b := make([]byte, 4+65535); return &b },
}

// packetPool provides buffers for intermediate packet copies.
var packetPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 1600); return &b },
}

// ---------------------------------------------------------------------------
// Config — parsed from JSON passed by Swift
// ---------------------------------------------------------------------------

// OverlayConfig is the JSON configuration passed from Swift to start the
// overlay manager. All fields mirror the Swift OverlayTunnelConfiguration +
// key material fields that Swift collects before calling into Go.
type OverlayConfig struct {
	ServerURL        string `json:"serverUrl"`
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	LanID            string `json:"lanId"`
	DeviceID         string `json:"deviceId"`
	DeviceUUID       string `json:"deviceUuid"`
	PrivateKeyBase64 string `json:"privateKeyBase64"`
	Platform         string `json:"platform"`
	DeviceName       string `json:"deviceName"`
	TunnelFd         int    `json:"tunnelFd"`
	Mode             string `json:"mode"`             // "overlay" | "hybrid"
	HybridConfigPath string `json:"hybridConfigPath"` // hybrid mode only
	HomeDir          string `json:"homeDir"`
	PublicKeyBase64  string `json:"publicKeyBase64"`   // optional, computed if empty
}

// ---------------------------------------------------------------------------
// Control plane API models (mirror Swift OverlayControlClient)
// ---------------------------------------------------------------------------

type overlayEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

type overlayRegisterRequest struct {
	PublicKey  string             `json:"publicKey"`
	Platform   string             `json:"platform"`
	DeviceName string             `json:"deviceName"`
	Candidates []overlayCandidate `json:"candidates"`
}

type overlayRegisterResult struct {
	Device        overlayDeviceRecord `json:"device"`
	Peers         []overlayPeer       `json:"peers"`
	RelayEndpoint string              `json:"relayEndpoint"`
}

type overlayPeersResponse struct {
	Items []overlayPeer `json:"items"`
}

type overlayDeviceRecord struct {
	ID        string `json:"id"`
	OverlayIP string `json:"overlayIp"`
}

type overlayPeer struct {
	ID            string             `json:"id"`
	DeviceName    string             `json:"deviceName"`
	Platform      string             `json:"platform"`
	OverlayIP     string             `json:"overlayIp"`
	PublicKey     string             `json:"publicKey"`
	Status        string             `json:"status"`
	TransportType string             `json:"transportType"`
	Candidates    []overlayCandidate `json:"candidates,omitempty"`
}

type overlayCandidate struct {
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	Type       string `json:"type"`
	ObservedAt string `json:"observedAt,omitempty"` // ISO8601 string for gomobile compat
}

type publishCandidatesRequest struct {
	Candidates []overlayCandidate `json:"candidates"`
}

// ---------------------------------------------------------------------------
// Snapshot (must match Swift OverlayRuntimeSnapshot EXACTLY)
// ---------------------------------------------------------------------------

type overlaySnapshot struct {
	Mode           string            `json:"mode"`
	ServerURL      string            `json:"serverUrl"`
	LanID          string            `json:"lanId"`
	DeviceID       string            `json:"deviceId"`
	OverlayIP      string            `json:"overlayIp"`
	PeerCount      int               `json:"peerCount"`
	PeerIDs        []string          `json:"peerIds"`
	ConnectionMode string            `json:"connectionMode"`
	Peers          []overlayPeerInfo `json:"peers"`
}

type overlayPeerInfo struct {
	ID             string `json:"id"`
	DeviceName     string `json:"deviceName"`
	Platform       string `json:"platform"`
	OverlayIP      string `json:"overlayIp"`
	PublicKey      string `json:"publicKey"`
	Status         string `json:"status"`
	TransportType  string `json:"transportType"`
}

// ---------------------------------------------------------------------------
// Peer signaling state machine
// ---------------------------------------------------------------------------

type peerSignalingState int

const (
	peerStateIdle peerSignalingState = iota
	peerStateSDPReceived
	peerStateConnected
	peerStateFailed

	// maxP2PFailures is the number of consecutive P2P failures before entering
	// cooldown (relay-only mode). After cooldown expires, P2P is retried.
	maxP2PFailures = 3
	// p2pCooldownDuration is how long a peer stays in relay-only mode after
	// reaching maxP2PFailures. This prevents resource-wasting retry loops
	// when NAT traversal is impossible (e.g. both peers behind symmetric NAT).
	p2pCooldownDuration = 2 * time.Minute
)

func (s peerSignalingState) String() string {
	switch s {
	case peerStateIdle:
		return "idle"
	case peerStateSDPReceived:
		return "sdpReceived"
	case peerStateConnected:
		return "connected"
	case peerStateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// OverlayManager — central overlay protocol manager
// ---------------------------------------------------------------------------

// OverlayManager owns all overlay protocol logic: registration, peer
// discovery, signaling, crypto, routing, and TUN I/O.  It replaces the
// equivalent Swift code in PacketTunnelProvider + OverlayRouteEngine +
// OverlayControlClient with a single Go type that runs inside the mate
// framework alongside mihomo.
type OverlayManager struct {
	mu sync.RWMutex

	config     *OverlayConfig
	httpClient *http.Client

	// Registration state
	deviceID  string
	overlayIP string
	peers     []overlayPeer
	knownPeerIDs map[string]bool

	// Per-peer crypto: deviceID -> sharedKey (raw bytes kept for re-derivation check)
	peerKeys map[string][]byte
	// Per-peer cached AES-GCM cipher instances — avoids re-creating cipher on every packet.
	peerCiphers map[string]cipher.AEAD

	// Signaling state
	peerStates     map[string]peerSignalingState
	seenCandidates map[string]map[string]bool // peerID -> set of sigKeys
	peerUfrags     map[string]string
	peerSessionIDs map[string]string
	offeredPeers   map[string]bool
	failedPeers    map[string]bool

	// Per-peer P2P failure tracking
	peerFailCount     map[string]int       // peerID -> consecutive P2P failure count
	peerCooldownUntil map[string]time.Time // peerID -> time when cooldown expires

	// Route table: overlayIP -> peerID
	routes map[string]string

	// Transport
	platform PlatformInterface

	// Lifecycle
	running  atomic.Bool
	cancelCh chan struct{}
	wg       sync.WaitGroup
	tunFd    int

	// TUN diagnostic counter (capped at 30)
	tunPktLogged atomic.Int32

	// Computed public key
	publicKeyBase64 string
	privateKey      [32]byte

	// Log level control — suppress per-packet logs after startup
	debugPacketLog atomic.Bool
}

// maxSeenPerPeer caps the seenCandidates set per peer to prevent unbounded growth.
const maxSeenPerPeer = 200

// NewOverlayManager creates a new (stopped) OverlayManager.
func NewOverlayManager() *OverlayManager {
	return &OverlayManager{
		knownPeerIDs:      make(map[string]bool),
		peerKeys:          make(map[string][]byte),
		peerCiphers:       make(map[string]cipher.AEAD),
		peerStates:        make(map[string]peerSignalingState),
		seenCandidates:    make(map[string]map[string]bool),
		peerUfrags:        make(map[string]string),
		peerSessionIDs:    make(map[string]string),
		offeredPeers:      make(map[string]bool),
		failedPeers:       make(map[string]bool),
		peerFailCount:     make(map[string]int),
		peerCooldownUntil: make(map[string]time.Time),
		routes:            make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

// Start parses the JSON config, registers with the control plane, discovers
// peers, derives shared keys, wires P2P callbacks, and starts background
// goroutines for signaling and (in overlay-only mode) TUN I/O.
func (m *OverlayManager) Start(configJSON string, platform PlatformInterface) error {
	if m.running.Load() {
		return fmt.Errorf("overlay manager already running")
	}

	// 1. Parse config
	var cfg OverlayConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("overlay config parse: %w", err)
	}
	m.config = &cfg
	m.platform = platform
	m.tunFd = cfg.TunnelFd

	// 2. Decode and validate private key
	privateKeyBytes, err := base64.StdEncoding.DecodeString(cfg.PrivateKeyBase64)
	if err != nil {
		return fmt.Errorf("overlay private key decode: %w", err)
	}
	if len(privateKeyBytes) != 32 {
		return fmt.Errorf("overlay private key must be 32 bytes, got %d", len(privateKeyBytes))
	}
	copy(m.privateKey[:], privateKeyBytes)

	// 3. Compute public key if not provided
	if cfg.PublicKeyBase64 != "" {
		m.publicKeyBase64 = cfg.PublicKeyBase64
	} else {
		// curve25519 public key from private key
		pub, err := curve25519.X25519(m.privateKey[:], curve25519.Basepoint)
		if err != nil {
			return fmt.Errorf("overlay public key compute: %w", err)
		}
		m.publicKeyBase64 = base64.StdEncoding.EncodeToString(pub)
	}

	// 4. HTTP client
	m.httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		},
	}

	// 5. Register with server (retry up to 3 times for transient errors)
	m.logf("[Overlay-Go] Registering with control plane...")
	regResult, err := m.registerWithRetry(3)
	if err != nil {
		return fmt.Errorf("overlay register: %w", err)
	}
	m.deviceID = regResult.Device.ID
	m.overlayIP = regResult.Device.OverlayIP
	m.peers = regResult.Peers
	for _, p := range regResult.Peers {
		m.knownPeerIDs[p.ID] = true
	}
	m.logf("[Overlay-Go] Registered: deviceID=%s overlayIP=%s peers=%d relayEndpoint=%s",
		m.deviceID, m.overlayIP, len(regResult.Peers), regResult.RelayEndpoint)

	// 6. Fetch peers (full list)
	peers, err := m.fetchPeers()
	if err != nil {
		m.logf("[Overlay-Go] Warning: fetchPeers failed: %v", err)
	} else {
		m.mu.Lock()
		m.peers = peers
		for _, p := range peers {
			m.knownPeerIDs[p.ID] = true
		}
		m.mu.Unlock()
	}

	// 7. Configure overlay transport (relay)
	resolvedRelay := resolveRelayEndpoint(regResult.RelayEndpoint, cfg.ServerURL)
	if err := globalOverlayTransport.Configure(resolvedRelay, cfg.AccessToken, m.deviceID); err != nil {
		m.logf("[Overlay-Go] Warning: overlay transport configure failed: %v", err)
	}
	for _, p := range m.peers {
		if err := globalOverlayTransport.RegisterPeer(p.ID); err != nil {
			m.logf("[Overlay-Go] Warning: register relay peer %s: %v", p.ID, err)
		}
	}

	// 8. Derive shared keys for all peers
	m.deriveAllPeerKeys()

	// 9. Build route table
	m.buildRouteTable()

	// 10. Configure ICE servers
	m.configureICEServers()

	// 11. Wire P2P callbacks directly (no Swift round-trip)
	m.wireP2PCallbacks()

	// 12. Mark running and start goroutines
	m.running.Store(true)
	m.cancelCh = make(chan struct{})

	// Enable per-packet debug logging for the first 30 seconds after start.
	m.debugPacketLog.Store(true)
	go func() {
		time.Sleep(30 * time.Second)
		m.debugPacketLog.Store(false)
		m.logf("[Overlay-Go] Per-packet debug logging disabled (30s elapsed)")
	}()

	// 13. Start signaling poll loop
	m.wg.Add(1)
	go m.signalingLoop()

	// 14. Start TUN fd reader (overlay-only mode)
	if cfg.Mode == "overlay" && cfg.TunnelFd > 0 {
		m.wg.Add(1)
		go m.tunReadLoop()
	}

	// 15. Wait briefly for TUN to stabilize, then initiate P2P offers
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		time.Sleep(2 * time.Second)
		m.initiateP2POffers()
	}()

	// 16. Hybrid mode: inject p2p entries into YAML
	if cfg.Mode == "hybrid" && cfg.HybridConfigPath != "" {
		if err := m.injectHybridYAML(); err != nil {
			m.logf("[Overlay-Go] Warning: hybrid YAML injection failed: %v", err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

// Stop shuts down the overlay manager, cancels background goroutines, and
// resets all internal state.
func (m *OverlayManager) Stop() error {
	if !m.running.Load() {
		return nil
	}
	m.running.Store(false)

	// Signal goroutines to stop
	if m.cancelCh != nil {
		close(m.cancelCh)
	}

	// Wait for background goroutines to finish
	m.wg.Wait()

	// Remove all P2P peers
	mgr := p2p.GetManager()
	m.mu.RLock()
	peerIDs := make([]string, 0, len(m.knownPeerIDs))
	for id := range m.knownPeerIDs {
		peerIDs = append(peerIDs, id)
	}
	m.mu.RUnlock()
	for _, id := range peerIDs {
		mgr.RemovePeer(id)
	}

	// Reset transport
	globalOverlayTransport.Reset()

	// Reset P2P callbacks
	mgr.Mu.Lock()
	mgr.OnLocalDescription = nil
	mgr.OnLocalCandidate = nil
	mgr.OnConnectionStateChange = nil
	mgr.Mu.Unlock()

	// Clear crypto state
	m.mu.Lock()
	m.peerKeys = make(map[string][]byte)
	m.peerCiphers = make(map[string]cipher.AEAD)
	m.seenCandidates = make(map[string]map[string]bool)
	m.mu.Unlock()

	m.logf("[Overlay-Go] Stopped")
	return nil
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

// SnapshotJSON returns a JSON string with the current overlay runtime state,
// compatible with the Swift OverlayRuntimeSnapshot format consumed by the
// main app's monitor UI.
func (m *OverlayManager) SnapshotJSON() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	peerInfos := make([]overlayPeerInfo, 0, len(m.peers))
	for _, p := range m.peers {
		state := m.peerStates[p.ID]
		transportType := "relay"
		switch state {
		case peerStateConnected:
			transportType = "p2p"
		case peerStateSDPReceived:
			transportType = "connecting"
		}
		dname := p.DeviceName
		if dname == "" {
			dname = "Unknown"
		}
		plat := p.Platform
		if plat == "" {
			plat = "unknown"
		}
		peerInfos = append(peerInfos, overlayPeerInfo{
			ID:             p.ID,
			DeviceName:     dname,
			Platform:       plat,
			OverlayIP:      p.OverlayIP,
			PublicKey:      p.PublicKey,
			Status:         p.Status,
			TransportType:  transportType,
		})
	}

	peerIDs := make([]string, 0, len(m.peers))
	for _, p := range m.peers {
		peerIDs = append(peerIDs, p.ID)
	}

	modeStr := m.config.Mode
	connectionMode := "raw-packet" // overlay-only: direct TUN read/write
	if modeStr == "hybrid" {
		connectionMode = "hybrid" // proxy + overlay via mihomo
	}
	if !m.running.Load() {
		connectionMode = "disconnected"
	}

	snap := overlaySnapshot{
		Mode:           modeStr,
		ServerURL:      m.config.ServerURL,
		LanID:          m.config.LanID,
		DeviceID:       m.deviceID,
		OverlayIP:      m.overlayIP,
		PeerCount:      len(m.peers),
		PeerIDs:        peerIDs,
		ConnectionMode: connectionMode,
		Peers:          peerInfos,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// Crypto — key derivation, encryption, decryption
// ---------------------------------------------------------------------------

// deriveSharedKey performs X25519 ECDH and HKDF-SHA256 to derive a shared
// symmetric key from the local private key and a peer's public key.
func (m *OverlayManager) deriveSharedKey(peerPublicKeyBase64 string) ([]byte, error) {
	peerPubBytes, err := base64.StdEncoding.DecodeString(peerPublicKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("decode peer public key: %w", err)
	}
	if len(peerPubBytes) != 32 {
		return nil, fmt.Errorf("peer public key must be 32 bytes, got %d", len(peerPubBytes))
	}

	sharedSecret, err := curve25519.X25519(m.privateKey[:], peerPubBytes)
	if err != nil {
		return nil, fmt.Errorf("X25519: %w", err)
	}
	if len(sharedSecret) == 0 {
		return nil, fmt.Errorf("X25519 produced zero-length shared secret (low-order public key)")
	}

	// HKDF-SHA256(sharedSecret, salt=empty, info="overlay-v1", length=32)
	reader := hkdf.New(sha256.New, sharedSecret, nil, []byte("overlay-v1"))
	sharedKey := make([]byte, 32)
	if _, err := io.ReadFull(reader, sharedKey); err != nil {
		return nil, fmt.Errorf("HKDF: %w", err)
	}
	return sharedKey, nil
}

// deriveAllPeerKeys iterates over all known peers and derives a shared key
// for each one that has a valid public key. Only re-derives if the peer's
// public key has changed (avoids redundant X25519+HKDF every 20s refresh).
func (m *OverlayManager) deriveAllPeerKeys() {
	m.mu.Lock()
	defer m.mu.Unlock()

	newKeys := make(map[string][]byte, len(m.peers))
	newCiphers := make(map[string]cipher.AEAD, len(m.peers))
	for _, peer := range m.peers {
		if peer.PublicKey == "" {
			continue
		}
		// Reuse existing key if peer's public key hasn't changed.
		if existingKey, ok := m.peerKeys[peer.ID]; ok {
			if existingCipher, cok := m.peerCiphers[peer.ID]; cok {
				newKeys[peer.ID] = existingKey
				newCiphers[peer.ID] = existingCipher
				continue
			}
		}
		key, err := m.deriveSharedKey(peer.PublicKey)
		if err != nil {
			m.logf("[Overlay-Go] ECDH failed for peer %s: %v", peer.ID, err)
			continue
		}
		gcm, err := newAEAD(key)
		if err != nil {
			m.logf("[Overlay-Go] AES-GCM init failed for peer %s: %v", peer.ID, err)
			continue
		}
		newKeys[peer.ID] = key
		newCiphers[peer.ID] = gcm
		m.logf("[Overlay-Go] Derived shared key for peer %s", peer.ID)
	}
	m.peerKeys = newKeys
	m.peerCiphers = newCiphers
}

// newAEAD creates an AES-256-GCM cipher from a 32-byte key.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptPacket encrypts plaintext using AES-256-GCM with a pre-cached cipher.
// The returned ciphertext has a 12-byte nonce prepended (nonce || sealed),
// matching the Swift CryptoKit AES.GCM.seal combined format.
func (m *OverlayManager) encryptPacket(plaintext []byte, gcm cipher.AEAD) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("refusing to encrypt empty packet")
	}

	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	// Seal appends to nonce slice — single allocation for nonce+ciphertext.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decryptPacket decrypts a ciphertext produced by encryptPacket using a
// pre-cached cipher. It expects the first 12 bytes to be the nonce.
func (m *OverlayManager) decryptPacket(ciphertext []byte, gcm cipher.AEAD) ([]byte, error) {
	nonceSize := gcm.NonceSize() // 12 bytes
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short: %d bytes", len(ciphertext))
	}

	nonce := ciphertext[:nonceSize]
	sealed := ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, sealed, nil)
}


// ---------------------------------------------------------------------------
// TUN fd operations (overlay-only mode)
// ---------------------------------------------------------------------------

// tunReadLoop reads raw IP packets from the TUN file descriptor (passed from
// Swift), routes them to the appropriate peer via the overlay transport.
// This goroutine only runs in pure overlay mode; in hybrid mode mihomo owns
// the TUN fd and routes overlay traffic through its p2p proxy outbounds.
func (m *OverlayManager) tunReadLoop() {
	defer m.wg.Done()

	m.logf("[Overlay-Go] TUN read loop started (fd=%d)", m.tunFd)

	// macOS/iOS utun prepends a 4-byte protocol family header to every
	// packet.  We must skip it to reach the actual IP packet.
	const utunHeaderLen = 4

	pktCount := 0
	lastLogTime := time.Now()

	for m.running.Load() {
		bufPtr := tunReadPool.Get().(*[]byte)
		buf := *bufPtr

		n, err := syscall.Read(m.tunFd, buf)
		if err != nil {
			tunReadPool.Put(bufPtr)
			// EAGAIN on non-blocking fd is not fatal — just retry.
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if m.running.Load() {
				m.logf("[Overlay-Go] TUN read error: %v", err)
			}
			return
		}

		// Skip utun protocol family header
		if n <= utunHeaderLen {
			tunReadPool.Put(bufPtr)
			continue
		}
		pktLen := n - utunHeaderLen
		packet := buf[utunHeaderLen:n]

		pktCount++
		if time.Since(lastLogTime) >= 10*time.Second {
			m.logf("[Overlay-Go] TUN read loop alive: %d packets in last interval", pktCount)
			pktCount = 0
			lastLogTime = time.Now()
		}

		version := (packet[0] >> 4) & 0x0F
		var dstIP string
		switch version {
		case 4:
			if pktLen < 20 {
				tunReadPool.Put(bufPtr)
				continue
			}
			dstIP = fmt.Sprintf("%d.%d.%d.%d", packet[16], packet[17], packet[18], packet[19])
		case 6:
			if pktLen < 40 {
				tunReadPool.Put(bufPtr)
				continue
			}
			dstIP = net.IP(packet[24:40]).String()
		default:
			tunReadPool.Put(bufPtr)
			continue
		}

		// Skip loopback to self
		m.mu.RLock()
		localIP := m.overlayIP
		m.mu.RUnlock()
		if dstIP == localIP {
			// Copy packet before returning buffer to pool
			pktCopy := make([]byte, pktLen)
			copy(pktCopy, packet)
			tunReadPool.Put(bufPtr)
			m.writeToTUN(pktCopy)
			continue
		}

		// Lookup peer by overlay IP
		m.mu.RLock()
		peerID, ok := m.routes[dstIP]
		peerCipher, hasCipher := m.peerCiphers[peerID]
		m.mu.RUnlock()

		if !ok || !hasCipher {
			tunReadPool.Put(bufPtr)
			continue
		}

		// Copy packet data before returning buffer to pool
		pktCopy := make([]byte, pktLen)
		copy(pktCopy, packet)
		tunReadPool.Put(bufPtr)

		// Encrypt
		encrypted, err := m.encryptPacket(pktCopy, peerCipher)
		if err != nil {
			m.logf("[Overlay-Go] Failed to encrypt packet for %s: %v", dstIP, err)
			continue
		}

		// Send via overlay transport
		if err := globalOverlayTransport.Send(peerID, encrypted); err != nil {
			if m.debugPacketLog.Load() {
				m.logf("[Overlay-Go] Failed to send to %s: %v", dstIP, err)
			}
		}
	}
}

// writeToTUN writes a raw IP packet to the TUN file descriptor.
// On macOS/iOS utun devices, a 4-byte protocol family header must be
// prepended (AF_INET=2 for IPv4, AF_INET6=30 for IPv6) in host byte order.
func (m *OverlayManager) writeToTUN(packet []byte) error {
	if len(packet) < 1 {
		return nil
	}
	bufPtr := tunWritePool.Get().(*[]byte)
	buf := *bufPtr
	defer tunWritePool.Put(bufPtr)

	version := (packet[0] >> 4) & 0x0F
	if version == 6 {
		binary.LittleEndian.PutUint32(buf[:4], 30) // AF_INET6
	} else {
		binary.LittleEndian.PutUint32(buf[:4], 2) // AF_INET
	}
	copy(buf[4:], packet)
	_, err := syscall.Write(m.tunFd, buf[:4+len(packet)])
	return err
}

// handleInboundPacket is called by the overlay transport when a packet
// arrives from a peer. It decrypts the payload and writes the resulting IP
// packet to the TUN fd.
func (m *OverlayManager) handleInboundPacket(peerID string, payload []byte) {
	if !m.running.Load() {
		return
	}

	m.mu.RLock()
	peerCipher, ok := m.peerCiphers[peerID]
	m.mu.RUnlock()

	if !ok {
		m.logf("[Overlay-Go] No cipher for inbound packet from %s", peerID)
		return
	}

	decrypted, err := m.decryptPacket(payload, peerCipher)
	if err != nil {
		m.logf("[Overlay-Go] Failed to decrypt packet from %s: %v", peerID, err)
		return
	}

	// Validate minimum IP packet size
	if len(decrypted) < 20 {
		return
	}

	// Write to TUN fd
	if err := m.writeToTUN(decrypted); err != nil {
		m.logf("[Overlay-Go] Failed to write packet to TUN from %s: %v", peerID, err)
	}
}

// ---------------------------------------------------------------------------
// Control plane API
// ---------------------------------------------------------------------------

// register sends a POST /api/v1/overlay/register request to the control plane.
func (m *OverlayManager) register() (*overlayRegisterResult, error) {
	reqBody := overlayRegisterRequest{
		PublicKey:  m.publicKeyBase64,
		Platform:   m.config.Platform,
		DeviceName: m.config.DeviceName,
		Candidates: nil, // no candidates on initial registration
	}

	var result overlayRegisterResult
	if err := m.sendRequest("POST", "api/v1/overlay/register", reqBody, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// registerWithRetry calls register up to maxRetries times with exponential
// backoff for transient network errors.
func (m *OverlayManager) registerWithRetry(maxRetries int) (*overlayRegisterResult, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		result, err := m.register()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt < maxRetries {
			m.logf("[Overlay-Go] Registration failed (attempt %d/%d), retrying in %ds: %v",
				attempt, maxRetries, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return nil, fmt.Errorf("registration failed after %d retries: %w", maxRetries, lastErr)
}

// fetchPeers sends a GET /api/v1/overlay/peers request.
func (m *OverlayManager) fetchPeers() ([]overlayPeer, error) {
	var resp overlayPeersResponse
	if err := m.sendRequest("GET", "api/v1/overlay/peers", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// publishCandidates sends a POST /api/v1/overlay/candidates request with
// retry logic (3 retries with exponential backoff), matching the Swift
// publishWithRetry implementation.
func (m *OverlayManager) publishCandidates(candidates []overlayCandidate, peerID string) error {
	reqBody := publishCandidatesRequest{Candidates: candidates}
	var lastErr error

	for attempt := 1; attempt <= 3; attempt++ {
		err := m.sendRequest("POST", "api/v1/overlay/candidates", reqBody, nil)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < 3 {
			m.logf("[Overlay-Go] Publish failed (attempt %d/3) for peer %s, retrying in %ds: %v",
				attempt, peerID, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return fmt.Errorf("publish failed after 3 retries for peer %s: %w", peerID, lastErr)
}

// sendRequest is a generic HTTP helper that sends a request to the control
// plane, parses the overlay envelope, checks success, and unmarshals the
// response data.
func (m *OverlayManager) sendRequest(method, path string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = strings.NewReader(string(bodyBytes))
	}

	url := strings.TrimRight(m.config.ServerURL, "/") + "/" + path
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.config.AccessToken)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	var envelope overlayEnvelope
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return fmt.Errorf("parse envelope: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := envelope.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("server error: %s", msg)
	}
	if !envelope.Success {
		return fmt.Errorf("server error: %s", envelope.Error)
	}

	// If result is nil, caller does not care about the response body.
	if result == nil {
		return nil
	}

	if len(envelope.Data) == 0 {
		return fmt.Errorf("empty response data")
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return fmt.Errorf("unmarshal response data: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Route table
// ---------------------------------------------------------------------------

// buildRouteTable creates a mapping from overlay IP to peer device ID
// and updates the per-peer crypto keys.
func (m *OverlayManager) buildRouteTable() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.routes = make(map[string]string, len(m.peers))
	for _, peer := range m.peers {
		if peer.OverlayIP != "" {
			m.routes[peer.OverlayIP] = peer.ID
		}
	}
	m.logf("[Overlay-Go] Route table built: %d entries", len(m.routes))
}

// ---------------------------------------------------------------------------
// Signaling poll loop
// ---------------------------------------------------------------------------

// signalingLoop runs in a background goroutine, polling the control plane
// for remote signaling every 5 seconds, with a full peer sync + route
// refresh every 20 seconds (4th iteration).
func (m *OverlayManager) signalingLoop() {
	defer m.wg.Done()
	iteration := 0
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.cancelCh:
			return
		case <-ticker.C:
			if !m.running.Load() {
				return
			}
			m.pollRemoteSignaling()

			iteration++
			if iteration%4 == 0 {
				m.refreshOverlayRuntime()
			}
		}
	}
}

// pollRemoteSignaling fetches peers with their signaling candidates from
// the control plane and injects SDPs and ICE candidates into the P2P
// manager. This is a direct port of the Swift logic (~190 lines).
func (m *OverlayManager) pollRemoteSignaling() {
	peers, err := m.fetchPeers()
	if err != nil {
		m.logf("[Overlay-Go] Signaling poll failed: %v", err)
		return
	}

	m.mu.Lock()
	myDeviceID := m.deviceID

	for _, peer := range peers {
		if len(peer.Candidates) == 0 {
			continue
		}

		state := m.peerStates[peer.ID]

		// Diagnostic logging
		sdpCount := 0
		iceCount := 0
		for _, c := range peer.Candidates {
			if c.IP == "sdp" {
				sdpCount++
			} else if c.IP == "candidate" {
				iceCount++
			}
		}
		m.logf("[Overlay-Go] Fetched peer %s: %d candidates (sdp=%d, ice=%d), state=%s",
			truncateID(peer.ID), len(peer.Candidates), sdpCount, iceCount, state)

		// Separate SDP and ICE candidates
		var sdpCandidates []overlayCandidate
		var iceCandidates []overlayCandidate

		// Skip P2P signaling for peers in cooldown (relay-only).
		if cooldownUntil, ok := m.peerCooldownUntil[peer.ID]; ok && time.Now().Before(cooldownUntil) {
			continue
		}
		// Cooldown expired — reset failure tracking to allow retry.
		if _, hadCooldown := m.peerCooldownUntil[peer.ID]; hadCooldown {
			delete(m.peerCooldownUntil, peer.ID)
			delete(m.peerFailCount, peer.ID)
			m.logf("[Overlay-Go] P2P cooldown expired for %s, will retry", truncateID(peer.ID))
		}
		for _, c := range peer.Candidates {
			if c.IP == "sdp" {
				sdpCandidates = append(sdpCandidates, c)
			} else if c.IP == "candidate" {
				iceCandidates = append(iceCandidates, c)
			}
		}

		// Process SDPs first
		isOfferer := myDeviceID < peer.ID
		type latestSDPInfo struct {
			candidate overlayCandidate
			sigKey    string
			sdpType   string
		}
		var latestSDP *latestSDPInfo

		for _, candidate := range sdpCandidates {
			if candidate.Type == "" {
				continue
			}

			sigKey := fmt.Sprintf("%s:%d:%s", candidate.IP, candidate.Port, candidate.Type)
			sdpType := "offer"
			if candidate.Port == 1 {
				sdpType = "answer"
			}

			// Skip SDPs of the wrong type for our role
			if isOfferer && sdpType == "offer" {
				m.markSeen(peer.ID, sigKey)
				continue
			}
			if !isOfferer && sdpType == "answer" {
				m.markSeen(peer.ID, sigKey)
				continue
			}

			// Skip already-processed SDPs
			if m.hasSeen(peer.ID, sigKey) {
				continue
			}

			// If peer is in failed state, check for new session
			currentState := m.peerStates[peer.ID]
			if currentState == peerStateFailed {
				newSessionID := extractSessionID(candidate.Type)
				oldSessionID := m.peerSessionIDs[peer.ID]
				if oldSessionID != "" && newSessionID == oldSessionID {
					m.markSeen(peer.ID, sigKey)
					continue
				}
				// New session detected — reset state
				m.logf("[Overlay-Go] New SDP session detected for %s, resetting from failed state", truncateID(peer.ID))
				m.peerStates[peer.ID] = peerStateIdle
				delete(m.failedPeers, peer.ID)
				delete(m.peerUfrags, peer.ID)
				delete(m.peerSessionIDs, peer.ID)
				delete(m.seenCandidates, peer.ID)
			}

			latestSDP = &latestSDPInfo{
				candidate: candidate,
				sigKey:    sigKey,
				sdpType:   sdpType,
			}
		}

		if latestSDP != nil {
			currentState := m.peerStates[peer.ID]

			// Answerer in failed state receiving a new offer — recover
			if currentState == peerStateFailed && !isOfferer && latestSDP.sdpType == "offer" {
				m.logf("[Overlay-Go] Received new offer from peer %s, clearing failed state", truncateID(peer.ID))
				delete(m.peerSessionIDs, peer.ID)
				delete(m.peerUfrags, peer.ID)
				delete(m.failedPeers, peer.ID)
				currentState = peerStateIdle
			}

			if currentState == peerStateConnected {
				m.logf("[Overlay-Go] Skipping new %s from %s, already connected", latestSDP.sdpType, truncateID(peer.ID))
			} else if currentState == peerStateSDPReceived && isOfferer && latestSDP.sdpType == "answer" {
				preview := truncateStr(latestSDP.candidate.Type, 120)
				m.logf("[Overlay-Go] Injecting remote sdp (%s) from peer %s, len=%d, preview: %s",
					latestSDP.sdpType, peer.ID, len(latestSDP.candidate.Type), preview)

				msg := P2PSignalingMessage{Type: "sdp", Payload: latestSDP.candidate.Type, SDPType: latestSDP.sdpType}
				if sigJSON, err := json.Marshal(msg); err == nil {
					if injErr := AddP2PPeer(peer.ID, string(sigJSON)); injErr != nil {
						m.logf("[Overlay-Go] Failed to inject SDP: %v", injErr)
						m.peerStates[peer.ID] = peerStateFailed
						m.failedPeers[peer.ID] = true
					} else {
						m.markSeen(peer.ID, latestSDP.sigKey)
						m.peerUfrags[peer.ID] = extractUfrag(latestSDP.candidate.Type)
						if sid := extractSessionID(latestSDP.candidate.Type); sid != "" {
							m.peerSessionIDs[peer.ID] = sid
						}
					}
				}
			} else if currentState == peerStateSDPReceived {
				m.logf("[Overlay-Go] Skipping new %s from %s, SDP already in progress (%s)",
					latestSDP.sdpType, truncateID(peer.ID), currentState)
				m.markSeen(peer.ID, latestSDP.sigKey)
			} else if currentState == peerStateFailed {
				m.logf("[Overlay-Go] Skipping %s from %s, peer connection failed (traffic will use relay)",
					latestSDP.sdpType, truncateID(peer.ID))
				m.markSeen(peer.ID, latestSDP.sigKey)
			} else {
				// State is idle — safe to inject
				preview := truncateStr(latestSDP.candidate.Type, 120)
				m.logf("[Overlay-Go] Injecting remote sdp (%s) from peer %s, len=%d, preview: %s",
					latestSDP.sdpType, peer.ID, len(latestSDP.candidate.Type), preview)

				msg := P2PSignalingMessage{Type: "sdp", Payload: latestSDP.candidate.Type, SDPType: latestSDP.sdpType}
				if sigJSON, err := json.Marshal(msg); err == nil {
					if injErr := AddP2PPeer(peer.ID, string(sigJSON)); injErr != nil {
						m.logf("[Overlay-Go] Failed to inject SDP: %v", injErr)
						m.peerStates[peer.ID] = peerStateFailed
						m.failedPeers[peer.ID] = true
					} else {
						m.markSeen(peer.ID, latestSDP.sigKey)
						m.peerStates[peer.ID] = peerStateSDPReceived
						m.peerUfrags[peer.ID] = extractUfrag(latestSDP.candidate.Type)
						if sid := extractSessionID(latestSDP.candidate.Type); sid != "" {
							m.peerSessionIDs[peer.ID] = sid
						}
					}
				}
			}
		}

		// Only inject ICE candidates after SDP has been set
		if m.peerStates[peer.ID] != peerStateSDPReceived && m.peerStates[peer.ID] != peerStateConnected {
			continue
		}
		currentUfrag, hasUfrag := m.peerUfrags[peer.ID]
		if !hasUfrag {
			continue
		}

		for _, candidate := range iceCandidates {
			if candidate.Type == "" {
				continue
			}

			sigKey := fmt.Sprintf("%s:%d:%s", candidate.IP, candidate.Port, candidate.Type)
			if m.hasSeen(peer.ID, sigKey) {
				continue
			}

			// Filter stale candidates from previous connection attempts
			if !strings.Contains(candidate.Type, "ufrag "+currentUfrag) {
				m.markSeen(peer.ID, sigKey)
				continue
			}

			// Filter overlay/tunnel interface candidates that are unreachable
			if p2p.FilteredICECandidate(candidate.Type) {
				m.markSeen(peer.ID, sigKey)
				continue
			}

			preview := truncateStr(candidate.Type, 120)
			m.logf("[Overlay-Go] Injecting remote candidate (ice) from peer %s, len=%d, preview: %s",
				peer.ID, len(candidate.Type), preview)

			msg := P2PSignalingMessage{Type: "candidate", Payload: candidate.Type}
			if sigJSON, err := json.Marshal(msg); err == nil {
				if injErr := AddP2PPeer(peer.ID, string(sigJSON)); injErr != nil {
					m.logf("[Overlay-Go] Failed to inject ICE candidate: %v", injErr)
				}
				m.markSeen(peer.ID, sigKey)
			}
		}
	}
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// P2P offer initiation
// ---------------------------------------------------------------------------

// initiateP2POffers iterates over all known peers where we are the offerer
// (myDeviceID < peerID) and initiates WebRTC offers for peers that haven't
// been offered yet or have failed and need a retry.
func (m *OverlayManager) initiateP2POffers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	myDeviceID := m.deviceID

	for _, peer := range m.peers {
		state := m.peerStates[peer.ID]

		// Skip connected or in-progress peers
		if state == peerStateConnected || state == peerStateSDPReceived {
			continue
		}

		// Skip peers in P2P cooldown (relay-only)
		if cooldownUntil, ok := m.peerCooldownUntil[peer.ID]; ok && time.Now().Before(cooldownUntil) {
			continue
		}
		// Cooldown expired — allow retry
		if _, hadCooldown := m.peerCooldownUntil[peer.ID]; hadCooldown {
			delete(m.peerCooldownUntil, peer.ID)
			delete(m.peerFailCount, peer.ID)
		}

		// Skip if already offered (unless failed, which allows retry)
		if m.offeredPeers[peer.ID] && state != peerStateFailed {
			continue
		}

		if myDeviceID < peer.ID {
			// We are the offerer
			if state == peerStateFailed {
				// Clear stale tracking for retry
				delete(m.peerSessionIDs, peer.ID)
				delete(m.peerUfrags, peer.ID)
				delete(m.failedPeers, peer.ID)
			} else {
				// Fresh start: purge stale candidates from server
				if len(peer.Candidates) > 0 {
					m.logf("[Overlay-Go] Purging %d stale candidates for peer %s...",
						len(peer.Candidates), peer.ID)
					for _, c := range peer.Candidates {
						sigKey := fmt.Sprintf("%s:%d:%s", c.IP, c.Port, c.Type)
						m.markSeen(peer.ID, sigKey)
					}
				}
			}

			m.logf("[Overlay-Go] Initiating P2P offer for peer %s (we are offerer)", peer.ID)
			if err := StartP2POffer(peer.ID); err != nil {
				m.logf("[Overlay-Go] Failed to start P2P offer for %s: %v", peer.ID, err)
			} else {
				m.offeredPeers[peer.ID] = true
				m.peerStates[peer.ID] = peerStateSDPReceived
			}
		} else {
			m.logf("[Overlay-Go] Waiting for P2P offer from peer %s (they are offerer)", peer.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Refresh overlay runtime (full peer sync)
// ---------------------------------------------------------------------------

// refreshOverlayRuntime performs a full peer sync: publishes empty
// candidates as keep-alive, fetches the latest peer list, updates routes
// and keys (only if peer list changed), prunes stale state for removed
// peers, and re-initiates P2P offers. Called every 20 seconds from the
// signaling loop.
func (m *OverlayManager) refreshOverlayRuntime() {
	// Publish empty candidates (keep-alive)
	if err := m.publishCandidates(nil, ""); err != nil {
		m.logf("[Overlay-Go] Keep-alive publish failed: %v", err)
	}

	// Fetch peers
	peers, err := m.fetchPeers()
	if err != nil {
		m.logf("[Overlay-Go] Peer refresh failed: %v", err)
		return
	}

	m.mu.Lock()
	m.peers = peers
	newPeerIDs := make(map[string]bool, len(peers))
	for _, p := range peers {
		newPeerIDs[p.ID] = true
	}

	// Detect peer list changes
	peersChanged := !mapsEqual(m.knownPeerIDs, newPeerIDs)
	if peersChanged {
		m.logf("[Overlay-Go] Peer list changed: %d -> %d peers",
			len(m.knownPeerIDs), len(newPeerIDs))
		m.pruneOverlayState(newPeerIDs)
		m.knownPeerIDs = newPeerIDs
	}
	m.mu.Unlock()

	// Update routes always (cheap), keys only when peers changed
	m.buildRouteTable()
	if peersChanged {
		m.deriveAllPeerKeys()
	}

	// Re-initiate P2P offers
	m.initiateP2POffers()
}

// pruneOverlayState removes signaling state for peers that are no longer in
// the active peer list.
func (m *OverlayManager) pruneOverlayState(validPeerIDs map[string]bool) {
	for id := range m.seenCandidates {
		if !validPeerIDs[id] {
			delete(m.seenCandidates, id)
		}
	}
	for id := range m.peerStates {
		if !validPeerIDs[id] {
			delete(m.peerStates, id)
		}
	}
	for id := range m.peerUfrags {
		if !validPeerIDs[id] {
			delete(m.peerUfrags, id)
		}
	}
	for id := range m.peerSessionIDs {
		if !validPeerIDs[id] {
			delete(m.peerSessionIDs, id)
		}
	}
	for id := range m.offeredPeers {
		if !validPeerIDs[id] {
			delete(m.offeredPeers, id)
		}
	}
	for id := range m.failedPeers {
		if !validPeerIDs[id] {
			delete(m.failedPeers, id)
		}
	}
	for id := range m.peerFailCount {
		if !validPeerIDs[id] {
			delete(m.peerFailCount, id)
		}
	}
	for id := range m.peerCooldownUntil {
		if !validPeerIDs[id] {
			delete(m.peerCooldownUntil, id)
		}
	}
	for id := range m.peerCiphers {
		if !validPeerIDs[id] {
			delete(m.peerCiphers, id)
		}
	}
	for id := range m.peerKeys {
		if !validPeerIDs[id] {
			delete(m.peerKeys, id)
		}
	}
}

// ---------------------------------------------------------------------------
// P2P callback wiring
// ---------------------------------------------------------------------------

// wireP2PCallbacks replaces the Swift round-trip callbacks on the p2p.Manager
// with direct Go handlers that publish signaling to the control plane server
// and handle connection state transitions internally.  It also wraps the
// overlay transport's platform so that inbound packets are decrypted and
// written to the TUN fd instead of being forwarded to Swift.
func (m *OverlayManager) wireP2PCallbacks() {
	mgr := p2p.GetManager()

	mgr.Mu.Lock()
	mgr.OnLocalDescription = func(peerID string, sdp string, sdpType string) {
		m.logf("[Overlay-Go] Publishing local sdp (%s) for peer %s, payload len=%d",
			sdpType, truncateID(peerID), len(sdp))
		// Encode SDP type in port field: 0=offer, 1=answer
		port := 0
		if sdpType == "answer" {
			port = 1
		}
		candidate := overlayCandidate{
			IP:   "sdp",
			Port: port,
			Type: sdp,
		}
		if err := m.publishCandidates([]overlayCandidate{candidate}, peerID); err != nil {
			m.logf("[Overlay-Go] Failed to publish local SDP: %v", err)
		}
	}
	mgr.OnLocalCandidate = func(peerID string, candidate string) {
		cand := overlayCandidate{
			IP:   "candidate",
			Port: 0,
			Type: candidate,
		}
		if err := m.publishCandidates([]overlayCandidate{cand}, peerID); err != nil {
			m.logf("[Overlay-Go] Failed to publish local ICE candidate: %v", err)
		}
	}
	mgr.OnConnectionStateChange = func(peerID string, state string) {
		m.logf("[Overlay-Go] P2P connection state for %s: %s", truncateID(peerID), state)
		m.mu.Lock()
		defer m.mu.Unlock()

		currentState := m.peerStates[peerID]

		switch state {
		case "connected":
			m.peerStates[peerID] = peerStateConnected
			delete(m.failedPeers, peerID)
			delete(m.peerFailCount, peerID)
			delete(m.peerCooldownUntil, peerID)
			hasDC := globalOverlayTransport.HasPacketConn(peerID)
			m.logf("[Overlay-Go] P2P connected for %s — DataChannel registered: %v", truncateID(peerID), hasDC)
			// DataChannel may open shortly after PeerConnection becomes connected.
			// If not yet registered, check again after a short delay.
			if !hasDC {
				go func() {
					time.Sleep(2 * time.Second)
					if globalOverlayTransport.HasPacketConn(peerID) {
						m.logf("[Overlay-Go] DataChannel for %s opened after delay", truncateID(peerID))
					} else {
						m.logf("[Overlay-Go] DataChannel for %s still NOT open after 2s — SCTP may have failed", truncateID(peerID))
					}
				}()
			}
		case "connecting":
			// Don't change state if already tracking as sdpReceived
		case "disconnected":
			m.logf("[Overlay-Go] P2P disconnected for %s, waiting for recovery...", truncateID(peerID))
		case "failed":
			m.peerStates[peerID] = peerStateFailed
			m.failedPeers[peerID] = true
			m.peerFailCount[peerID]++
			failCount := m.peerFailCount[peerID]
			if failCount >= maxP2PFailures {
				m.peerCooldownUntil[peerID] = time.Now().Add(p2pCooldownDuration)
				m.logf("[Overlay-Go] P2P failed %d times for %s, entering %v cooldown (relay-only)",
					failCount, truncateID(peerID), p2pCooldownDuration)
			} else {
				m.logf("[Overlay-Go] P2P failed for %s (attempt %d/%d), traffic will relay via VPS",
					truncateID(peerID), failCount, maxP2PFailures)
			}
		case "closed":
			if currentState == peerStateSDPReceived || currentState == peerStateIdle || currentState == peerStateFailed {
				m.logf("[Overlay-Go] Ignoring stale 'closed' callback for %s (current state: %s)",
					truncateID(peerID), currentState)
			} else {
				m.peerStates[peerID] = peerStateFailed
				m.failedPeers[peerID] = true
				m.logf("[Overlay-Go] P2P closed for %s, traffic will relay via VPS", truncateID(peerID))
			}
		}
	}
	mgr.Mu.Unlock()

	// Set the packet handler on the overlay transport so that inbound
	// packets are handled directly in Go (decrypt + write to TUN).
	globalOverlayTransport.SetPacketHandler(func(peerID string, payload []byte) {
		m.handleInboundPacket(peerID, payload)
	})
}

// ---------------------------------------------------------------------------
// ICE server configuration
// ---------------------------------------------------------------------------

// configureICEServers sets up STUN servers for WebRTC NAT discovery.
// Uses a mix of Chinese and international servers for global coverage.
// TURN servers can be added to the JSON when deployed on the VPS:
//   {"urls":["turn:VPS_IP:3478?transport=udp"], "username":"user", "credential":"pass"}
func (m *OverlayManager) configureICEServers() {
	iceConfig := `[` +
		`{"urls":["stun:stun.l.google.com:19302"]},` +
		`{"urls":["stun:stun1.l.google.com:19302"]},` +
		`{"urls":["stun:stun2.l.google.com:19302"]},` +
		`{"urls":["stun:stun.qq.com:3478"]},` +
		`{"urls":["stun:stun.aliyun.com:3478"]},` +
		`{"urls":["stun:stun.miwifi.com:3478"]},` +
		`{"urls":["stun:stun.syncthing.net:3478"]},` +
		`{"urls":["turn:ctus.playstone.info:3478?transport=udp"],"username":"bigbig","credential":"123qwe"}` +
		`]`

	if err := SetICEServersJSON(iceConfig); err != nil {
		m.logf("[Overlay-Go] Failed to configure ICE servers: %v", err)
	} else {
		m.logf("[Overlay-Go] ICE servers configured (7 STUN + 1 TURN)")
	}
}

// ---------------------------------------------------------------------------
// Hybrid YAML injection
// ---------------------------------------------------------------------------

// injectHybridYAML reads the base proxy YAML and injects a complete Virtual LAN
// overlay config block: p2p outbound entries, overlay proxy-group, and overlay
// routing rules. The base YAML contains only proxy configuration — no overlay
// placeholders. This function adds everything overlay-related as a self-contained
// block that doesn't modify any existing proxy content.
func (m *OverlayManager) injectHybridYAML() error {
	m.mu.RLock()
	hybridConfigPath := m.config.HybridConfigPath
	peers := m.peers
	deviceID := m.deviceID
	accessToken := m.config.AccessToken
	serverURL := m.config.ServerURL
	m.mu.RUnlock()

	if hybridConfigPath == "" {
		return fmt.Errorf("hybrid config path not set")
	}

	// Read existing YAML (proxy-only config generated by Swift)
	yamlBytes, err := os.ReadFile(hybridConfigPath)
	if err != nil {
		return fmt.Errorf("read hybrid config: %w", err)
	}
	proxyYaml := string(yamlBytes)

	// Resolve relay endpoint for YAML injection
	relayEndpoint := ""
	if m.httpClient != nil {
		regResult, err := m.register()
		if err == nil {
			relayEndpoint = resolveRelayEndpoint(regResult.RelayEndpoint, serverURL)
		}
	}
	if relayEndpoint == "" {
		relayEndpoint = resolveRelayEndpoint("", serverURL)
	}

	// --- Build Virtual LAN overlay block ---

	// 1. p2p outbound entries (go into proxies: section)
	var p2pOutbounds strings.Builder
	p2pOutbounds.WriteString("\n# --- Virtual LAN Outbounds ---")
	for _, peer := range peers {
		p2pOutbounds.WriteString(fmt.Sprintf("\n- name: \"p2p-%s\"", truncateID(peer.ID)))
		p2pOutbounds.WriteString("\n  type: p2p")
		p2pOutbounds.WriteString(fmt.Sprintf("\n  peer-id: \"%s\"", peer.ID))
		p2pOutbounds.WriteString(fmt.Sprintf("\n  local-device-id: \"%s\"", deviceID))
		p2pOutbounds.WriteString(fmt.Sprintf("\n  relay-endpoint: \"%s\"", relayEndpoint))
		p2pOutbounds.WriteString(fmt.Sprintf("\n  access-token: \"%s\"", accessToken))
	}

	// 2. overlay proxy-group (goes into proxy-groups: section)
	var overlayGroup strings.Builder
	overlayGroup.WriteString("\n# --- Virtual LAN Group ---")
	overlayGroup.WriteString("\n- name: overlay")
	overlayGroup.WriteString("\n  type: select")
	overlayGroup.WriteString("\n  proxies:")
	for _, peer := range peers {
		overlayGroup.WriteString(fmt.Sprintf("\n    - \"p2p-%s\"", truncateID(peer.ID)))
	}
	overlayGroup.WriteString("\n    - DIRECT")

	// 3. overlay routing rules (goes into rules: section, must be first)
	var overlayRules strings.Builder
	overlayRules.WriteString("# --- Virtual LAN Rules ---")
	overlayRules.WriteString("\n- IP-CIDR,100.96.0.0/12,overlay")

	// --- Inject overlay block into YAML ---

	// a) Insert p2p outbounds at end of proxies: section
	if idx := strings.Index(proxyYaml, "\nproxy-groups:"); idx >= 0 {
		proxyYaml = proxyYaml[:idx] + p2pOutbounds.String() + proxyYaml[idx:]
	}

	// b) Insert overlay group at end of proxy-groups: section.
	// The YAML order is: proxy-groups → rule-providers → rules.
	// We must insert before rule-providers (if present), otherwise before rules.
	insertBeforeGroup := "\nrule-providers:"
	if !strings.Contains(proxyYaml, insertBeforeGroup) {
		insertBeforeGroup = "\nrules:"
	}
	if idx := strings.Index(proxyYaml, insertBeforeGroup); idx >= 0 {
		proxyYaml = proxyYaml[:idx] + overlayGroup.String() + proxyYaml[idx:]
	}

	// c) Insert overlay rules at beginning of rules: section
	if idx := strings.Index(proxyYaml, "\nrules:"); idx >= 0 {
		// Find the end of the "rules:" line
		lineEnd := strings.Index(proxyYaml[idx+1:], "\n")
		if lineEnd >= 0 {
			insertAt := idx + 1 + lineEnd
			proxyYaml = proxyYaml[:insertAt] + "\n" + overlayRules.String() + proxyYaml[insertAt:]
		}
	}

	m.logf("[Overlay-Go] Injected overlay config: %d p2p peers", len(peers))

	// Write back
	if err := os.WriteFile(hybridConfigPath, []byte(proxyYaml), 0644); err != nil {
		return fmt.Errorf("write hybrid config: %w", err)
	}

	m.logf("[Overlay-Go] Hybrid config written with %d p2p peers", len(peers))
	m.logf("[Overlay-Go] Final YAML (after overlay injection):\n%s", proxyYaml)

	// Trigger mihomo config reload so it picks up the new p2p outbounds and
	// overlay proxy group.  We call ReloadConfig directly (same Go process)
	// instead of going through the REST API, because inside an iOS network
	// extension the TUN captures all loopback traffic.
	if globalService != nil {
		if err := globalService.ReloadConfig(hybridConfigPath); err != nil {
			m.logf("[Overlay-Go] mihomo config reload failed: %v (p2p outbounds not active)", err)
		} else {
			m.logf("[Overlay-Go] mihomo config reloaded successfully — p2p outbounds active")
		}
	} else {
		m.logf("[Overlay-Go] Warning: globalService is nil, cannot reload mihomo config")
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// logf logs a message via the platform interface if available, or to stderr.
func (m *OverlayManager) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if m.platform != nil {
		m.platform.WriteLog(msg)
		return
	}
	fmt.Fprintln(os.Stderr, msg)
}

// markSeen records a signaling key as seen for a peer. Caller must hold m.mu.
// Enforces maxSeenPerPeer to prevent unbounded memory growth.
func (m *OverlayManager) markSeen(peerID, sigKey string) {
	if m.seenCandidates[peerID] == nil {
		m.seenCandidates[peerID] = make(map[string]bool)
	}
	seen := m.seenCandidates[peerID]
	// If at capacity, clear the oldest half (simple eviction — no ordering needed
	// since stale entries are harmless, they just cause a re-process).
	if len(seen) >= maxSeenPerPeer {
		count := 0
		for k := range seen {
			delete(seen, k)
			count++
			if count >= maxSeenPerPeer/2 {
				break
			}
		}
	}
	seen[sigKey] = true
}

// hasSeen checks if a signaling key has been seen for a peer. Caller must hold m.mu.
func (m *OverlayManager) hasSeen(peerID, sigKey string) bool {
	return m.seenCandidates[peerID][sigKey]
}

// extractUfrag extracts the a=ice-ufrag: value from an SDP string.
func extractUfrag(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=ice-ufrag:") {
			return strings.TrimSpace(line[len("a=ice-ufrag:"):])
		}
	}
	return ""
}

// extractSessionID extracts the session ID from the o=- line of an SDP.
// Format: o=- <session-id> <version> IN IP4 <address>
func extractSessionID(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "o=- ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return ""
}

// resolveRelayEndpoint resolves the relay endpoint: if it starts with ":",
// prepend the server host.
func resolveRelayEndpoint(endpoint, serverURL string) string {
	if strings.HasPrefix(endpoint, ":") {
		// Parse host from server URL
		serverURL = strings.TrimRight(serverURL, "/")
		// Remove scheme
		if idx := strings.Index(serverURL, "://"); idx >= 0 {
			serverURL = serverURL[idx+3:]
		}
		// Remove path
		if idx := strings.Index(serverURL, "/"); idx >= 0 {
			serverURL = serverURL[:idx]
		}
		// Remove port
		if idx := strings.LastIndex(serverURL, ":"); idx >= 0 {
			// Only remove if it looks like a port (all digits after)
			portStr := serverURL[idx+1:]
			allDigits := true
			for _, c := range portStr {
				if c < '0' || c > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				serverURL = serverURL[:idx]
			}
		}
		return serverURL + endpoint
	}
	return endpoint
}

// truncateID returns a short prefix of a device/peer ID for log messages.
func truncateID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// truncateStr returns at most maxLen characters of s, appending "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// mapsEqual checks if two string->bool maps have the same keys.
func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

package mate

import (
	"bytes"
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
	"golang.org/x/sys/unix"
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
	BootstrapURL     string `json:"bootstrapUrl,omitempty"` // violet-next base URL for relay server list
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
	PublicKeyBase64  string `json:"publicKeyBase64"`      // optional, computed if empty
	ICEServers       string `json:"iceServers,omitempty"` // JSON string, user-configured
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
	Device         overlayDeviceRecord `json:"device"`
	Peers          []overlayPeer       `json:"peers"`
	RelayEndpoint  string              `json:"relayEndpoint"`
	RelayRegion    string              `json:"relayRegion,omitempty"`
	RelayPublicURL string              `json:"relayPublicUrl,omitempty"`
}

// relayServerEntry represents a relay server from the relay server list API.
type relayServerEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	URL      string `json:"url"`
	Priority int    `json:"priority"`
}

type relayServerListResponse struct {
	Servers []relayServerEntry `json:"servers"`
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
	RelayEndpoint  string            `json:"relayEndpoint,omitempty"`
	RelayRegion    string            `json:"relayRegion,omitempty"`
	Peers          []overlayPeerInfo `json:"peers"`
}

type overlayPeerInfo struct {
	ID            string `json:"id"`
	DeviceName    string `json:"deviceName"`
	Platform      string `json:"platform"`
	OverlayIP     string `json:"overlayIp"`
	PublicKey     string `json:"publicKey"`
	Status        string `json:"status"`
	TransportType string `json:"transportType"`
	LastPingMs    int64  `json:"lastPingMs"` // -1 = not measured, 0+ = RTT in ms
}

// ---------------------------------------------------------------------------
// Overlay control packet markers
// ---------------------------------------------------------------------------
// Decrypted overlay payloads that start with byte 0x00 are control messages
// (ping/pong). Real IP packets start with 0x45 (IPv4) or 0x60 (IPv6), so
// there is no ambiguity.
const (
	overlayControlPrefix byte = 0x00
	overlayPingType      byte = 0x01
	overlayPongType      byte = 0x02
	// Ping/pong payload: [0x00, type, 8-byte nonce]
	overlayPingSize = 10
)

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
	deviceID     string
	overlayIP    string
	peers        []overlayPeer
	knownPeerIDs map[string]bool

	// Per-peer crypto: deviceID -> sharedKey (raw bytes kept for re-derivation check)
	peerKeys map[string][]byte
	// Per-peer public keys used for the cached shared keys. If a peer rotates
	// its keypair, this lets us detect and re-derive instead of reusing stale AEADs.
	peerPublicKeys map[string]string
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

	// Relay info
	relayEndpoint string
	relayRegion   string
	relayServers  []relayServerEntry // all known relay servers

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

	// Ping tracking
	peerLastPingMs    map[string]int64      // peerID -> last measured RTT in ms (-1 = not measured)
	pendingPings      map[string]pingRecord // nonce-hex -> pending ping
	noCipherLogCounts map[string]int
	pingMu            sync.Mutex
}

type pingRecord struct {
	peerID string
	sentAt time.Time
	// result, when non-nil, receives the RTT in ms on pong. Buffered
	// capacity 1 so handleControlPacket never blocks.
	result chan int64
}

// maxSeenPerPeer caps the seenCandidates set per peer to prevent unbounded growth.
const maxSeenPerPeer = 200

// NewOverlayManager creates a new (stopped) OverlayManager.
func NewOverlayManager() *OverlayManager {
	return &OverlayManager{
		knownPeerIDs:      make(map[string]bool),
		peerKeys:          make(map[string][]byte),
		peerPublicKeys:    make(map[string]string),
		peerCiphers:       make(map[string]cipher.AEAD),
		peerStates:        make(map[string]peerSignalingState),
		seenCandidates:    make(map[string]map[string]bool),
		peerUfrags:        make(map[string]string),
		peerSessionIDs:    make(map[string]string),
		peerLastPingMs:    make(map[string]int64),
		pendingPings:      make(map[string]pingRecord),
		offeredPeers:      make(map[string]bool),
		failedPeers:       make(map[string]bool),
		peerFailCount:     make(map[string]int),
		peerCooldownUntil: make(map[string]time.Time),
		routes:            make(map[string]string),
		noCipherLogCounts: make(map[string]int),
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
	started := false
	defer func() {
		if !started {
			m.cleanupPartialStart()
		}
	}()

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
	resolvedRelay := normalizeRelayEndpoint(resolveRelayEndpoint(regResult.RelayEndpoint, cfg.ServerURL))
	m.logf("[Overlay-Go] Relay endpoint from registration: raw=%q resolved=%q", regResult.RelayEndpoint, resolvedRelay)

	// 7a. Try multi-region relay selection if bootstrap URL is available
	relayServers := m.fetchRelayServers()
	if len(relayServers) > 0 {
		m.mu.Lock()
		m.relayServers = relayServers
		m.mu.Unlock()
		selectedRelay := m.selectBestRelay(relayServers, resolvedRelay)
		if selectedRelay != "" {
			resolvedRelay = selectedRelay
		}
	} else if regResult.RelayRegion != "" {
		m.mu.Lock()
		m.relayRegion = regResult.RelayRegion
		m.mu.Unlock()
	}

	if err := globalOverlayTransport.Configure(resolvedRelay, cfg.AccessToken, m.deviceID); err != nil {
		m.logf("[Overlay-Go] Warning: overlay transport configure failed: %v", err)
	} else {
		m.logf("[Overlay-Go] Overlay transport configured OK (relay=%q)", resolvedRelay)
	}
	m.mu.Lock()
	m.relayEndpoint = resolvedRelay
	m.mu.Unlock()
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

	started = true
	return nil
}

func (m *OverlayManager) cleanupPartialStart() {
	globalOverlayTransport.Reset()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = nil
	m.httpClient = nil
	m.platform = nil
	m.deviceID = ""
	m.overlayIP = ""
	m.peers = nil
	m.knownPeerIDs = make(map[string]bool)
	m.routes = make(map[string]string)
	m.peerKeys = make(map[string][]byte)
	m.peerPublicKeys = make(map[string]string)
	m.peerCiphers = make(map[string]cipher.AEAD)
	m.peerStates = make(map[string]peerSignalingState)
	m.seenCandidates = make(map[string]map[string]bool)
	m.peerUfrags = make(map[string]string)
	m.peerSessionIDs = make(map[string]string)
	m.offeredPeers = make(map[string]bool)
	m.failedPeers = make(map[string]bool)
	m.peerFailCount = make(map[string]int)
	m.peerCooldownUntil = make(map[string]time.Time)
	m.relayEndpoint = ""
	m.relayRegion = ""
	m.relayServers = nil
	m.peerLastPingMs = make(map[string]int64)
	m.noCipherLogCounts = make(map[string]int)
	m.pingMu.Lock()
	m.pendingPings = make(map[string]pingRecord)
	m.pingMu.Unlock()
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
	m.config = nil
	m.overlayIP = ""
	m.peers = nil
	m.knownPeerIDs = make(map[string]bool)
	m.routes = make(map[string]string)
	m.peerKeys = make(map[string][]byte)
	m.peerPublicKeys = make(map[string]string)
	m.peerCiphers = make(map[string]cipher.AEAD)
	m.seenCandidates = make(map[string]map[string]bool)
	m.peerLastPingMs = make(map[string]int64)
	m.noCipherLogCounts = make(map[string]int)
	m.mu.Unlock()

	// Clear pending pings
	m.pingMu.Lock()
	m.pendingPings = make(map[string]pingRecord)
	m.pingMu.Unlock()

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
		dname := p.DeviceName
		if dname == "" {
			dname = "Unknown"
		}
		plat := p.Platform
		if plat == "" {
			plat = "unknown"
		}
		pingMs := int64(-1)
		if v, ok := m.peerLastPingMs[p.ID]; ok {
			pingMs = v
		}
		// Determine actual transport path. Relay is the default; a peer only
		// becomes direct after the P2P packet DataChannel is actually usable.
		hasDirectConn := globalOverlayTransport.HasPacketConn(p.ID)
		actualTransport := "relay"
		if hasDirectConn && state == peerStateConnected {
			actualTransport = "direct"
		} else if state == peerStateSDPReceived {
			actualTransport = "connecting"
		}
		peerInfos = append(peerInfos, overlayPeerInfo{
			ID:            p.ID,
			DeviceName:    dname,
			Platform:      plat,
			OverlayIP:     p.OverlayIP,
			PublicKey:     p.PublicKey,
			Status:        p.Status,
			TransportType: actualTransport,
			LastPingMs:    pingMs,
		})
	}

	peerIDs := make([]string, 0, len(m.peers))
	for _, p := range m.peers {
		peerIDs = append(peerIDs, p.ID)
	}

	modeStr := m.config.Mode
	connectionMode := "relay"
	if modeStr == "hybrid" {
		connectionMode = "hybrid"
	}
	if !m.running.Load() {
		connectionMode = "disconnected"
	} else if modeStr != "hybrid" {
		for _, peer := range peerInfos {
			if peer.TransportType == "direct" {
				connectionMode = "p2p-direct"
				break
			}
			if peer.TransportType == "connecting" {
				connectionMode = "connecting"
			}
		}
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
		RelayEndpoint:  m.relayEndpoint,
		RelayRegion:    m.relayRegion,
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
	newPublicKeys := make(map[string]string, len(m.peers))
	newCiphers := make(map[string]cipher.AEAD, len(m.peers))
	for _, peer := range m.peers {
		if peer.PublicKey == "" {
			continue
		}
		// Reuse existing key if peer's public key hasn't changed.
		if existingKey, ok := m.peerKeys[peer.ID]; ok && m.peerPublicKeys[peer.ID] == peer.PublicKey {
			if existingCipher, cok := m.peerCiphers[peer.ID]; cok {
				newKeys[peer.ID] = existingKey
				newPublicKeys[peer.ID] = peer.PublicKey
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
		newPublicKeys[peer.ID] = peer.PublicKey
		newCiphers[peer.ID] = gcm
		m.logf("[Overlay-Go] Derived shared key for peer %s", peer.ID)
	}
	m.peerKeys = newKeys
	m.peerPublicKeys = newPublicKeys
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
// the TUN fd and the sing-tun LinkEndpoint interceptor diverts overlay packets.
func (m *OverlayManager) tunReadLoop() {
	defer m.wg.Done()

	m.logf("[Overlay-Go] TUN read loop started (fd=%d)", m.tunFd)

	// macOS/iOS utun prepends a 4-byte protocol family header to every
	// packet.  We must skip it to reach the actual IP packet.
	const utunHeaderLen = 4

	pktCount := 0
	lastLogTime := time.Now()

	for m.running.Load() {
		ready, err := waitForTunReadable(m.tunFd, 500)
		if err != nil {
			if m.running.Load() {
				m.logf("[Overlay-Go] TUN poll error: %v", err)
			}
			return
		}
		if !ready {
			continue
		}

		bufPtr := tunReadPool.Get().(*[]byte)
		buf := *bufPtr

		n, err := syscall.Read(m.tunFd, buf)
		if err != nil {
			tunReadPool.Put(bufPtr)
			// EAGAIN on non-blocking fd is not fatal — poll again.
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
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

func waitForTunReadable(fd int, timeoutMs int) (bool, error) {
	fds := []unix.PollFd{{
		Fd:     int32(fd),
		Events: unix.POLLIN,
	}}

	n, err := unix.Poll(fds, timeoutMs)
	if err == unix.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	revents := fds[0].Revents
	if revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return false, fmt.Errorf("tun fd poll revents=%#x", revents)
	}
	return revents&unix.POLLIN != 0, nil
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
		binary.NativeEndian.PutUint32(buf[:4], 30) // AF_INET6
	} else {
		binary.NativeEndian.PutUint32(buf[:4], 2) // AF_INET
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
		m.mu.Lock()
		m.noCipherLogCounts[peerID]++
		count := m.noCipherLogCounts[peerID]
		m.mu.Unlock()
		if count <= 3 || count%100 == 0 {
			m.logf("[Overlay-Go] No cipher for inbound packet from %s (count=%d)", peerID, count)
		}
		return
	}

	decrypted, err := m.decryptPacket(payload, peerCipher)
	if err != nil {
		m.logf("[Overlay-Go] Failed to decrypt packet from %s: %v", peerID, err)
		return
	}

	// Check for overlay control packets (ping/pong)
	if len(decrypted) >= 2 && decrypted[0] == overlayControlPrefix {
		m.handleControlPacket(peerID, decrypted)
		return
	}

	// Validate minimum IP packet size
	if len(decrypted) < 20 {
		return
	}

	// In hybrid mode, rewrite the destination IP from our overlay IP (100.96.x.y)
	// back to the mihomo TUN address (198.18.0.1). This completes the symmetric
	// NAT: outbound rewrites source 198.18.0.1→overlay, inbound rewrites
	// destination overlay→198.18.0.1. Without this, the TCP stack drops replies
	// because the socket is bound to 198.18.0.1 but receives packets for 100.96.x.y.
	m.mu.RLock()
	cfg := m.config
	localIP := m.overlayIP
	m.mu.RUnlock()
	if cfg != nil && cfg.Mode == "hybrid" {
		decrypted = rewriteHybridOverlayDestination(decrypted, localIP)
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
		bodyReader = bytes.NewReader(bodyBytes)
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
// Multi-region relay selection
// ---------------------------------------------------------------------------

// fetchRelayServers fetches the list of available relay servers from the
// bootstrap API (violet-next). Falls back to the registration relay endpoint
// if the API is unavailable or returns no servers.
func (m *OverlayManager) fetchRelayServers() []relayServerEntry {
	bootstrapURL := m.config.BootstrapURL
	if bootstrapURL == "" {
		return nil
	}

	url := strings.TrimRight(bootstrapURL, "/") + "/api/servers/relay"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		m.logf("[Relay-Select] Failed to create relay list request: %v", err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+m.config.AccessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		m.logf("[Relay-Select] Failed to fetch relay servers: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		m.logf("[Relay-Select] Relay server list API returned %d", resp.StatusCode)
		return nil
	}

	var envelope struct {
		Success bool                    `json:"success"`
		Data    relayServerListResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		m.logf("[Relay-Select] Failed to decode relay server list: %v", err)
		return nil
	}
	if !envelope.Success || len(envelope.Data.Servers) == 0 {
		return nil
	}

	m.logf("[Relay-Select] Fetched %d relay servers", len(envelope.Data.Servers))
	return envelope.Data.Servers
}

// probeRelayLatency sends a UDP keepalive packet to a relay endpoint and
// measures the round-trip time. Returns -1 if the probe fails or times out.
func (m *OverlayManager) probeRelayLatency(endpoint string) int64 {
	udpAddr, err := net.ResolveUDPAddr("udp", normalizeRelayEndpoint(endpoint))
	if err != nil {
		return -1
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return -1
	}
	defer conn.Close()

	// Relay probe protocol: the relay treats a 34-byte UDP packet with
	// version=0x01 and type=keepalive(0x02) as an unauthenticated latency
	// probe and echoes a response. Keep this in sync with the relay service.
	probe := make([]byte, 34)
	probe[0] = 0x01 // version
	probe[1] = 0x02 // keepalive type

	conn.SetDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := conn.Write(probe); err != nil {
		return -1
	}

	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		return -1
	}

	return time.Since(start).Milliseconds()
}

// selectBestRelay probes all available relay servers and returns the one with
// the lowest latency. Falls back to the registration relay endpoint if no
// servers respond or the list is empty.
func (m *OverlayManager) selectBestRelay(servers []relayServerEntry, fallbackEndpoint string) string {
	if len(servers) == 0 {
		return fallbackEndpoint
	}

	type probeResult struct {
		endpoint  string
		region    string
		latencyMs int64
	}

	results := make([]probeResult, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, min(len(servers), 8))
	for i, s := range servers {
		i, s := i, s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ep := normalizeRelayEndpoint(s.URL)
			lat := m.probeRelayLatency(ep)
			results[i] = probeResult{endpoint: ep, region: s.Region, latencyMs: lat}
		}()
	}
	wg.Wait()

	bestIdx := -1
	bestLat := int64(999999)
	for i, r := range results {
		if r.latencyMs >= 0 && r.latencyMs < bestLat {
			bestLat = r.latencyMs
			bestIdx = i
		}
		if r.latencyMs >= 0 {
			m.logf("[Relay-Select] %s (%s): %dms", r.endpoint, r.region, r.latencyMs)
		} else {
			m.logf("[Relay-Select] %s (%s): timeout", r.endpoint, r.region)
		}
	}

	if bestIdx >= 0 {
		chosen := results[bestIdx]
		m.logf("[Relay-Select] Selected relay: %s (%s, %dms)", chosen.endpoint, chosen.region, chosen.latencyMs)
		m.mu.Lock()
		m.relayRegion = chosen.region
		m.mu.Unlock()
		return chosen.endpoint
	}

	m.logf("[Relay-Select] No relay responded, using fallback: %s", fallbackEndpoint)
	return fallbackEndpoint
}

// ---------------------------------------------------------------------------
// Signaling poll loop
// ---------------------------------------------------------------------------

// signalingLoop runs in a background goroutine, polling the control plane
// for remote signaling every 5 seconds (with jitter to prevent thundering
// herd in multi-device deployments), with a full peer sync + route refresh
// every 20 seconds (4th iteration).
func (m *OverlayManager) signalingLoop() {
	defer m.wg.Done()
	iteration := 0

	// M4: Add random jitter (0-2s) to prevent thundering herd
	jitterBytes := make([]byte, 2)
	_, _ = rand.Read(jitterBytes)
	jitter := time.Duration(int(jitterBytes[0])*8+int(jitterBytes[1])) * time.Millisecond // 0-2s
	time.Sleep(jitter)

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
			// Auto-ping all peers every ~30 seconds (iteration 6 = 30s)
			if iteration%6 == 0 {
				m.autoPingAllPeers()
			}
		}
	}
}

// pollRemoteSignaling fetches peers with their signaling candidates from
// the control plane and injects SDPs and ICE candidates into the P2P
// manager. Lock scope minimized: network I/O and P2P injection happen
// outside the lock (H1).
func (m *OverlayManager) pollRemoteSignaling() {
	// Fetch peers outside the lock (network I/O)
	peers, err := m.fetchPeers()
	if err != nil {
		m.logf("[Overlay-Go] Signaling poll failed: %v", err)
		return
	}

	// Collect signaling work items under lock, execute outside
	type sdpWork struct {
		peerID  string
		sigKey  string
		sdpType string
		payload string
	}
	type iceWork struct {
		peerID  string
		sigKey  string
		payload string
	}

	var sdpItems []sdpWork
	var iceItems []iceWork
	// Provisional ufrags from SDPs queued in this poll, keyed by peer ID.
	// Lets ICE candidates that arrive in the same /overlay/peers response
	// be injected immediately after the SDP, instead of waiting ~5s for the
	// next poll.
	pendingUfrags := make(map[string]string)

	m.mu.Lock()

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

		// Process SDPs first. Direct P2P is now explicitly initiated by
		// ForceP2POffer, so the old deterministic "lower device ID offers"
		// rule is not reliable: either peer may be the offerer.
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

			weOffered := m.offeredPeers[peer.ID]

			// Answers are only meaningful when this device actually has a
			// pending local offer. Otherwise they are stale remote signaling.
			if sdpType == "answer" && !weOffered {
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

			weOffered := m.offeredPeers[peer.ID]

			// Answerer in failed state receiving a new offer — recover
			if currentState == peerStateFailed && !weOffered && latestSDP.sdpType == "offer" {
				m.logf("[Overlay-Go] Received new offer from peer %s, clearing failed state", truncateID(peer.ID))
				delete(m.peerSessionIDs, peer.ID)
				delete(m.peerUfrags, peer.ID)
				delete(m.failedPeers, peer.ID)
				currentState = peerStateIdle
			}

			shouldInject := false
			if currentState == peerStateConnected {
				m.logf("[Overlay-Go] Skipping new %s from %s, already connected", latestSDP.sdpType, truncateID(peer.ID))
			} else if currentState == peerStateSDPReceived && weOffered && latestSDP.sdpType == "answer" {
				shouldInject = true
			} else if currentState == peerStateSDPReceived && weOffered && latestSDP.sdpType == "offer" {
				m.logf("[Overlay-Go] Skipping glare offer from %s while waiting for answer", truncateID(peer.ID))
				m.markSeen(peer.ID, latestSDP.sigKey)
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
				shouldInject = true
			}

			if shouldInject {
				sdpItems = append(sdpItems, sdpWork{
					peerID:  peer.ID,
					sigKey:  latestSDP.sigKey,
					sdpType: latestSDP.sdpType,
					payload: latestSDP.candidate.Type,
				})
				// Record the incoming SDP's ufrag so ICE candidates arriving
				// in the same poll can be injected without waiting for the
				// next cycle. Actual m.peerUfrags is updated after successful
				// injection in the post-unlock loop.
				if u := extractUfrag(latestSDP.candidate.Type); u != "" {
					pendingUfrags[peer.ID] = u
				}
			}
		}

		// Collect ICE candidates for injection
		currentState := m.peerStates[peer.ID]
		pendingUfrag, hasPending := pendingUfrags[peer.ID]
		if currentState != peerStateSDPReceived && currentState != peerStateConnected && !hasPending {
			continue
		}
		// Prefer the ufrag from the SDP queued this poll so ICE from a
		// retry/restart is matched against the fresh session instead of
		// the stale cached ufrag.
		var currentUfrag string
		switch {
		case hasPending:
			currentUfrag = pendingUfrag
		default:
			var hasUfrag bool
			currentUfrag, hasUfrag = m.peerUfrags[peer.ID]
			if !hasUfrag {
				continue
			}
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

			iceItems = append(iceItems, iceWork{
				peerID:  peer.ID,
				sigKey:  sigKey,
				payload: candidate.Type,
			})
		}
	}
	m.mu.Unlock()

	// H1: Execute P2P injections outside the lock (AddP2PPeer, json.Marshal)
	for _, item := range sdpItems {
		preview := truncateStr(item.payload, 120)
		m.logf("[Overlay-Go] Injecting remote sdp (%s) from peer %s, len=%d, preview: %s",
			item.sdpType, item.peerID, len(item.payload), preview)

		msg := P2PSignalingMessage{Type: "sdp", Payload: item.payload, SDPType: item.sdpType}
		sigJSON, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		injErr := AddP2PPeer(item.peerID, string(sigJSON))

		m.mu.Lock()
		if injErr != nil {
			m.logf("[Overlay-Go] Failed to inject SDP: %v", injErr)
			m.peerStates[item.peerID] = peerStateFailed
			m.failedPeers[item.peerID] = true
		} else {
			m.markSeen(item.peerID, item.sigKey)
			if m.peerStates[item.peerID] == peerStateIdle {
				m.peerStates[item.peerID] = peerStateSDPReceived
			}
			m.peerUfrags[item.peerID] = extractUfrag(item.payload)
			if sid := extractSessionID(item.payload); sid != "" {
				m.peerSessionIDs[item.peerID] = sid
			}
		}
		m.mu.Unlock()
	}

	for _, item := range iceItems {
		preview := truncateStr(item.payload, 120)
		m.logf("[Overlay-Go] Injecting remote candidate (ice) from peer %s, len=%d, preview: %s",
			item.peerID, len(item.payload), preview)

		msg := P2PSignalingMessage{Type: "candidate", Payload: item.payload}
		sigJSON, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		// Only mark as seen on successful injection so the candidate can
		// be retried on the next poll if injection failed (e.g. SDP not
		// yet applied or peer still warming up).
		if injErr := AddP2PPeer(item.peerID, string(sigJSON)); injErr != nil {
			m.logf("[Overlay-Go] Failed to inject ICE candidate: %v", injErr)
			continue
		}

		m.mu.Lock()
		m.markSeen(item.peerID, item.sigKey)
		m.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// P2P offer initiation
// ---------------------------------------------------------------------------

// initiateP2POffers intentionally does not start WebRTC. Relay is the
// default transport; direct P2P is opt-in via ForceP2POffer.
func (m *OverlayManager) initiateP2POffers() {
	m.logf("[Overlay-Go] Automatic P2P offers disabled; using relay until ForceP2POffer is requested")
}

// ForceP2POffer resets all P2P failure state for a peer and initiates
// a fresh WebRTC offer. Used when the user explicitly requests a
// direct connection upgrade from relay.
func (m *OverlayManager) ForceP2POffer(peerID string) error {
	if !m.running.Load() {
		return fmt.Errorf("overlay not running")
	}

	m.mu.Lock()
	peerExists := false
	for _, p := range m.peers {
		if p.ID == peerID {
			peerExists = true
			break
		}
	}
	if !peerExists {
		m.mu.Unlock()
		return fmt.Errorf("peer %s not found", truncateID(peerID))
	}

	delete(m.peerFailCount, peerID)
	delete(m.peerCooldownUntil, peerID)
	delete(m.peerSessionIDs, peerID)
	delete(m.peerUfrags, peerID)
	delete(m.failedPeers, peerID)
	delete(m.offeredPeers, peerID)
	m.peerStates[peerID] = peerStateIdle
	m.mu.Unlock()

	m.logf("[Overlay-Go] ForceP2POffer: resetting state for peer %s", truncateID(peerID))

	if err := StartP2POffer(peerID); err != nil {
		m.logf("[Overlay-Go] ForceP2POffer failed for %s: %v", truncateID(peerID), err)
		return err
	}

	m.mu.Lock()
	m.offeredPeers[peerID] = true
	m.peerStates[peerID] = peerStateSDPReceived
	m.mu.Unlock()

	m.logf("[Overlay-Go] ForceP2POffer: P2P offer sent for peer %s", truncateID(peerID))

	// Schedule a relay-only fallback attempt if the initial P2P offer fails.
	// After 15 seconds, if the peer is still not connected, retry with
	// ICETransportPolicy=relay to bypass firewall/NAT issues via TURN.
	go m.scheduleRelayFallback(peerID)

	return nil
}

// scheduleRelayFallback waits for the initial P2P attempt to either succeed
// or fail, then retries with relay-only ICE transport policy if needed.
func (m *OverlayManager) scheduleRelayFallback(peerID string) {
	// Wait 15 seconds for the initial attempt to complete
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-m.cancelCh:
		return
	}

	if !m.running.Load() {
		return
	}

	m.mu.RLock()
	state := m.peerStates[peerID]
	m.mu.RUnlock()

	// If already connected, no fallback needed
	if state == peerStateConnected {
		return
	}

	// Check if DataChannel is already open (connection may have succeeded
	// but state tracking lagged)
	if globalOverlayTransport.HasPacketConn(peerID) {
		return
	}

	m.logf("[Overlay-Go] P2P direct attempt timed out for %s (state=%s), retrying with TURN relay-only",
		truncateID(peerID), state)

	// Reset state for retry
	m.mu.Lock()
	delete(m.peerSessionIDs, peerID)
	delete(m.peerUfrags, peerID)
	delete(m.failedPeers, peerID)
	delete(m.offeredPeers, peerID)
	m.peerStates[peerID] = peerStateIdle
	m.mu.Unlock()

	if err := StartP2POfferRelayOnly(peerID); err != nil {
		m.logf("[Overlay-Go] ForceP2POffer relay-only fallback failed for %s: %v", truncateID(peerID), err)
		return
	}

	m.mu.Lock()
	m.offeredPeers[peerID] = true
	m.peerStates[peerID] = peerStateSDPReceived
	m.mu.Unlock()

	m.logf("[Overlay-Go] ForceP2POffer: relay-only P2P offer sent for peer %s (TURN fallback)", truncateID(peerID))
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

	// Keep relay as the default transport. Direct P2P is attempted only when
	// the app explicitly calls ForceP2POffer for a selected peer.
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
			delete(m.offeredPeers, peerID)
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
			m.logf("[Overlay-Go] P2P disconnected for %s, resetting state to allow re-negotiation", truncateID(peerID))
			m.peerStates[peerID] = peerStateFailed
			m.failedPeers[peerID] = true
			delete(m.offeredPeers, peerID)
		case "failed":
			m.peerStates[peerID] = peerStateFailed
			m.failedPeers[peerID] = true
			delete(m.offeredPeers, peerID)
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
				delete(m.offeredPeers, peerID)
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

// configureICEServers sets up STUN/TURN servers for WebRTC NAT discovery.
// Uses user-configured servers from iceServers config field if present,
// otherwise falls back to hardcoded defaults.
func (m *OverlayManager) configureICEServers() {
	// Use user-configured ICE servers if provided
	if custom := m.config.ICEServers; custom != "" {
		if err := SetICEServersJSON(custom); err != nil {
			m.logf("[Overlay-Go] Failed to configure custom ICE servers: %v, falling back to defaults", err)
		} else {
			m.logf("[Overlay-Go] Custom ICE servers configured from user settings")
			return
		}
	}

	// Default fallback: public STUN only. TURN credentials must come from
	// the app-provided ICE server config so they can be rotated server-side.
	iceConfig := `[` +
		`{"urls":["stun:stun.l.google.com:19302"]},` +
		`{"urls":["stun:stun1.l.google.com:19302"]},` +
		`{"urls":["stun:stun2.l.google.com:19302"]},` +
		`{"urls":["stun:stun.qq.com:3478"]},` +
		`{"urls":["stun:stun.aliyun.com:3478"]},` +
		`{"urls":["stun:stun.miwifi.com:3478"]},` +
		`{"urls":["stun:stun.syncthing.net:3478"]}` +
		`]`

	if err := SetICEServersJSON(iceConfig); err != nil {
		m.logf("[Overlay-Go] Failed to configure ICE servers: %v", err)
	} else {
		m.logf("[Overlay-Go] ICE servers configured (7 STUN)")
	}
}

// ---------------------------------------------------------------------------
// Ping / Pong
// ---------------------------------------------------------------------------

// handleControlPacket processes overlay control messages (ping/pong).
func (m *OverlayManager) handleControlPacket(peerID string, data []byte) {
	if len(data) < 2 {
		return
	}
	switch data[1] {
	case overlayPingType:
		// Received a ping — reply with pong using the same nonce
		if len(data) < overlayPingSize {
			return
		}
		pong := make([]byte, overlayPingSize)
		copy(pong, data)
		pong[1] = overlayPongType
		m.sendControlPacket(peerID, pong)

	case overlayPongType:
		// Received a pong — match to pending ping and record RTT
		if len(data) < overlayPingSize {
			return
		}
		nonceHex := fmt.Sprintf("%x", data[2:overlayPingSize])
		m.pingMu.Lock()
		rec, ok := m.pendingPings[nonceHex]
		if ok {
			delete(m.pendingPings, nonceHex)
		}
		m.pingMu.Unlock()

		if ok && rec.peerID == peerID {
			rtt := time.Since(rec.sentAt).Milliseconds()
			m.mu.Lock()
			m.peerLastPingMs[peerID] = rtt
			m.mu.Unlock()
			if rec.result != nil {
				select {
				case rec.result <- rtt:
				default:
				}
			}
			m.logf("[Overlay-Ping] Pong from %s: %dms", truncateID(peerID), rtt)
		}
	}
}

// sendControlPacket encrypts and sends a control packet to a peer.
func (m *OverlayManager) sendControlPacket(peerID string, payload []byte) {
	m.mu.RLock()
	peerCipher, ok := m.peerCiphers[peerID]
	m.mu.RUnlock()
	if !ok {
		return
	}

	encrypted, err := m.encryptPacket(payload, peerCipher)
	if err != nil {
		return
	}
	_ = globalOverlayTransport.Send(peerID, encrypted)
}

// autoPingAllPeers sends a non-blocking ping to every peer that has an
// encryption key. Results are stored in peerLastPingMs for the snapshot.
func (m *OverlayManager) autoPingAllPeers() {
	m.mu.RLock()
	peerIDs := make([]string, 0, len(m.peers))
	for _, p := range m.peers {
		if _, ok := m.peerCiphers[p.ID]; ok {
			peerIDs = append(peerIDs, p.ID)
		}
	}
	m.mu.RUnlock()

	if len(peerIDs) == 0 {
		return
	}

	go func(peerIDs []string) {
		pending := make(map[string]string, len(peerIDs))

		for _, pid := range peerIDs {
			ping := make([]byte, overlayPingSize)
			ping[0] = overlayControlPrefix
			ping[1] = overlayPingType
			if _, err := rand.Read(ping[2:]); err != nil {
				continue
			}
			nonceHex := fmt.Sprintf("%x", ping[2:overlayPingSize])
			now := time.Now()

			m.pingMu.Lock()
			m.pendingPings[nonceHex] = pingRecord{peerID: pid, sentAt: now}
			m.pingMu.Unlock()

			m.sendControlPacket(pid, ping)
			pending[nonceHex] = pid
		}

		if len(pending) == 0 {
			return
		}

		deadline := time.NewTimer(3 * time.Second)
		tick := time.NewTicker(50 * time.Millisecond)
		defer deadline.Stop()
		defer tick.Stop()

		for len(pending) > 0 {
			select {
			case <-deadline.C:
				m.pingMu.Lock()
				for nonceHex, pid := range pending {
					delete(m.pendingPings, nonceHex)
					m.mu.Lock()
					m.peerLastPingMs[pid] = -1
					m.mu.Unlock()
				}
				m.pingMu.Unlock()
				return
			case <-tick.C:
				m.pingMu.Lock()
				for nonceHex := range pending {
					if _, ok := m.pendingPings[nonceHex]; !ok {
						delete(pending, nonceHex)
					}
				}
				m.pingMu.Unlock()
			}
		}
	}(peerIDs)
}

// PingPeer sends a ping probe to the specified peer and returns the result
// as a JSON string. It blocks for up to 5 seconds waiting for the pong.
// Result: {"peerID":"...","latencyMs":42,"error":""} or {"error":"..."}
func (m *OverlayManager) PingPeer(peerID string) string {
	type pingResult struct {
		PeerID    string `json:"peerID"`
		LatencyMs int64  `json:"latencyMs"`
		Error     string `json:"error,omitempty"`
	}

	marshal := func(r pingResult) string {
		data, _ := json.Marshal(r)
		return string(data)
	}

	if !m.running.Load() {
		return marshal(pingResult{PeerID: peerID, LatencyMs: -1, Error: "overlay not running"})
	}

	m.mu.RLock()
	_, hasCipher := m.peerCiphers[peerID]
	m.mu.RUnlock()
	if !hasCipher {
		return marshal(pingResult{PeerID: peerID, LatencyMs: -1, Error: "no encryption key for peer"})
	}

	// Build ping packet: [0x00, 0x01, 8-byte random nonce]
	ping := make([]byte, overlayPingSize)
	ping[0] = overlayControlPrefix
	ping[1] = overlayPingType
	if _, err := rand.Read(ping[2:]); err != nil {
		return marshal(pingResult{PeerID: peerID, LatencyMs: -1, Error: "failed to generate nonce"})
	}

	nonceHex := fmt.Sprintf("%x", ping[2:overlayPingSize])
	now := time.Now()
	// Buffered so handleControlPacket delivers the RTT without blocking,
	// even if we hit the timeout branch below first.
	resultCh := make(chan int64, 1)

	m.pingMu.Lock()
	m.pendingPings[nonceHex] = pingRecord{peerID: peerID, sentAt: now, result: resultCh}
	m.pingMu.Unlock()

	// Send the encrypted ping
	m.sendControlPacket(peerID, ping)

	select {
	case rtt := <-resultCh:
		return marshal(pingResult{PeerID: peerID, LatencyMs: rtt})
	case <-time.After(5 * time.Second):
		m.pingMu.Lock()
		delete(m.pendingPings, nonceHex)
		m.pingMu.Unlock()
		return marshal(pingResult{PeerID: peerID, LatencyMs: -1, Error: "timeout"})
	}
}

// PingAllPeers pings all peers with encryption keys concurrently and returns
// a JSON array of results. Blocks for up to 5 seconds.
func (m *OverlayManager) PingAllPeers() string {
	type pingResult struct {
		PeerID    string `json:"peerID"`
		LatencyMs int64  `json:"latencyMs"`
		Error     string `json:"error,omitempty"`
	}

	m.mu.RLock()
	peerIDs := make([]string, 0, len(m.peers))
	for _, p := range m.peers {
		if _, ok := m.peerCiphers[p.ID]; ok {
			peerIDs = append(peerIDs, p.ID)
		}
	}
	m.mu.RUnlock()

	if len(peerIDs) == 0 {
		return "[]"
	}

	results := make([]pingResult, len(peerIDs))
	var wg sync.WaitGroup
	for i, pid := range peerIDs {
		i, pid := i, pid
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw := m.PingPeer(pid)
			var r pingResult
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				results[i] = pingResult{PeerID: pid, LatencyMs: -1, Error: "parse error"}
			} else {
				results[i] = r
			}
		}()
	}
	wg.Wait()

	data, _ := json.Marshal(results)
	return string(data)
}

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

// normalizeRelayEndpoint converts a relay endpoint into the bare "host:port"
// form expected by net.ResolveUDPAddr. It tolerates values that carry a
// scheme prefix (e.g. "udp://host:8091", "wss://host:8091") as well as a
// trailing path/query, both of which would otherwise make ResolveUDPAddr
// fail. Bare "host:port" (and IPv6 "[::1]:port") values are returned as-is.
func normalizeRelayEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	// Strip an optional "scheme://" prefix.
	if idx := strings.Index(raw, "://"); idx >= 0 {
		raw = raw[idx+3:]
	}
	// Strip any trailing path or query (e.g. "host:8091/foo?bar").
	if idx := strings.IndexAny(raw, "/?"); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(raw)
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

package mate

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pion/webrtc/v4"
)

// realOffer returns a genuine pion offer SDP so AddP2PPeer can apply it.
func realOffer(t *testing.T) string {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.CreateDataChannel("mate-overlay", nil); err != nil {
		t.Fatalf("dc: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	return offer.SDP
}

// stubPeersServer serves GET /api/v1/overlay/peers with one peer carrying an
// offer SDP (port 0 = offer in the overlay signaling encoding).
func stubPeersServer(t *testing.T, peerID, sdp string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := json.Marshal(overlayPeersResponse{Items: []overlayPeer{{
			ID:         peerID,
			Candidates: []overlayCandidate{{IP: "sdp", Port: 0, Type: sdp}},
		}}})
		_ = json.NewEncoder(w).Encode(overlayEnvelope{Success: true, Data: data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func glareManager(t *testing.T, localID, peerID string) *OverlayManager {
	t.Helper()
	m := newTestOverlayManager(t)
	srv := stubPeersServer(t, peerID, realOffer(t))
	m.config = &OverlayConfig{ServerURL: srv.URL, AccessToken: "t"}
	m.httpClient = srv.Client()
	m.deviceID = localID
	// We already sent our own offer and are waiting for an answer.
	m.peerStates[peerID] = peerStateSDPReceived
	m.offeredPeers[peerID] = true
	m.p2pAttemptGen[peerID] = 1
	t.Cleanup(func() { RemoveP2PPeer(peerID) })
	return m
}

// Both devices offered at once: the larger device ID must yield and answer,
// so exactly one negotiation survives instead of both dying.
func TestGlarePoliteSideYieldsAndAnswers(t *testing.T) {
	const peer = "aaaa-remote"
	m := glareManager(t, "zzzz-local", peer)

	m.pollRemoteSignaling()

	if m.offeredPeers[peer] {
		t.Fatalf("polite side kept its own offer; glare unresolved")
	}
	if got := m.peerStates[peer]; got != peerStateSDPReceived {
		t.Fatalf("state = %v, want sdpReceived (answering remote offer)", got)
	}
	if m.p2pAttemptGen[peer] < 2 {
		t.Fatalf("own offer's timers were not invalidated (gen=%d)", m.p2pAttemptGen[peer])
	}
	if m.peerUfrags[peer] == "" {
		t.Fatalf("remote offer was not applied (no ufrag recorded)")
	}
}

func TestGlareImpoliteSideKeepsItsOffer(t *testing.T) {
	const peer = "zzzz-remote"
	m := glareManager(t, "aaaa-local", peer)

	m.pollRemoteSignaling()

	if !m.offeredPeers[peer] {
		t.Fatalf("impolite side dropped its own offer; both sides would yield")
	}
	if m.p2pAttemptGen[peer] != 1 {
		t.Fatalf("impolite side's offer timers were disturbed (gen=%d)", m.p2pAttemptGen[peer])
	}
}

// One direct connection per peer: a second request while one is pending must
// not start another negotiation.
func TestForceP2POfferIgnoredWhileNegotiationInProgress(t *testing.T) {
	m := newTestOverlayManager(t)
	const peer = "peer-busy"
	m.peers = []overlayPeer{{ID: peer}}
	m.peerStates[peer] = peerStateSDPReceived
	m.p2pAttemptGen[peer] = 7

	status, err := m.ForceP2POffer(peer)
	if err != nil {
		t.Fatalf("ForceP2POffer: %v", err)
	}
	if status != forceP2PInProgress {
		t.Fatalf("status = %q, want %q", status, forceP2PInProgress)
	}
	if m.p2pAttemptGen[peer] != 7 {
		t.Fatalf("a second negotiation was started (gen=%d)", m.p2pAttemptGen[peer])
	}
}

func TestForceP2POfferReportsAlreadyDirect(t *testing.T) {
	m := newTestOverlayManager(t)
	const peer = "peer-direct"
	m.peers = []overlayPeer{{ID: peer}}
	m.peerStates[peer] = peerStateConnected
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	globalOverlayTransport.AttachPeerPacketConn(peer, conn)

	status, err := m.ForceP2POffer(peer)
	if err != nil || status != forceP2PAlreadyDirect {
		t.Fatalf("got (%q, %v), want (%q, nil)", status, err, forceP2PAlreadyDirect)
	}
}

// The exported gomobile wrapper must emit valid JSON with the status field,
// even when the error text contains quotes.
func TestForceP2POfferJSON(t *testing.T) {
	prev := globalOverlayManager
	t.Cleanup(func() { globalOverlayManager = prev })

	m := newTestOverlayManager(t)
	const peer = "peer-json"
	m.peers = []overlayPeer{{ID: peer}}
	m.peerStates[peer] = peerStateSDPReceived
	globalOverlayManager = m

	var got struct{ PeerID, Status, Error string }
	if err := json.Unmarshal([]byte(ForceP2POffer(peer)), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.PeerID != peer || got.Status != forceP2PInProgress || got.Error != "" {
		t.Fatalf("got %+v", got)
	}

	if err := json.Unmarshal([]byte(ForceP2POffer(`no"such"peer`)), &got); err != nil {
		t.Fatalf("invalid JSON for quoted error: %v", err)
	}
	if got.Status != "" || got.Error == "" {
		t.Fatalf("got %+v, want empty status and an error", got)
	}
}

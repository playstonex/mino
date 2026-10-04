package mate

import (
	"net"
	"testing"
	"time"
)

func newTestOverlayManager(t *testing.T) *OverlayManager {
	t.Helper()
	m := NewOverlayManager()
	m.cancelCh = make(chan struct{})
	m.running.Store(true)
	t.Cleanup(func() {
		m.running.Store(false)
		close(m.cancelCh)
		globalOverlayTransport.Reset()
	})
	return m
}

func shrinkP2PTimeouts(t *testing.T) {
	t.Helper()
	d, g, r := p2pDirectAttemptTimeout, p2pDataChannelGrace, p2pRelayOnlyAttemptTimeout
	p2pDirectAttemptTimeout = 20 * time.Millisecond
	p2pDataChannelGrace = 300 * time.Millisecond
	p2pRelayOnlyAttemptTimeout = 20 * time.Millisecond
	t.Cleanup(func() { p2pDirectAttemptTimeout, p2pDataChannelGrace, p2pRelayOnlyAttemptTimeout = d, g, r })
}

// A relay-only attempt whose answer never arrives must not leave the peer in
// sdpReceived ("connecting") forever.
func TestRelayOnlyAttemptIsAbandonedWhenNoDataChannel(t *testing.T) {
	shrinkP2PTimeouts(t)
	m := newTestOverlayManager(t)
	const peer = "peer-abandon"
	m.peerStates[peer] = peerStateSDPReceived
	m.offeredPeers[peer] = true
	m.p2pAttemptGen[peer] = 1

	m.abandonP2PIfNoDataChannel(peer, 1, p2pRelayOnlyAttemptTimeout)

	if got := m.peerStates[peer]; got != peerStateFailed {
		t.Fatalf("state = %v, want failed", got)
	}
	if m.offeredPeers[peer] {
		t.Fatalf("offeredPeers still set; a later remote offer would be treated as glare")
	}
}

// A newer ForceP2POffer supersedes an older timeout goroutine.
func TestRelayOnlyAbandonSkipsSupersededAttempt(t *testing.T) {
	shrinkP2PTimeouts(t)
	m := newTestOverlayManager(t)
	const peer = "peer-superseded"
	m.peerStates[peer] = peerStateSDPReceived
	m.offeredPeers[peer] = true
	m.p2pAttemptGen[peer] = 2

	m.abandonP2PIfNoDataChannel(peer, 1, p2pRelayOnlyAttemptTimeout)

	if got := m.peerStates[peer]; got != peerStateSDPReceived {
		t.Fatalf("state = %v, want sdpReceived (newer attempt must be left alone)", got)
	}
}

// A PeerConnection that is "connected" with its DataChannel opening just
// after the direct timeout must NOT be torn down for a relay-only retry.
func TestDirectTimeoutGivesConnectedPeerDataChannelGrace(t *testing.T) {
	shrinkP2PTimeouts(t)
	m := newTestOverlayManager(t)
	const peer = "peer-grace"
	m.peerStates[peer] = peerStateConnected
	m.p2pAttemptGen[peer] = 1

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		time.Sleep(p2pDirectAttemptTimeout + 100*time.Millisecond)
		globalOverlayTransport.AttachPeerPacketConn(peer, conn)
	}()

	m.scheduleRelayFallback(peer, 1)

	if got := m.peerStates[peer]; got != peerStateConnected {
		t.Fatalf("state = %v, want connected (DataChannel opened within grace)", got)
	}
	if m.p2pAttemptGen[peer] != 1 {
		t.Fatalf("relay-only retry was started despite DataChannel opening in grace window")
	}
}

package mate

import (
	"net"
	"strings"
	"testing"
	"time"
)

// slowAckRelay is a loopback UDP relay that answers the first registration
// with an ack only after `delay`, then counts the data packets it receives.
func slowAckRelay(t *testing.T, delay time.Duration) (addr string, dataPkts <-chan struct{}) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	data := make(chan struct{}, 16)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 34 {
				continue
			}
			switch buf[1] {
			case 0x10: // register
				ack := make([]byte, 34)
				ack[0], ack[1] = buf[0], 0x11
				time.Sleep(delay)
				_, _ = conn.WriteToUDP(ack, from)
			case 0x01: // data
				select {
				case data <- struct{}{}:
				default:
				}
			}
		}
	}()
	return conn.LocalAddr().String(), data
}

// TestEnsureRelayIncludesPeerRegisteredDuringDial is the regression guard for
// the relay-path blackhole seen on device (log 2026-10-04): Configure() starts
// the relay dial in the background, RegisterPeer() runs while it is in flight
// (relayClient still nil), and the finished client was stored without that
// peer. Every Send then failed with "relay: unknown peer" forever, so SSH over
// relay never got past the TCP SYN.
func TestEnsureRelayIncludesPeerRegisteredDuringDial(t *testing.T) {
	const peer = "86f5222e-06d7-478f-ab36-a28b8cbd552f"
	addr, data := slowAckRelay(t, 500*time.Millisecond)

	m := newOverlayTransportManager()
	m.setRelayConfig(addr, "test-token", testDeviceUUID)

	done := make(chan error, 1)
	go func() {
		_, err := m.ensureRelay()
		done <- err
	}()

	// Land inside the dial window, exactly as wireP2PCallbacks does on device.
	time.Sleep(150 * time.Millisecond)
	if err := m.RegisterPeer(peer); err != nil {
		t.Fatalf("RegisterPeer: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ensureRelay: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ensureRelay did not return")
	}
	t.Cleanup(func() { m.Reset() })

	if err := m.Send(peer, []byte("ssh-syn")); err != nil {
		if strings.Contains(err.Error(), "unknown peer") {
			t.Fatalf("peer registered during the dial is missing from the relay client: %v", err)
		}
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-data:
	case <-time.After(2 * time.Second):
		t.Fatal("relay never received the data packet")
	}
}

package mate

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestRewriteHybridOverlaySourceUpdatesIPv4AndTCPChecksums(t *testing.T) {
	oldSource := [4]byte{198, 18, 0, 1}
	newSource := [4]byte{100, 96, 0, 10}
	destination := [4]byte{100, 96, 0, 11}
	packet := buildIPv4TCPPacket(oldSource, destination, []byte("SSH-2.0-test"))

	originalTCPChecksum := binary.BigEndian.Uint16(packet[36:38])
	rewritten := rewriteHybridOverlaySource(packet, "100.96.0.10")

	if got := [4]byte{rewritten[12], rewritten[13], rewritten[14], rewritten[15]}; got != newSource {
		t.Fatalf("source = %v, want %v", got, newSource)
	}
	if got := internetChecksum(rewritten[:20]); got != 0 {
		t.Fatalf("IPv4 checksum invalid: got %#04x", got)
	}
	if got := binary.BigEndian.Uint16(rewritten[36:38]); got == originalTCPChecksum {
		t.Fatalf("TCP checksum was not updated: %#04x", got)
	}
	if got := ipv4TransportChecksum(6, rewritten[12:16], rewritten[16:20], rewritten[20:]); got != 0 {
		t.Fatalf("TCP checksum invalid after source rewrite: got %#04x", got)
	}
}

func TestRewriteHybridOverlaySourceKeepsExistingOverlaySource(t *testing.T) {
	source := [4]byte{100, 96, 0, 10}
	destination := [4]byte{100, 96, 0, 11}
	packet := buildIPv4TCPPacket(source, destination, nil)
	before := append([]byte(nil), packet...)

	rewritten := rewriteHybridOverlaySource(packet, "100.96.0.10")

	if !bytes.Equal(rewritten, before) {
		t.Fatal("packet changed even though source was already the local overlay IP")
	}
}

func buildIPv4TCPPacket(source [4]byte, destination [4]byte, payload []byte) []byte {
	const (
		ipHeaderLen  = 20
		tcpHeaderLen = 20
	)

	totalLen := ipHeaderLen + tcpHeaderLen + len(payload)
	packet := make([]byte, totalLen)

	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(totalLen))
	binary.BigEndian.PutUint16(packet[4:6], 0x1234)
	packet[8] = 64
	packet[9] = 6
	copy(packet[12:16], source[:])
	copy(packet[16:20], destination[:])

	tcp := packet[ipHeaderLen:]
	binary.BigEndian.PutUint16(tcp[0:2], 54321)
	binary.BigEndian.PutUint16(tcp[2:4], 22)
	binary.BigEndian.PutUint32(tcp[4:8], 1)
	tcp[12] = 5 << 4
	tcp[13] = 0x18
	binary.BigEndian.PutUint16(tcp[14:16], 4096)
	copy(tcp[tcpHeaderLen:], payload)

	binary.BigEndian.PutUint16(tcp[16:18], ipv4TransportChecksum(6, source[:], destination[:], tcp))
	binary.BigEndian.PutUint16(packet[10:12], internetChecksum(packet[:ipHeaderLen]))

	return packet
}

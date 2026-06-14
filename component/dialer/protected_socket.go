package dialer

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// GetPlatformProtectedSocket is set by the mate package to provide access to
// pre-protected sockets from the Kotlin SocketPool.
var GetPlatformProtectedSocket func() int32

// dialWithProtectedSocket creates a TCP connection using a pre-protected socket fd
// from the Kotlin SocketPool. It bypasses Go's net.Dialer entirely to avoid
// Android 16's kernel restriction on socket(AF_INET/AF_INET6) after establish().
func dialWithProtectedSocket(ctx context.Context, network, address string) (net.Conn, error) {
	if GetPlatformProtectedSocket == nil {
		return nil, fmt.Errorf("GetPlatformProtectedSocket not set")
	}

	fd32 := GetPlatformProtectedSocket()
	if fd32 < 0 {
		return nil, fmt.Errorf("no protected sockets available in pool")
	}
	fd := int(fd32)

	fmt.Fprintf(os.Stderr, "[dialer] dialWithProtectedSocket: got fd=%d for %s %s\n", fd, network, address)

	// Parse destination
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bad address: %w", err)
	}

	// Resolve destination IP
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("dns resolve failed: %w", err)
	}
	if len(ips) == 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("no IPs found for %s", host)
	}
	dstIP := ips[0].IP

	portNum, err := net.LookupPort("tcp", port)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bad port: %w", err)
	}

	// Build sockaddr
	var rsa syscall.Sockaddr
	if dstIP4 := dstIP.To4(); dstIP4 != nil {
		sa := &syscall.SockaddrInet4{Port: portNum}
		copy(sa.Addr[:], dstIP4)
		rsa = sa
	} else {
		sa := &syscall.SockaddrInet6{Port: portNum}
		copy(sa.Addr[:], dstIP.To16())
		rsa = sa
	}

	// Do a blocking connect with a timeout using a goroutine.
	// The fd is blocking mode (default from socket creation).
	errCh := make(chan error, 1)
	go func() {
		errCh <- syscall.Connect(fd, rsa)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			syscall.Close(fd)
			return nil, fmt.Errorf("connect error: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[dialer] dialWithProtectedSocket: connected\n")
	case <-time.After(DefaultTCPTimeout):
		syscall.Close(fd)
		return nil, fmt.Errorf("connect timeout after %v", DefaultTCPTimeout)
	case <-ctx.Done():
		syscall.Close(fd)
		return nil, fmt.Errorf("connect cancelled: %w", ctx.Err())
	}

	// Wrap the connected fd as net.Conn using os.NewFile + net.FileConn.
	// FileConn will dup the fd and set up non-blocking I/O with the poller.
	osFile := os.NewFile(uintptr(fd), fmt.Sprintf("tcp:%s", address))
	if osFile == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("os.NewFile failed")
	}

	conn, err := net.FileConn(osFile)
	osFile.Close() // FileConn dups the fd, so close original
	if err != nil {
		return nil, fmt.Errorf("FileConn failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[dialer] dialWithProtectedSocket: success, conn type=%T\n", conn)
	return conn, nil
}

// listenPacketWithProtectedSocket creates a UDP PacketConn using a pre-protected
// socket fd from the Kotlin SocketPool.
func listenPacketWithProtectedSocket(ctx context.Context, network, address string) (net.PacketConn, error) {
	if GetPlatformProtectedSocket == nil {
		return nil, fmt.Errorf("GetPlatformProtectedSocket not set")
	}

	fd32 := GetPlatformProtectedSocket()
	if fd32 < 0 {
		return nil, fmt.Errorf("no protected sockets available in pool")
	}
	fd := int(fd32)

	fmt.Fprintf(os.Stderr, "[dialer] listenPacketWithProtectedSocket: got fd=%d for %s %s\n", fd, network, address)

	// Bind to local address if specified
	if address != "" && address != ":0" && address != "0.0.0.0:0" {
		udpAddr, err := net.ResolveUDPAddr(network, address)
		if err != nil {
			syscall.Close(fd)
			return nil, fmt.Errorf("resolve addr failed: %w", err)
		}
		var lsa syscall.Sockaddr
		if udpAddr.IP.To4() != nil {
			sa := &syscall.SockaddrInet4{Port: udpAddr.Port}
			copy(sa.Addr[:], udpAddr.IP.To4())
			lsa = sa
		} else {
			sa := &syscall.SockaddrInet6{Port: udpAddr.Port}
			copy(sa.Addr[:], udpAddr.IP.To16())
			lsa = sa
		}
		if err := syscall.Bind(fd, lsa); err != nil {
			syscall.Close(fd)
			return nil, fmt.Errorf("bind failed: %w", err)
		}
	}

	// Wrap as PacketConn
	osFile := os.NewFile(uintptr(fd), fmt.Sprintf("udp:%s", address))
	if osFile == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("os.NewFile failed")
	}

	conn, err := net.FilePacketConn(osFile)
	osFile.Close()
	if err != nil {
		return nil, fmt.Errorf("FilePacketConn failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[dialer] listenPacketWithProtectedSocket: success, conn type=%T\n", conn)
	return conn, nil
}

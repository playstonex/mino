package dialer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// SocketControl
// never change type traits because it's used in CMFA
type SocketControl func(network, address string, conn syscall.RawConn) error

// DefaultSocketHook
// never change type traits because it's used in CMFA
var DefaultSocketHook SocketControl

// isSocketBlocked returns true if the error indicates socket() creation
// was blocked by the OS (e.g. Android 16 seccomp filter in VpnService).
func isSocketBlocked(err error) bool {
	if err == nil {
		return false
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return se.Err == syscall.EPERM || se.Err == syscall.EACCES
	}
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

func socketHookToToDialer(dialer *net.Dialer) {
	fmt.Fprintf(os.Stderr, "[dialer] socketHookToToDialer called\n")
	addControlToDialer(dialer, func(ctx context.Context, network, address string, c syscall.RawConn) error {
		fmt.Fprintf(os.Stderr, "[dialer] socketHook control: network=%s address=%s\n", network, address)
		return DefaultSocketHook(network, address, c)
	})
}

func socketHookToListenConfig(lc *net.ListenConfig) {
	fmt.Fprintf(os.Stderr, "[dialer] socketHookToListenConfig called\n")
	addControlToListenConfig(lc, func(ctx context.Context, network, address string, c syscall.RawConn) error {
		fmt.Fprintf(os.Stderr, "[dialer] socketHook listenControl: network=%s address=%s\n", network, address)
		return DefaultSocketHook(network, address, c)
	})
}

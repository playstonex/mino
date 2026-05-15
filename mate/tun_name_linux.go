//go:build !darwin

package mate

import (
	"fmt"
	"syscall"
	"unsafe"
)

func getTunnelName(fd int32) (string, error) {
	// On Linux/Android, use TUNGETIFF ioctl to get the interface name
	// from the TUN file descriptor.
	const TUNGETIFF = 0x800454d2 // linux/if_tun.h
	var ifr [32]byte // struct ifreq: 16 bytes name + padding
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(TUNGETIFF),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		return fmt.Sprintf("tun%d", fd), nil
	}
	name := string(ifr[:16])
	for i := 0; i < len(name); i++ {
		if name[i] == 0 {
			name = name[:i]
			break
		}
	}
	return name, nil
}

package constant

import (
	"sync"

	tun "github.com/playstonex/sing-tun"
)

type TunListenOutterCreator interface {
	OpenTun(options *tun.Options) (tun.Tun, error)
}

var (
	creator             TunListenOutterCreator
	packetInterceptor   tun.PacketInterceptor
	packetInterceptorMu sync.RWMutex
)

func GetTunOutterCreator() TunListenOutterCreator {
	return creator
}

func SetOutterCreator(acreator TunListenOutterCreator) {
	creator = acreator
}

func GetTunPacketInterceptor() tun.PacketInterceptor {
	packetInterceptorMu.RLock()
	defer packetInterceptorMu.RUnlock()
	return packetInterceptor
}

func SetTunPacketInterceptor(interceptor tun.PacketInterceptor) {
	packetInterceptorMu.Lock()
	defer packetInterceptorMu.Unlock()
	packetInterceptor = interceptor
}

package constant

import (
	tun "github.com/metacubex/sing-tun"
)

type TunListenOutterCreator interface {
	OpenTun(options *tun.Options) (tun.Tun, error)
}

var creator TunListenOutterCreator

func GetTunOutterCreator() TunListenOutterCreator {
	return creator
}

func SetOutterCreator(acreator TunListenOutterCreator) {
	creator = acreator
}

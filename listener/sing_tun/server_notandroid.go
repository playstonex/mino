//go:build !android || cmfa

package sing_tun

import (
	tun "github.com/playstonex/sing-tun"
)

func (l *Listener) buildAndroidRules(tunOptions *tun.Options) error {
	return nil
}

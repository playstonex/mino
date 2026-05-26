package rpc

import (
	"strings"

	"github.com/WarrDoge/sslcon/auth"
	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/lib"
	"github.com/WarrDoge/sslcon/session"
	"github.com/WarrDoge/sslcon/utils/vpnc"
	"github.com/WarrDoge/sslcon/vpn"
)

func activateContext(ctx *lib.VPNContext) {
	if ctx == nil {
		return
	}
	if ctx.Cfg == nil {
		ctx.Cfg = base.NewClientConfig()
	}
	if ctx.LocalInterface == nil {
		ctx.LocalInterface = base.NewInterface()
	}
	if ctx.Profile == nil {
		ctx.Profile = lib.NewProfile()
	}
	if ctx.Session == nil {
		ctx.Session = &session.Session{}
	}
	if ctx.Auth == nil {
		ctx.Auth = lib.NewAuthState()
	}
	if ctx.Logger == nil {
		ctx.Logger = base.NewLogger(ctx.Cfg)
	}

	base.Cfg = ctx.Cfg
	base.LocalInterface = ctx.LocalInterface
	base.SetDefaultLogger(ctx.Logger)
	auth.Prof = ctx.Profile
	auth.State = ctx.Auth
	if ctx.TLSCert != nil || ctx.RootCAs != nil {
		if ctx.TLSCert != nil {
			auth.SetTLSCredentials(ctx, *ctx.TLSCert, ctx.RootCAs)
		}
	} else {
		auth.ClearTLSCredentials(ctx)
	}
	session.Sess = ctx.Session
}

// Connect requires the frontend to populate auth.Prof first; providing base.Interface is recommended.
func Connect(ctxs ...*lib.VPNContext) error {
	if len(ctxs) > 0 {
		activateContext(ctxs[0])
	}
	if strings.Contains(auth.Prof.Host, ":") {
		auth.Prof.HostWithPort = auth.Prof.Host
	} else {
		auth.Prof.HostWithPort = auth.Prof.Host + ":443"
	}
	if !auth.Prof.Initialized {
		err := vpnc.GetLocalInterface()
		if err != nil {
			return err
		}
	}
	err := auth.InitAuth()
	if err != nil {
		return err
	}
	err = auth.PasswordAuth()
	if err != nil {
		return err
	}

	return SetupTunnel(false)
}

// SetupTunnel is only meant for short reconnects; reconnecting after long OS sleep may fail.
func SetupTunnel(reconnect bool, ctxs ...*lib.VPNContext) error {
	if len(ctxs) > 0 {
		activateContext(ctxs[0])
	}
	// Complex networks require NIC change awareness; it is better for the frontend to push current network info
	// instead of relying only on Go-side detection before login.
	// Interface details may change before reconnect, so refresh them when rebuilding the tunnel.
	if reconnect && !auth.Prof.Initialized {
		err := vpnc.GetLocalInterface()
		if err != nil {
			return err
		}
	}
	return vpn.SetupTunnel()
}

// DisConnect handles intentional disconnects such as user actions or Ctrl+C, not network or TUN failures.
func DisConnect(ctxs ...*lib.VPNContext) {
	if len(ctxs) > 0 {
		activateContext(ctxs[0])
	}
	session.Sess.ActiveClose = true
	if session.Sess.CSess != nil {
		vpnc.ResetRoutes(session.Sess.CSess) // Keeps the existing circular dependency intact.
		session.Sess.CSess.Close()
	}
}

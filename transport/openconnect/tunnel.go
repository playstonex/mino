package openconnect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/WarrDoge/sslcon/auth"
	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/lib"
	"github.com/WarrDoge/sslcon/proto"
	"github.com/WarrDoge/sslcon/session"
	"github.com/WarrDoge/sslcon/vpn"
	"github.com/metacubex/mihomo/log"
)

type Tunnel struct {
	cSess    *session.ConnSession
	closeOnce sync.Once
	closeChan chan struct{}
}

type TunnelConfig struct {
	Server         string
	Port           int
	Username       string
	Password       string
	Group          string
	Secret         string
	SkipCertVerify bool
	CiscoCompat    bool
	NoDTLS         bool
	MTU            int
}

func NewTunnel(ctx context.Context, cfg TunnelConfig, dialer func(ctx context.Context, network, addr string) (net.Conn, error)) (*Tunnel, error) {
	hostWithPort := net.JoinHostPort(cfg.Server, strconv.Itoa(cfg.Port))

	prof := &lib.Profile{
		Host:         cfg.Server,
		Username:     cfg.Username,
		Password:     cfg.Password,
		Group:        cfg.Group,
		SecretKey:    cfg.Secret,
		HostWithPort: hostWithPort,
		Scheme:       "https://",
		BasePath:     "/",
	}
	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = 1399
	}
	baseCfg := base.NewClientConfig() // applies defaults (AgentVersion, CiscoCompat, etc.)
	baseCfg.InsecureSkipVerify = cfg.SkipCertVerify
	baseCfg.NoDTLS = cfg.NoDTLS
	baseCfg.BaseMTU = mtu
	if !cfg.CiscoCompat {
		baseCfg.CiscoCompat = false
	}
	base.Cfg = baseCfg
	base.LocalInterface = &base.Interface{}
	auth.Prof = prof
	auth.State = lib.NewAuthState()
	session.Sess = &session.Session{}
	vpn.SkipTunSetup = true

	auth.DialTLS = func(network, addr string, config *tls.Config) (*tls.Conn, error) {
		rawConn, err := dialer(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("openconnect dial: %w", err)
		}
		// Ensure ServerName is set for TLS verification when not skipping cert verify.
		if config.ServerName == "" && !config.InsecureSkipVerify {
			host, _, _ := net.SplitHostPort(addr)
			if host != "" {
				config = config.Clone()
				config.ServerName = host
			}
		}
		tlsConn := tls.Client(rawConn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("openconnect tls handshake: %w", err)
		}
		return tlsConn, nil
	}

	if err := auth.InitAuth(); err != nil {
		return nil, fmt.Errorf("openconnect auth init: %w", err)
	}
	log.Debugln("[OpenConnect] auth init succeeded, tunnel-group=%s auth-method=%s", prof.TunnelGroup, prof.AuthMethod)

	if err := auth.PasswordAuth(); err != nil {
		return nil, fmt.Errorf("openconnect password auth: %w", err)
	}
	log.Debugln("[OpenConnect] password auth succeeded")

	if err := vpn.SetupTunnel(); err != nil {
		return nil, fmt.Errorf("openconnect setup tunnel: %w", err)
	}

	cSess := session.Sess.CSess
	if cSess == nil {
		return nil, errors.New("openconnect: no ConnSession after tunnel setup")
	}
	log.Debugln("[OpenConnect] tunnel established: vpn-ip=%s mask=%s mtu=%d dns=%v", cSess.VPNAddress, cSess.VPNMask, cSess.MTU, cSess.DNS)

	return &Tunnel{
		cSess:     cSess,
		closeChan: make(chan struct{}),
	}, nil
}

func (t *Tunnel) ReadPacket() ([]byte, error) {
	select {
	case pl, ok := <-t.cSess.PayloadIn:
		if !ok {
			return nil, net.ErrClosed
		}
		if pl.Type != 0x00 {
			return nil, nil
		}
		return pl.Data, nil
	case <-t.cSess.CloseChan:
		return nil, net.ErrClosed
	case <-t.closeChan:
		return nil, net.ErrClosed
	}
}

func (t *Tunnel) WritePacket(data []byte) error {
	// Allocate buffer with room for the 8-byte CSTP header that payloadOutTLSToServer prepends.
	buf := make([]byte, len(data)+8)
	copy(buf, data)
	pl := &proto.Payload{
		Type: 0x00,
		Data: buf[:len(data)],
	}
	// Prefer DTLS when connected (lower latency), fall back to TLS.
	if t.cSess.DtlsConnected.Load() {
		select {
		case t.cSess.PayloadOutDTLS <- pl:
			return nil
		case <-t.cSess.CloseChan:
			return net.ErrClosed
		case <-t.closeChan:
			return net.ErrClosed
		}
	}
	select {
	case t.cSess.PayloadOutTLS <- pl:
		return nil
	case <-t.cSess.CloseChan:
		return net.ErrClosed
	case <-t.closeChan:
		return net.ErrClosed
	}
}

func (t *Tunnel) VPNAddress() string {
	if t.cSess == nil {
		return ""
	}
	return t.cSess.VPNAddress
}

func (t *Tunnel) VPNMask() string {
	if t.cSess == nil {
		return ""
	}
	return t.cSess.VPNMask
}

func (t *Tunnel) MTU() int {
	if t.cSess == nil {
		return 0
	}
	return t.cSess.MTU
}

func (t *Tunnel) DNS() []string {
	if t.cSess == nil {
		return nil
	}
	return t.cSess.DNS
}

func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		session.Sess.ActiveClose = true
		if t.cSess != nil {
			t.cSess.Close()
		}
		close(t.closeChan)
	})
	return nil
}

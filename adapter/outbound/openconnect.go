package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/metacubex/mihomo/common/pool"
	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/dns"
	"github.com/metacubex/mihomo/log"
	oc "github.com/metacubex/mihomo/transport/openconnect"

	wireguard "github.com/metacubex/sing-wireguard"
	M "github.com/metacubex/sing/common/metadata"
)

type OpenConnect struct {
	*Base
	tunDevice wireguard.Device
	tunnel    *oc.Tunnel
	resolver  resolver.Resolver
	option    OpenConnectOption

	runCtx    context.Context
	runCancel context.CancelFunc
	runMutex  sync.Mutex
	running   bool
}

type OpenConnectOption struct {
	BasicOption
	Name           string `proxy:"name"`
	Server         string `proxy:"server"`
	Port           int    `proxy:"port"`
	Username       string `proxy:"username"`
	Password       string `proxy:"password"`
	Group          string `proxy:"group,omitempty"`
	Secret         string `proxy:"secret,omitempty"`
	SkipCertVerify bool   `proxy:"skip-cert-verify,omitempty"`
	CiscoCompat    bool   `proxy:"cisco-compat,omitempty"`
	NoDTLS         bool   `proxy:"no-dtls,omitempty"`
	MTU            int    `proxy:"mtu,omitempty"`
	UDP            bool   `proxy:"udp,omitempty"`

	RemoteDnsResolve bool     `proxy:"remote-dns-resolve,omitempty"`
	Dns              []string `proxy:"dns,omitempty"`
}

func NewOpenConnect(option OpenConnectOption) (*OpenConnect, error) {
	addr := net.JoinHostPort(option.Server, strconv.Itoa(option.Port))
	outbound := &OpenConnect{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Addr:         addr,
			Type:         C.OpenConnect,
			ProviderName: option.ProviderName,
			UDP:          true,
			TFO:          option.TFO,
			MPTCP:        option.MPTCP,
			Interface:    option.Interface,
			RoutingMark:  option.RoutingMark,
			Prefer:       option.IPVersion,
		}),
		option: option,
	}
	outbound.dialer = option.NewDialer(outbound.DialOptions())
	outbound.runCtx, outbound.runCancel = context.WithCancel(context.Background())
	return outbound, nil
}

func (o *OpenConnect) DialContext(ctx context.Context, metadata *C.Metadata) (_ C.Conn, err error) {
	if err = o.run(ctx); err != nil {
		return nil, err
	}
	var conn net.Conn
	if !metadata.Resolved() || o.resolver != nil {
		r := resolver.DefaultResolver
		if o.resolver != nil {
			r = o.resolver
		}
		options := o.DialOptions()
		options = append(options, dialer.WithResolver(r))
		options = append(options, dialer.WithNetDialer(wgNetDialer{tunDevice: o.tunDevice}))
		conn, err = dialer.NewDialer(options...).DialContext(ctx, "tcp", metadata.RemoteAddress())
	} else {
		conn, err = o.tunDevice.DialContext(ctx, "tcp", M.SocksaddrFrom(metadata.DstIP, metadata.DstPort).Unwrap())
	}
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("conn is nil")
	}
	return NewConn(conn, o), nil
}

func (o *OpenConnect) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (_ C.PacketConn, err error) {
	var pc net.PacketConn
	if err = o.run(ctx); err != nil {
		return nil, err
	}
	if err = o.ResolveUDP(ctx, metadata); err != nil {
		return nil, err
	}
	pc, err = o.tunDevice.ListenPacket(ctx, M.SocksaddrFrom(metadata.DstIP, metadata.DstPort).Unwrap())
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, errors.New("packetConn is nil")
	}
	return NewPacketConn(pc, o), nil
}

func (o *OpenConnect) ResolveUDP(ctx context.Context, metadata *C.Metadata) error {
	if (!metadata.Resolved() || o.resolver != nil) && metadata.Host != "" {
		r := resolver.DefaultResolver
		if o.resolver != nil {
			r = o.resolver
		}
		ip, err := resolveIPWithResolver(ctx, metadata.Host, o.prefer, r)
		if err != nil {
			return fmt.Errorf("can't resolve ip: %w", err)
		}
		metadata.DstIP = ip
	}
	return nil
}

func (o *OpenConnect) ProxyInfo() C.ProxyInfo {
	info := o.Base.ProxyInfo()
	info.DialerProxy = o.option.DialerProxy
	return info
}

func (o *OpenConnect) IsL3Protocol(metadata *C.Metadata) bool {
	return true
}

func (o *OpenConnect) Close() error {
	if o.runCancel != nil {
		o.runCancel()
	}
	o.runMutex.Lock()
	tunDevice := o.tunDevice
	tunnel := o.tunnel
	o.tunDevice = nil
	o.tunnel = nil
	o.running = false
	o.runMutex.Unlock()
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if tunDevice != nil {
		return tunDevice.Close()
	}
	return nil
}

func (o *OpenConnect) run(ctx context.Context) error {
	o.runMutex.Lock()
	defer o.runMutex.Unlock()
	if o.running {
		return nil
	}
	if o.runCtx.Err() != nil {
		return o.runCtx.Err()
	}

	cfg := oc.TunnelConfig{
		Server:         o.option.Server,
		Port:           o.option.Port,
		Username:       o.option.Username,
		Password:       o.option.Password,
		Group:          o.option.Group,
		Secret:         o.option.Secret,
		SkipCertVerify: o.option.SkipCertVerify,
		CiscoCompat:    o.option.CiscoCompat,
		NoDTLS:         o.option.NoDTLS,
		MTU:            o.option.MTU,
	}

	dialFn := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return o.dialer.DialContext(ctx, "tcp", o.addr)
	}

	tunnel, err := oc.NewTunnel(ctx, cfg, dialFn)
	if err != nil {
		return err
	}

	vpnIP := tunnel.VPNAddress()
	vpnMask := tunnel.VPNMask()
	if vpnIP == "" {
		tunnel.Close()
		return errors.New("openconnect: no VPN address from server")
	}

	prefix, err := parseVPNPrefix(vpnIP, vpnMask)
	if err != nil {
		tunnel.Close()
		return fmt.Errorf("openconnect: parse VPN address: %w", err)
	}

	mtu := tunnel.MTU()
	if mtu <= 0 {
		mtu = 1399
	}

	tunDevice, err := wireguard.NewStackDevice([]netip.Prefix{prefix}, uint32(mtu))
	if err != nil {
		tunnel.Close()
		return fmt.Errorf("openconnect: create stack device: %w", err)
	}
	if err := tunDevice.Start(); err != nil {
		tunnel.Close()
		_ = tunDevice.Close()
		return err
	}

	var remoteResolver resolver.Resolver
	if o.option.RemoteDnsResolve && o.resolver == nil {
		dnsServers := o.option.Dns
		// Fall back to DNS servers provided by the OpenConnect server.
		if len(dnsServers) == 0 {
			for _, d := range tunnel.DNS() {
				dnsServers = append(dnsServers, d)
			}
		}
		// Always keep public resolvers as backup — server-provided DNS may be
		// absent or unusable. All nameservers race and the fastest valid
		// answer wins, so healthy entries keep serving while dead ones lose.
		for _, backup := range []string{"8.8.8.8", "1.1.1.1"} {
			if !slices.Contains(dnsServers, backup) {
				dnsServers = append(dnsServers, backup)
			}
		}
		log.Infoln("[OpenConnect](%s) remote DNS servers: %v (server provided: %v)", o.name, dnsServers, tunnel.DNS())
		if len(dnsServers) > 0 {
			nss, err := dns.ParseNameServer(dnsServers)
			if err != nil {
				tunnel.Close()
				_ = tunDevice.Close()
				return fmt.Errorf("openconnect: parse remote DNS: %w", err)
			}
			for i := range nss {
				nss[i].ProxyAdapter = o
			}
			remoteResolver = dns.NewResolver(dns.Config{
				Main: nss,
				IPv6: false,
			})
		}
	}

	o.tunDevice = tunDevice
	o.tunnel = tunnel
	if remoteResolver != nil {
		o.resolver = remoteResolver
	}
	o.running = true
	log.Debugln("[OpenConnect](%s) tunnel established: vpn-ip=%s mtu=%d", o.name, prefix, mtu)

	o.startPacketLoops(tunnel)
	return nil
}

func (o *OpenConnect) startPacketLoops(tunnel *oc.Tunnel) {
	runCtx, runCancel := context.WithCancel(o.runCtx)
	tunDevice := o.tunDevice
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			runCancel()
			_ = tunnel.Close()
			_ = tunDevice.Close()
			o.runMutex.Lock()
			if o.tunDevice == tunDevice {
				o.tunDevice = nil
				o.tunnel = nil
				o.running = false
			}
			o.runMutex.Unlock()
		})
	}

	go func() {
		defer stop()
		buf := pool.Get(pool.UDPBufferSize)
		defer pool.Put(buf)
		bufs := [][]byte{buf}
		sizes := []int{0}
		for runCtx.Err() == nil {
			_, err := tunDevice.Read(bufs, sizes, 0)
			if err != nil {
				if runCtx.Err() == nil && !errors.Is(err, net.ErrClosed) {
					log.Errorln("[OpenConnect](%s) error reading from stack device: %v", o.name, err)
				}
				return
			}
			if err := tunnel.WritePacket(buf[:sizes[0]]); err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Warnln("[OpenConnect](%s) error writing packet: %v", o.name, err)
				}
				return
			}
		}
	}()

	go func() {
		defer stop()
		for runCtx.Err() == nil {
			packet, err := tunnel.ReadPacket()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Warnln("[OpenConnect](%s) error reading packet: %v", o.name, err)
				}
				return
			}
			if len(packet) == 0 {
				continue
			}
			if _, err := tunDevice.Write([][]byte{packet}, 0); err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Errorln("[OpenConnect](%s) error writing to stack device: %v", o.name, err)
				}
				return
			}
		}
	}()
}

func parseVPNPrefix(ip, mask string) (netip.Prefix, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("parse IP %q: %w", ip, err)
	}
	if strings.Contains(mask, ".") {
		m := net.ParseIP(mask)
		if m == nil {
			return netip.Prefix{}, fmt.Errorf("parse mask %q", mask)
		}
		mask4 := m.To4()
		if mask4 != nil {
			ones := 0
			for _, b := range mask4 {
				for b != 0 {
					ones++
					b &= b - 1
				}
			}
			return netip.PrefixFrom(addr, ones), nil
		}
	}
	bits, _ := strconv.Atoi(mask)
	if bits > 0 {
		return netip.PrefixFrom(addr, bits), nil
	}
	return netip.PrefixFrom(addr, 32), nil
}

package mate

import (
	"context"
	"runtime"
	runtimeDebug "runtime/debug"

	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"

	tun "github.com/metacubex/sing-tun"

	E "github.com/metacubex/sing/common/exceptions"
)

func init() {
	// iOS Network Extension memory limit is ~50MB (iOS 17+), ~15MB (older).
	// Cap Go heap to leave room for C/ObjC allocations and stack.
	runtimeDebug.SetMemoryLimit(30 * 1024 * 1024) // 30 MB

	// Limit OS threads — Network Extension has limited CPU budget.
	runtime.GOMAXPROCS(3)

	// More aggressive GC to stay within memory budget.
	runtimeDebug.SetGCPercent(50)
}

type MihomoService struct {
	ctx    context.Context
	cancel context.CancelFunc
	*config.Config
	plantformWrapper *platformInterfaceWrapper
}

func NewService(configPath string, platformInterface PlatformInterface) (*MihomoService, error) {
	ctx := BaseContext(platformInterface)
	config, err := executor.ParseWithPath(configPath)
	if err != nil {
		return nil, err
	}
	runtimeDebug.FreeOSMemory()
	ctx, cancel := context.WithCancel(ctx)
	platformWrapper := &platformInterfaceWrapper{
		iif:       platformInterface,
		useProcFS: platformInterface.UseProcFS(),
	}

	runtimeDebug.FreeOSMemory()
	return &MihomoService{
		ctx:              ctx,
		cancel:           cancel,
		Config:           config,
		plantformWrapper: platformWrapper,
	}, nil
}

func (s *MihomoService) Start() error {
	hub.ApplyConfig(s.Config)
	return nil
}

func (s *MihomoService) Close() error {
	s.cancel()
	executor.Shutdown()
	return nil
}

// ReloadConfig re-parses the config file at configPath and applies only
// the proxy, rule, and provider configuration — without touching the TUN,
// DNS server, or network listeners.
//
// This is called from the overlay manager after injecting p2p outbound
// entries into the hybrid YAML.
//
// CRITICAL: We must NOT call hub.ApplyConfig or executor.ApplyConfig here.
// Those functions call updateTun() which destroys and recreates the TUN
// device.  Inside a Network Extension the TUN file descriptor is provided
// by the OS (NEPacketTunnelProvider / NEPacketTunnelFlow).  Once destroyed,
// a new TUN is disconnected from the OS packet flow — all proxy traffic
// stops permanently.  The overlay injection only changes:
//   - proxies  (adds p2p outbound entries)
//   - proxy-groups  (adds overlay group)
//   - rules  (adds overlay IP-CIDR rule)
//
// So we perform a targeted "light reload" that updates ONLY those parts.
func (s *MihomoService) ReloadConfig(configPath string) error {
	cfg, err := executor.ParseWithPath(configPath)
	if err != nil {
		return err
	}

	// Suspend the tunnel while we swap proxies and rules.
	tunnel.OnSuspend()

	// Replace proxy map (includes new p2p outbounds) and rule list.
	tunnel.UpdateProxies(cfg.Proxies, cfg.Providers)
	tunnel.UpdateRules(cfg.Rules, cfg.SubRules, cfg.RuleProviders)

	tunnel.OnInnerLoading()

	// Initialize providers so rule-sets can be fetched.
	for _, pv := range cfg.Providers {
		if initErr := pv.Initial(); initErr != nil {
			log.Warnln("initial proxy provider %s error: %v", pv.Name(), initErr)
		}
	}
	for _, pv := range cfg.RuleProviders {
		if initErr := pv.Initial(); initErr != nil {
			log.Warnln("initial rule provider %s error: %v", pv.Name(), initErr)
		}
	}

	// Resume packet processing with the new config.
	tunnel.OnRunning()
	return nil
}

func SetHomeDir(homeDir string) {
	C.SetHomeDir(homeDir)
}

func SetCacheDir(cacheDir string) {
	// C.SetCacheDir(cacheDir)
}

func SetLogDir(logDir string) {
	C.Path.SetLogDir(logDir)
}

func SetTunCreator(acreator C.TunListenOutterCreator) {
	C.SetOutterCreator(acreator)
}

type platformInterfaceWrapper struct {
	iif       PlatformInterface
	useProcFS bool
	// networkManger          adapter.NetworkManager
	myTunName string
}

func (w *platformInterfaceWrapper) UsePlatformAutoDetectInterfaceControl() bool {
	return w.iif.UsePlatformAutoDetectInterfaceControl()
}

func (w *platformInterfaceWrapper) AutoDetectInterfaceControl(fd int) error {
	return w.iif.AutoDetectInterfaceControl(int32(fd))
}

func (w *platformInterfaceWrapper) OpenTun(options *tun.Options) (tun.Tun, error) {
	if len(options.IncludeUID) > 0 || len(options.ExcludeUID) > 0 {
		return nil, E.New("platform: unsupported uid options")
	}
	if len(options.IncludeAndroidUser) > 0 {
		return nil, E.New("platform: unsupported android_user option")
	}
	routeRanges, err := options.BuildAutoRouteRanges(true)
	if err != nil {
		return nil, err
	}
	tunFd, err := w.iif.OpenTun(&tunOptions{options, routeRanges})
	if err != nil {
		return nil, err
	}
	options.Name, err = getTunnelName(tunFd)
	if err != nil {
		return nil, E.Cause(err, "query tun name")
	}
	// options.InterfaceMonitor.RegisterMyInterface(options.Name)
	dupFd, err := dup(int(tunFd))
	if err != nil {
		return nil, E.Cause(err, "dup tun file descriptor")
	}
	options.FileDescriptor = dupFd
	w.myTunName = options.Name
	return tun.New(*options)
}

func (w *platformInterfaceWrapper) UnderNetworkExtension() bool {
	return w.iif.UnderNetworkExtension()
}

func (w *platformInterfaceWrapper) IncludeAllNetworks() bool {
	return w.iif.IncludeAllNetworks()
}

func (w *platformInterfaceWrapper) ClearDNSCache() {
	w.iif.ClearDNSCache()
}
func (w *platformInterfaceWrapper) SystemCertificates() []string {
	return iteratorToArray[string](w.iif.SystemCertificates())
}

func (w *platformInterfaceWrapper) DisableColors() bool {
	return runtime.GOOS != "android"
}

func (w *platformInterfaceWrapper) WriteMessage(level log.LogLevel, message string) {
	w.iif.WriteLog(message)
}

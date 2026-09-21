package mate

import (
	"context"
	"fmt"
	"os"
	"runtime"
	runtimeDebug "runtime/debug"

	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/log"

	tun "github.com/playstonex/sing-tun"

	E "github.com/metacubex/sing/common/exceptions"
)

func init() {
	switch runtime.GOOS {
	case "ios":
		// 20 MB, and the number is derived rather than chosen.
		//
		// This was 40 MB on the reasoning that an iOS Network Extension gets
		// ~50 MB and Go should be left most of it. Measurement showed that
		// makes the limit UNREACHABLE: the process is killed on its
		// phys_footprint, and footprint runs about 24 MB ahead of the live heap
		// (measured across one kill: 18.5 vs 3.5, 22.4 vs 4.6, 32.5 vs 10.2,
		// 47.2 vs 22.8). So the fatal heap is ~23 MB, and a limit set at 40 MB
		// can only ever be crossed by a process that is already dead. Every run
		// died with the limit reporting 57-65% used.
		//
		//	footprint <= 44 MB  =>  live heap <= 20 MB
		//
		// 44 MB rather than the ~50 MB ceiling because a limit that binds only
		// at the kill line has no time to act.
		//
		// The trade-off is deliberate and is NOT free: a soft limit does not
		// fail allocations, it makes the collector work harder, so a transfer
		// that wants more than 20 MB of live heap will now see GC CPU climb and
		// throughput fall instead of the tunnel dying. Measured GC CPU at the
		// old cliff was already 16-19%, so expect more. That is the intended
		// exchange -- degraded and alive beats killed and reconnecting, because
		// a kill drops every connection while back-pressure only slows them.
		const iosMemLimit = 20 * 1024 * 1024
		runtimeDebug.SetMemoryLimit(iosMemLimit)
		// Network Extension has limited CPU budget.
		runtime.GOMAXPROCS(3)
		// 50. This was raised to 100 on the theory that GC CPU was the binding
		// constraint, and the device then refuted it: measured GC CPU is 0.0%
		// to 0.5%, nowhere near the runtime's 50%-of-budget limiter, and
		// measured process CPU is 1% of a core. There is no GC CPU problem to
		// trade footprint for, so the trade was all cost.
		//
		// What the device did report, via Xcode on 2026-09-21 14:13:35, is
		// `Terminated due to memory issue` — SIGKILL 9 on the process
		// footprint. And the shape of the growth says the soft limit above
		// cannot prevent it: live heap stays at 5-15 MB while total mapped
		// climbs (measured 9.6 -> 18.7 -> 26.5 MB over 90 s, with footprint
		// tracking it at 15.4 -> 24.6 -> 33.1 MB). SetMemoryLimit is compared
		// against LIVE HEAP, which never approaches 40 MB, so it never
		// intervenes — while the quantity iOS actually kills on keeps rising.
		//
		// GOGC is the lever that does bear on mapped memory: it decides how
		// much garbage accumulates before a collection, and therefore how many
		// spans the heap has to have mapped to hold it. Lower means collect
		// sooner, retain less, map less. 50 costs GC CPU that measurement shows
		// is available in abundance, and buys back the only resource that is
		// actually scarce here.
		const iosGCPercent = 50
		runtimeDebug.SetGCPercent(iosGCPercent)
		recordRuntimeLimits(iosMemLimit, iosGCPercent)

	case "darwin":
		// macOS has no meaningful memory constraint for Network Extension.
		// Leave defaults (no memory limit, GOMAXPROCS = NumCPU, GCPercent = 100).
		// Recorded as 0/100 so GetRuntimeStatsJSON reports "unlimited" rather
		// than an invented number.
		recordRuntimeLimits(0, 100)

	default:
		// Conservative defaults for unknown platforms.
		const defaultMemLimit = 40 * 1024 * 1024
		const defaultGCPercent = 50
		runtimeDebug.SetMemoryLimit(defaultMemLimit)
		runtime.GOMAXPROCS(3)
		runtimeDebug.SetGCPercent(defaultGCPercent)
		recordRuntimeLimits(defaultMemLimit, defaultGCPercent)
	}
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
	fmt.Fprintf(os.Stderr, "[mate] MihomoService.Start: applying config...\n")
	hub.ApplyConfig(s.Config)
	fmt.Fprintf(os.Stderr, "[mate] MihomoService.Start: config applied OK\n")
	return nil
}

func (s *MihomoService) Close() error {
	s.cancel()
	executor.Shutdown()
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

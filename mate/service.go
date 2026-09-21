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
		// iOS Network Extension memory limit is ~50 MB (iOS 15+).
		// Reserve headroom for C/ObjC/stack — cap Go heap at 40 MB.
		const iosMemLimit = 40 * 1024 * 1024
		runtimeDebug.SetMemoryLimit(iosMemLimit)
		// Network Extension has limited CPU budget.
		runtime.GOMAXPROCS(3)
		// 100, not 50, and the reasoning matters because this was 50 and the
		// process livelocked.
		//
		// SetMemoryLimit is a SOFT limit: breaching it does not fail an
		// allocation, it makes the collector run harder and harder to get back
		// under, up to the runtime's 50%-of-CPU GC limiter. On a tunnel with
		// GOMAXPROCS(3) on efficiency cores that presents as the extension
		// being unable to move packets — unresponsive, not crashed.
		//
		// That is what was captured on 2026-09-21: an Xcode thread dump taken
		// while the tunnel was down under a saturating speedtest showed two GC
		// mark workers draining concurrently at a 48.4 MB footprint, with NO
		// crash report and NO JetsamEvent. No Jetsam kill means the footprint
		// was survivable and CPU was the binding constraint, not RSS.
		//
		// The 40 MB limit above already guarantees the ceiling on its own — as
		// the heap approaches it the runtime collects earlier regardless of
		// this value. So GOGC's only remaining job is steady-state pacing, and
		// 50 was paying extra GC CPU for a bound the memory limit already
		// enforces. 100 trades a little idle heap footprint for materially
		// less GC CPU, which is the axis that actually failed.
		//
		// Changed ALONE, on purpose: the 40 MB limit is left exactly as it was
		// so the next device run measures one variable. Picking a new limit
		// needs the live-heap number from GetRuntimeStatsJSON
		// (mate/runtime_stats.go), which did not exist before this change —
		// the old value could only have been guessed at.
		const iosGCPercent = 100
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

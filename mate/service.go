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
		// 40 MB, restored after 20 MB was measured to be actively harmful.
		//
		// The reasoning for 20 MB was arithmetically sound and empirically
		// wrong. footprint runs ~19 MB ahead of live heap and iOS kills at
		// ~50 MB, so a heap ceiling of 20 MB keeps footprint at a safe 39 MB.
		// What that reasoning missed is that the workload's real demand is
		// ~28 MB of LIVE heap, and a soft limit below demand does not cap
		// anything -- it makes the collector chase a target it cannot reach:
		//
		//	heap=21.5MiB (107% of limit)  numGC= 98  gc-cpu=12.5%
		//	heap=28.3MiB (141%)           numGC=154  gc-cpu=11.9%
		//	heap=26.0MiB (130%)           numGC=219  gc-cpu=32.0%
		//	heap= 5.2MiB ( 26%)           numGC=635  gc-cpu=41.6%  proc-cpu=82%
		//
		// 635 collections and 41.6% of the CPU budget in GC, against the
		// runtime's own 50% limiter. Throughput measured 3.45 Mbps -- the
		// extension was spending its CPU collecting rather than moving packets.
		// footprint still reached 47.4 MB, so the limit did not even buy the
		// safety it was chosen for.
		//
		// 40 MB is above demand, so the pacer does not bind at the working
		// point and the data path gets its CPU back. Anything nearer 28 MB
		// reintroduces the spiral, because the limit-based GC target tightens
		// as the heap APPROACHES the limit, not only when it crosses.
		//
		// This is a deliberate trade, not a fix. It restores full throughput
		// and restores the ~62 s SIGKILL under sustained saturation, which the
		// app-side self-heal reconnects from. The real bind is that measured
		// demand (28 MB heap -> 47 MB footprint) does not fit a ~50 MB budget
		// with Go's ~19 MB of runtime overhead on top, and no value of this
		// constant satisfies both sides. What closes it is bounding aggregate
		// in-flight bytes -- gVisor's receive-buffer moderation is per
		// connection and nothing limits the sum.
		const iosMemLimit = 40 * 1024 * 1024
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

		// The soft limit and GCPercent above bound the LIVE heap, but the
		// 2026-09-22 profiles proved the kill is on phys_footprint while the
		// live heap stays at ~16 MB: freed spans the background scavenger
		// cannot return fast enough under packet churn. Arm a periodic,
		// threshold-gated FreeOSMemory that forces those spans back to the OS
		// (via MADV_FREE_REUSABLE, which leaves phys_footprint) before Jetsam
		// charges for them. See footprint_reclaim.go.
		startFootprintReclaimer()

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

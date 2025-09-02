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

	tun "github.com/metacubex/sing-tun"

	E "github.com/metacubex/sing/common/exceptions"
)

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

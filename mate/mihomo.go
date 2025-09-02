package mate

import (
	"fmt"
	"runtime"
	// "github.com/metacubex/mihomo/config"
	// "github.com/metacubex/mihomo/service"
)

func Say(name string) string {
	if name == "" {
		return "Hello, mihomo!"
	}
	return fmt.Sprintf("Hello, %s!", runtime.GOOS)
}

var globalService *MihomoService

type ServerConfig struct {
	ConfigPath string
	HomeDir    string
	CacheDir   string
	LogDir     string
	ErroDir    string
}

func Start(config *ServerConfig, platformInterface PlatformInterface) error {
	var err error

	configPath := config.ConfigPath
	if configPath == "" {
		configPath = "config.yaml"
	}
	if config.HomeDir != "" {
		SetHomeDir(config.HomeDir)
	}

	if config.CacheDir != "" {
		SetCacheDir(config.CacheDir)
	}

	if config.LogDir != "" {
		SetLogDir(config.LogDir)
	}

	globalService, err = NewService(configPath, platformInterface)
	if err != nil {
		return err
	}
	SetTunCreator(globalService.plantformWrapper)
	return globalService.Start()
}

func Close() error {
	if globalService != nil {
		return globalService.Close()
	}
	return nil
}

// func Version() string {
// 	fmt.Printf("Mihomo Meta %s %s %s with %s %s\n",
// 		C.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), C.BuildTime)

// 	return fmt.Sprintf("Mihomo Meta %s %s %s with %s %s\n",
// 		C.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), C.BuildTime)
// }

// func TestConfig(configPath string, homePath string) bool {
// 	if homePath != "" {
// 		C.SetHomeDir(homePath)
// 	}
// 	configBytes, err := os.ReadFile(configPath)
// 	if err != nil {
// 		fmt.Println("Initial configuration error:", err.Error())
// 		return false
// 	}

// 	if len(configBytes) != 0 {
// 		if _, err := executor.ParseWithBytes(configBytes); err != nil {
// 			fmt.Println("configuration test failed")
// 			return false
// 		}
// 	} else {
// 		if _, err := executor.Parse(); err != nil {
// 			fmt.Printf("configuration file %s test failed\n", C.Path.Config())
// 			return false
// 		}
// 	}
// 	fmt.Printf("configuration file %s test is successful\n", C.Path.Config())
// 	return true
// }

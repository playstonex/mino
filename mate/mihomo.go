package mate

import (
	"encoding/json"
	"fmt"
	"runtime"
	"time"
	// "github.com/metacubex/mihomo/config"
	// "github.com/metacubex/mihomo/service"

	"github.com/metacubex/mihomo/transport/p2p"
	"github.com/metacubex/mihomo/tunnel/statistic"
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

	globalOverlayTransport.Reset()
	globalOverlayTransport.SetPlatform(platformInterface)

	// Reset P2P state from any previous session (Close+Start restart cycle
	// leaves the singleton manager with stale PeerConnections and callbacks).
	m := p2p.GetManager()
	m.Reset()

	// Register P2P signaling callbacks
	m.OnLocalDescription = func(peerID string, sdp string, sdpType string) {
		platformInterface.OnLocalDescription(peerID, sdp, sdpType)
	}
	m.OnLocalCandidate = func(peerID string, candidate string) {
		platformInterface.OnLocalCandidate(peerID, candidate)
	}
	m.OnConnectionStateChange = func(peerID string, state string) {
		platformInterface.OnP2PConnectionStateChange(peerID, state)
	}

	SetTunCreator(globalService.plantformWrapper)
	return globalService.Start()
}
func Close() error {
	globalOverlayTransport.Reset()
	if globalService != nil {
		return globalService.Close()
	}
	return nil
}

type MonitorConnection struct {
	ID                string `json:"id"`
	Domain            string `json:"domain"`
	IP                string `json:"ip"`
	RemoteDestination string `json:"remoteDestination"`
	Network           string `json:"network"`
	Type              string `json:"type"`
	Process           string `json:"process"`
	Upload            int64  `json:"upload"`
	Download          int64  `json:"download"`
	Start             string `json:"start"`
}

type MonitorSnapshot struct {
	UploadRate    int64                `json:"uploadRate"`
	DownloadRate  int64                `json:"downloadRate"`
	UploadTotal   int64                `json:"uploadTotal"`
	DownloadTotal int64                `json:"downloadTotal"`
	Memory        uint64               `json:"memory"`
	Connections   []*MonitorConnection `json:"connections"`
}

func MonitorSnapshotJSON() string {
	upRate, downRate := statistic.DefaultManager.Now()
	snapshot := statistic.DefaultManager.Snapshot()

	connections := make([]*MonitorConnection, 0, len(snapshot.Connections))
	for _, tracker := range snapshot.Connections {
		var domain string
		var ip string
		var remote string
		var network string
		var ctype string
		var process string

		if tracker.Metadata != nil {
			domain = tracker.Metadata.RuleHost()
			if domain == "" {
				domain = tracker.Metadata.Host
			}
			if tracker.Metadata.DstIP.IsValid() {
				ip = tracker.Metadata.DstIP.String()
			}
			remote = tracker.Metadata.RemoteDst
			network = tracker.Metadata.NetWork.String()
			ctype = tracker.Metadata.Type.String()
			process = tracker.Metadata.Process
		}

		connections = append(connections, &MonitorConnection{
			ID:                tracker.UUID.String(),
			Domain:            domain,
			IP:                ip,
			RemoteDestination: remote,
			Network:           network,
			Type:              ctype,
			Process:           process,
			Upload:            tracker.UploadTotal.Load(),
			Download:          tracker.DownloadTotal.Load(),
			Start:             tracker.Start.Format(time.RFC3339),
		})
	}

	payload := MonitorSnapshot{
		UploadRate:    upRate,
		DownloadRate:  downRate,
		UploadTotal:   snapshot.UploadTotal,
		DownloadTotal: snapshot.DownloadTotal,
		Memory:        snapshot.Memory,
		Connections:   connections,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(data)
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

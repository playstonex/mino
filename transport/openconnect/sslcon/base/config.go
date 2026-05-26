package base

var (
	Cfg            = NewClientConfig()
	LocalInterface = NewInterface()
)

type ClientConfig struct {
	LogLevel           string   `json:"log_level"`
	LogPath            string   `json:"log_path"`
	DebugLogPath       string   `json:"debug_log_path"`
	RPCAddr            string   `json:"rpc_addr"`
	InsecureSkipVerify bool     `json:"skip_verify"`
	CiscoCompat        bool     `json:"cisco_compat"`
	NoDTLS             bool     `json:"no_dtls"`
	AgentName          string   `json:"agent_name"`
	AgentVersion       string   `json:"agent_version"`
	BaseMTU            int      `json:"base_mtu"`
	SplitRoutes        []string `json:"split_routes"`
	DNSDomains         []string `json:"dns_domains"`
	ServerCertPin      string   `json:"server_cert"`
}

type Interface struct {
	Name    string `json:"name"`
	Ip4     string `json:"ip4"`
	Mac     string `json:"mac"`
	Gateway string `json:"gateway"`
}

func NewClientConfig() *ClientConfig {
	cfg := &ClientConfig{}
	ApplyDefaults(cfg)
	return cfg
}

func NewInterface() *Interface {
	return &Interface{}
}

func ApplyDefaults(cfg *ClientConfig) {
	if cfg == nil {
		return
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "Debug"
	}
	if cfg.RPCAddr == "" {
		cfg.RPCAddr = "127.0.0.1:6210"
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = "4.10.07062"
	}
	if cfg.BaseMTU == 0 {
		cfg.BaseMTU = 1399
	}
	cfg.CiscoCompat = true
}

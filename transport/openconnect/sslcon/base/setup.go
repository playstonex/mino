package base

func Setup() {
	ApplyDefaults(Cfg)
	// The default startup logger covers RPC startup and UI connection to the RPC service.
	// The UI must push config proactively after connect or after config changes.
	InitLog()
}

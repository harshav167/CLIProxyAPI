package config

func newConfigWithDefaults() Config {
	var cfg Config
	cfg.Host = ""
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.Observability.ServiceName = "cliproxy"
	cfg.Observability.Environment = "local"
	cfg.Observability.OTLP.Endpoint = "http://localhost:57018"
	cfg.Observability.OTLP.Protocol = "http/protobuf"
	cfg.Observability.OTLP.Insecure = true
	cfg.Observability.OTLP.Traces = true
	cfg.Observability.OTLP.Metrics = true
	cfg.Observability.OTLP.Logs = true
	cfg.Observability.OTLP.SampleRatio = 1.0
	cfg.DisableCooling = false
	cfg.SaveCooldownStatus = false
	cfg.TransientErrorCooldownSeconds = 0
	cfg.DisableImageGeneration = DisableImageGenerationOff
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.CodexResponseChaining.Enabled = true
	return cfg
}

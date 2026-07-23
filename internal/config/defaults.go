package config

import (
	log "github.com/sirupsen/logrus"
)

// MaxRedisUsageQueueRetentionSeconds is 7 days. Shared by parse + load paths.
const MaxRedisUsageQueueRetentionSeconds = 7 * 24 * 3600

func normalizeRedisUsageQueueRetention(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
		return
	}
	if cfg.RedisUsageQueueRetentionSeconds > MaxRedisUsageQueueRetentionSeconds {
		log.WithField("value", cfg.RedisUsageQueueRetentionSeconds).Warn("redis-usage-queue-retention-seconds too large; clamping to 604800 (7 days)")
		cfg.RedisUsageQueueRetentionSeconds = MaxRedisUsageQueueRetentionSeconds
	}
}

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

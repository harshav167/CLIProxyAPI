package observability

import (
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	defaultServiceName = "cliproxy"
	defaultEnvironment = "local"
	defaultEndpoint    = "http://localhost:57018"
	defaultProtocol    = "http/protobuf"
)

// Settings is the normalized observability runtime configuration.
type Settings struct {
	Enabled                bool
	ServiceName            string
	Environment            string
	Endpoint               string
	Protocol               string
	Headers                map[string]string
	RedactedHeaders        map[string]string
	Insecure               bool
	Traces                 bool
	Metrics                bool
	Logs                   bool
	TransportLogs          bool
	TransportLogsFullBody  bool
	TransportLogsErrorBody bool
	SampleRatio            float64
	ResourceAttributes     map[string]string
}

// Normalize resolves YAML and OpenTelemetry environment overrides into a
// conservative runtime config. Observability stays disabled unless explicitly
// enabled by YAML or CLIPROXY_OBSERVABILITY_ENABLED.
func Normalize(cfg *config.Config) Settings {
	settings := Settings{
		ServiceName: defaultServiceName,
		Environment: defaultEnvironment,
		Endpoint:    defaultEndpoint,
		Protocol:    defaultProtocol,
		Headers:     map[string]string{},
		Insecure:    true,
		Traces:      true,
		Metrics:     true,
		Logs:        true,
		SampleRatio: 1.0,
	}

	if cfg != nil {
		obs := cfg.Observability
		settings.Enabled = obs.Enabled
		settings.ServiceName = firstNonEmpty(obs.ServiceName, settings.ServiceName)
		settings.Environment = firstNonEmpty(obs.Environment, settings.Environment)
		settings.Endpoint = firstNonEmpty(obs.OTLP.Endpoint, settings.Endpoint)
		settings.Protocol = firstNonEmpty(obs.OTLP.Protocol, settings.Protocol)
		settings.Headers = cloneStringMap(obs.OTLP.Headers)
		settings.Insecure = obs.OTLP.Insecure
		settings.Traces = obs.OTLP.Traces
		settings.Metrics = obs.OTLP.Metrics
		settings.Logs = obs.OTLP.Logs
		settings.TransportLogs = obs.TransportLogs
		settings.TransportLogsFullBody = obs.TransportLogsFullBody
		settings.TransportLogsErrorBody = obs.TransportLogsErrorBody
		settings.SampleRatio = obs.OTLP.SampleRatio
	}

	settings.Enabled = envBool("CLIPROXY_OBSERVABILITY_ENABLED", settings.Enabled)
	settings.TransportLogs = envBool("CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS", settings.TransportLogs)
	settings.TransportLogsFullBody = envBool("CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS_FULL_BODY", settings.TransportLogsFullBody)
	settings.TransportLogsErrorBody = envBool("CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS_ERROR_BODY", settings.TransportLogsErrorBody)
	settings.ServiceName = firstNonEmpty(os.Getenv("OTEL_SERVICE_NAME"), settings.ServiceName)
	settings.Endpoint = firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), settings.Endpoint)
	settings.Protocol = firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"), settings.Protocol)
	if headers := parseKeyValueCSV(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")); len(headers) > 0 {
		settings.Headers = headers
	}
	if envAttrs := parseKeyValueCSV(os.Getenv("OTEL_RESOURCE_ATTRIBUTES")); len(envAttrs) > 0 {
		settings.ResourceAttributes = envAttrs
		if env := strings.TrimSpace(envAttrs["deployment.environment"]); env != "" {
			settings.Environment = env
		}
	}

	settings.Endpoint = strings.TrimRight(strings.TrimSpace(settings.Endpoint), "/")
	if settings.Endpoint == "" {
		settings.Endpoint = defaultEndpoint
	}
	if !settings.TransportLogs {
		settings.TransportLogsFullBody = false
		settings.TransportLogsErrorBody = false
	}
	settings.Protocol = strings.ToLower(strings.TrimSpace(settings.Protocol))
	if settings.Protocol == "" {
		settings.Protocol = defaultProtocol
	}
	settings.ServiceName = firstNonEmpty(settings.ServiceName, defaultServiceName)
	settings.Environment = firstNonEmpty(settings.Environment, defaultEnvironment)
	settings.RedactedHeaders = redactHeaders(settings.Headers)
	if settings.SampleRatio <= 0 {
		settings.SampleRatio = 1.0
	}
	if settings.SampleRatio > 1 {
		settings.SampleRatio = 1.0
	}
	return settings
}

func traceEndpoint(endpoint string) string  { return signalEndpoint(endpoint, "/v1/traces") }
func metricEndpoint(endpoint string) string { return signalEndpoint(endpoint, "/v1/metrics") }
func logEndpoint(endpoint string) string    { return signalEndpoint(endpoint, "/v1/logs") }

func signalEndpoint(endpoint, signalPath string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}
	if strings.HasSuffix(parsed.Path, signalPath) {
		return parsed.String()
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = signalPath
		return parsed.String()
	}
	return parsed.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func envBool(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return parsed
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		dst[key] = strings.TrimSpace(value)
	}
	return dst
}

// parseKeyValueCSV parses an OTel-style comma-separated `key=value` list into a
// map, trimming whitespace and skipping malformed/empty entries. Shared by the
// OTEL_EXPORTER_OTLP_HEADERS and OTEL_RESOURCE_ATTRIBUTES parsers, which use
// identical syntax.
func parseKeyValueCSV(raw string) map[string]string {
	result := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		result[key] = strings.TrimSpace(value)
	}
	return result
}

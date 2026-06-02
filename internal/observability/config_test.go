package observability

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestNormalizeDisabledByDefault(t *testing.T) {
	settings := Normalize(&config.Config{})
	if settings.Enabled {
		t.Fatal("observability enabled by default")
	}
	if settings.ServiceName != "cliproxy" {
		t.Fatalf("service name = %q, want cliproxy", settings.ServiceName)
	}
	if settings.Endpoint != "http://localhost:57018" {
		t.Fatalf("endpoint = %q, want local SigNoz endpoint", settings.Endpoint)
	}
}

func TestNormalizeEnvironmentOverrides(t *testing.T) {
	t.Setenv("CLIPROXY_OBSERVABILITY_ENABLED", "true")
	t.Setenv("CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS", "true")
	t.Setenv("CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS_FULL_BODY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://signoz.example.com")
	t.Setenv("OTEL_SERVICE_NAME", "cliproxy-test")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test,service.namespace=cliproxy")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "CF-Access-Client-Id=id,Authorization=Bearer secret")

	settings := Normalize(&config.Config{})
	if !settings.Enabled {
		t.Fatal("observability not enabled from env")
	}
	if settings.Endpoint != "https://signoz.example.com" {
		t.Fatalf("endpoint = %q", settings.Endpoint)
	}
	if settings.ServiceName != "cliproxy-test" {
		t.Fatalf("service name = %q", settings.ServiceName)
	}
	if settings.Environment != "test" {
		t.Fatalf("environment = %q", settings.Environment)
	}
	if !settings.TransportLogs {
		t.Fatal("transport logs not enabled from env")
	}
	if !settings.TransportLogsFullBody {
		t.Fatal("transport full body logs not enabled from env")
	}
	if got := settings.Headers["Authorization"]; got != "Bearer secret" {
		t.Fatalf("authorization exporter header = %q, want raw credential", got)
	}
	if got := settings.RedactedHeaders["Authorization"]; got != "<redacted>" {
		t.Fatalf("redacted authorization header = %q, want redacted", got)
	}
	if got := settings.RedactedHeaders["CF-Access-Client-Id"]; got != "id" {
		t.Fatalf("redacted safe header = %q, want id", got)
	}
}

func TestNormalizeDisablesFullBodyWhenTransportLogsOff(t *testing.T) {
	settings := Normalize(&config.Config{
		Observability: config.ObservabilityConfig{
			TransportLogs:         false,
			TransportLogsFullBody: true,
		},
	})
	if settings.TransportLogs {
		t.Fatal("transport logs should stay disabled")
	}
	if settings.TransportLogsFullBody {
		t.Fatal("full-body transport logs must be disabled when transport logs are off")
	}
}

func TestSignalEndpointAppendsPathForBaseEndpoint(t *testing.T) {
	if got := traceEndpoint("https://signoz.example.com"); got != "https://signoz.example.com/v1/traces" {
		t.Fatalf("trace endpoint = %q", got)
	}
	if got := traceEndpoint("https://signoz.example.com/v1/traces"); got != "https://signoz.example.com/v1/traces" {
		t.Fatalf("trace endpoint with path = %q", got)
	}
}

func TestRedactHeaders(t *testing.T) {
	headers := redactHeaders(map[string]string{
		"authorization": "Bearer secret",
		"x-safe":        "ok",
		"api-key":       "secret",
	})
	if headers["authorization"] != "<redacted>" {
		t.Fatalf("authorization not redacted: %q", headers["authorization"])
	}
	if headers["api-key"] != "<redacted>" {
		t.Fatalf("api-key not redacted: %q", headers["api-key"])
	}
	if headers["x-safe"] != "ok" {
		t.Fatalf("safe header changed: %q", headers["x-safe"])
	}
}

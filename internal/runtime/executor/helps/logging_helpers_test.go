package helps

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/observability"
)

func TestRecordAPIResponseMetadataStoresHeadersWhenRequestLogDisabled(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	headers := http.Header{}
	headers.Add("X-Upstream-Request-Id", "upstream-req-1")

	RecordAPIResponseMetadata(ctx, &config.Config{}, http.StatusOK, headers)
	headers.Set("X-Upstream-Request-Id", "mutated")

	got := logging.GetResponseHeaders(ctx)
	if got.Get("X-Upstream-Request-Id") != "upstream-req-1" {
		t.Fatalf("response header = %q, want %q", got.Get("X-Upstream-Request-Id"), "upstream-req-1")
	}
}

func TestRecordTransportSummaryUsesObservabilityEnvOverrides(t *testing.T) {
	// Transport-summary capture is now gated on the LIVE observability state
	// (a cheap atomic read), not on a per-call Normalize(cfg) env lookup, so
	// the streaming hot path doesn't re-parse env / allocate maps per chunk.
	// Drive the active state directly instead of via env vars.
	observability.SetActive(observability.NewSettingsOnlyStateForTest(true, true))
	t.Cleanup(func() { observability.SetActive(nil) })

	ctx := logging.WithRequestSummaryHolder(context.Background())
	cfg := &config.Config{}

	RecordAPIRequest(ctx, cfg, UpstreamRequestLog{
		URL:      "https://api.openai.com/v1/chat/completions",
		Provider: "openai",
		Body:     []byte(`{"model":"gpt-5.5","authorization":"Bearer sk-secret","messages":[{"role":"user","content":"hi"}]}`),
	})
	AppendAPIResponseChunk(ctx, cfg, []byte(`{"token":"sk-abc def","output":"ok"}`))

	summary := logging.GetRequestSummary(ctx)
	if summary.Provider != "openai" {
		t.Fatalf("provider = %q, want openai", summary.Provider)
	}
	if summary.EndpointFamily != "openai" {
		t.Fatalf("endpoint family = %q, want openai", summary.EndpointFamily)
	}
	if summary.RequestBytes == 0 || summary.ResponseBytes == 0 {
		t.Fatalf("expected request/response bytes to be captured, got request=%d response=%d", summary.RequestBytes, summary.ResponseBytes)
	}
	for _, leaked := range []string{"sk-secret", "sk-abc", "def"} {
		if strings.Contains(summary.RequestBody, leaked) || strings.Contains(summary.ResponseBody, leaked) {
			t.Fatalf("transport summary leaked %q: request=%q response=%q", leaked, summary.RequestBody, summary.ResponseBody)
		}
	}
}

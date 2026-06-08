package observability

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type proxyMetrics struct {
	requests            metric.Int64Counter
	durationMs          metric.Int64Histogram
	upstreamCalls       metric.Int64Counter
	upstreamTTFTMs      metric.Int64Histogram
	inputTokens         metric.Int64Counter
	outputTokens        metric.Int64Counter
	totalTokens         metric.Int64Counter
	cacheReadTokens     metric.Int64Counter
	cacheCreationTokens metric.Int64Counter
	quotaUtilization    metric.Float64Gauge
	activeRequests      metric.Int64UpDownCounter
	activeStreams       metric.Int64UpDownCounter
	wsConnections       metric.Int64Counter
	wsDisconnects       metric.Int64Counter
	configReloads       metric.Int64Counter
	authRefreshes       metric.Int64Counter
}

var metrics atomic.Pointer[proxyMetrics]

func setProxyMetrics(next proxyMetrics) {
	metrics.Store(&next)
}

func currentMetrics() *proxyMetrics {
	return metrics.Load()
}

func newProxyMetrics() proxyMetrics {
	meter := otel.Meter(instrumentationName)
	requests, _ := meter.Int64Counter("cliproxy.http.server.requests", metric.WithDescription("Inbound proxy HTTP requests"))
	durationMs, _ := meter.Int64Histogram("cliproxy.http.server.duration_ms", metric.WithUnit("ms"), metric.WithDescription("Inbound proxy request duration"))
	upstreamCalls, _ := meter.Int64Counter("cliproxy.upstream.requests", metric.WithDescription("Upstream provider requests"))
	upstreamTTFTMs, _ := meter.Int64Histogram("cliproxy.upstream.ttft_ms", metric.WithUnit("ms"), metric.WithDescription("Upstream time to first byte"))
	inputTokens, _ := meter.Int64Counter("cliproxy.tokens.input", metric.WithDescription("Input tokens reported by upstream providers"))
	outputTokens, _ := meter.Int64Counter("cliproxy.tokens.output", metric.WithDescription("Output tokens reported by upstream providers"))
	totalTokens, _ := meter.Int64Counter("cliproxy.tokens.total", metric.WithDescription("Total tokens reported by upstream providers"))
	cacheReadTokens, _ := meter.Int64Counter("cliproxy.tokens.cache_read", metric.WithDescription("Cache-read input tokens reported by upstream providers"))
	cacheCreationTokens, _ := meter.Int64Counter("cliproxy.tokens.cache_creation", metric.WithDescription("Cache-creation input tokens reported by upstream providers"))
	quotaUtilization, _ := meter.Float64Gauge("cliproxy.quota.utilization", metric.WithDescription("Provider-reported quota utilization (0.0-1.0) for a credential and limit window"))
	activeRequests, _ := meter.Int64UpDownCounter("cliproxy.http.server.active_requests", metric.WithDescription("Active inbound requests"))
	activeStreams, _ := meter.Int64UpDownCounter("cliproxy.streams.active", metric.WithDescription("Active streaming or websocket requests"))
	wsConnections, _ := meter.Int64Counter("cliproxy.websocket.connections", metric.WithDescription("Websocket connections"))
	wsDisconnects, _ := meter.Int64Counter("cliproxy.websocket.disconnects", metric.WithDescription("Websocket disconnects"))
	configReloads, _ := meter.Int64Counter("cliproxy.config.reloads", metric.WithDescription("Configuration reload attempts"))
	authRefreshes, _ := meter.Int64Counter("cliproxy.auth.refreshes", metric.WithDescription("Auth refresh events"))
	return proxyMetrics{
		requests:            requests,
		durationMs:          durationMs,
		upstreamCalls:       upstreamCalls,
		upstreamTTFTMs:      upstreamTTFTMs,
		inputTokens:         inputTokens,
		outputTokens:        outputTokens,
		totalTokens:         totalTokens,
		cacheReadTokens:     cacheReadTokens,
		cacheCreationTokens: cacheCreationTokens,
		quotaUtilization:    quotaUtilization,
		activeRequests:      activeRequests,
		activeStreams:       activeStreams,
		wsConnections:       wsConnections,
		wsDisconnects:       wsDisconnects,
		configReloads:       configReloads,
		authRefreshes:       authRefreshes,
	}
}

func RecordHTTPRequest(ctx context.Context, method, route, family string, status int, duration time.Duration, streaming bool) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("http.route", route),
		attribute.String("cliproxy.endpoint_family", family),
		attribute.Int("http.response.status_code", status),
		attribute.Bool("cliproxy.streaming", streaming),
	}
	m.requests.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.durationMs.Record(ctx, duration.Milliseconds(), metric.WithAttributes(attrs...))
}

func AddActiveRequest(ctx context.Context, delta int64, streaming bool) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	m.activeRequests.Add(ctx, delta)
	if streaming {
		m.activeStreams.Add(ctx, delta)
	}
}

func RecordUsage(ctx context.Context, record coreusage.Record) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	failed := record.Failed
	if !failed {
		status := logging.GetResponseStatus(ctx)
		failed = status >= http.StatusBadRequest
	}
	// conversation_id and prompt_cache_key are deliberately NOT metric
	// dimensions: they are unbounded per-request identifiers that explode
	// metric cardinality. Past WithCardinalityLimit(2000) the SDK dumps
	// everything into an overflow bucket, making per-model/provider token
	// aggregates inaccurate. They live on the server span event
	// (usage_plugin.go) and the transport-summary log (transport_logs.go),
	// which is the correct place for high-cardinality correlation IDs.
	attrs := []attribute.KeyValue{
		attribute.String("cliproxy.provider", emptyDefault(record.Provider, "unknown")),
		attribute.String("cliproxy.model", emptyDefault(record.Model, "unknown")),
		attribute.String("cliproxy.requested_model", emptyDefault(record.Alias, record.Model)),
		attribute.String("cliproxy.auth_type", emptyDefault(record.AuthType, "unknown")),
		attribute.String("cliproxy.auth_index", emptyDefault(record.AuthIndex, "unknown")),
		attribute.String("cliproxy.api_key_fingerprint", emptyDefault(record.APIKey, "unknown")),
		attribute.Bool("cliproxy.failed", failed),
	}
	m.upstreamCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
	if record.TTFT > 0 {
		m.upstreamTTFTMs.Record(ctx, record.TTFT.Milliseconds(), metric.WithAttributes(attrs...))
	}
	if record.Detail.InputTokens > 0 {
		m.inputTokens.Add(ctx, record.Detail.InputTokens, metric.WithAttributes(attrs...))
	}
	if record.Detail.OutputTokens > 0 {
		m.outputTokens.Add(ctx, record.Detail.OutputTokens, metric.WithAttributes(attrs...))
	}
	total := record.Detail.TotalTokens
	if total == 0 {
		total = record.Detail.InputTokens + record.Detail.OutputTokens + record.Detail.ReasoningTokens
	}
	if total > 0 {
		m.totalTokens.Add(ctx, total, metric.WithAttributes(attrs...))
	}
	if record.Detail.CacheReadTokens > 0 {
		m.cacheReadTokens.Add(ctx, record.Detail.CacheReadTokens, metric.WithAttributes(attrs...))
	}
	if record.Detail.CacheCreationTokens > 0 {
		m.cacheCreationTokens.Add(ctx, record.Detail.CacheCreationTokens, metric.WithAttributes(attrs...))
	}
}

// RecordQuotaUtilization emits the provider-reported quota utilization gauge
// (0.0-1.0) for a single credential and limit window. provider is the upstream
// identifier (e.g. "claude", "codex"); authIndex/authID identify the credential
// so per-account burn can be tracked; window is the limit window the value
// applies to (e.g. "5h", "7d", "primary", "weekly"); status is the
// provider-reported limiter status (e.g. "allowed"), empty when unknown.
//
// Utilization is a point-in-time level, so it is modeled as a gauge: each call
// overwrites the last value for the matching attribute set. Values outside
// [0,1] are ignored to avoid polluting the series with parse garbage.
func RecordQuotaUtilization(ctx context.Context, provider, authIndex, authID, window, status string, utilization float64) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	if utilization < 0 || utilization > 1 {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("cliproxy.provider", emptyDefault(provider, "unknown")),
		attribute.String("cliproxy.auth_index", emptyDefault(authIndex, "unknown")),
		attribute.String("cliproxy.auth_id", emptyDefault(authID, "unknown")),
		attribute.String("cliproxy.quota_window", emptyDefault(window, "unknown")),
		attribute.String("cliproxy.quota_status", emptyDefault(status, "unknown")),
	}
	m.quotaUtilization.Record(ctx, utilization, metric.WithAttributes(attrs...))
}

// RecordQuotaFromHeaders extracts provider-reported quota utilization from an
// upstream response's headers and emits the gauge for each window it finds.
// It is a no-op when observability is disabled, headers is nil, or the provider
// does not expose a continuous utilization signal. Currently supported:
//
//   - claude: Anthropic-Ratelimit-Unified-{5h,7d}-Utilization (0.0-1.0) plus the
//     matching -Status header.
//   - codex:  x-codex-primary-used-percent and
//     x-codex-secondary-primary-used-percent (0-100, normalized to 0.0-1.0).
//
// Other providers (xai, antigravity, gemini) do not expose a clean continuous
// utilization header and are intentionally skipped.
func RecordQuotaFromHeaders(ctx context.Context, provider, authIndex, authID string, headers http.Header) {
	if !Enabled() || headers == nil {
		return
	}
	for _, sample := range parseQuotaSamples(provider, headers) {
		RecordQuotaUtilization(ctx, provider, authIndex, authID, sample.window, sample.status, sample.utilization)
	}
}

// quotaSample is one parsed (window, utilization, status) reading extracted from
// an upstream response's headers.
type quotaSample struct {
	window      string
	utilization float64
	status      string
}

// parseQuotaSamples extracts provider-reported quota utilization readings from
// response headers. It is pure (no metric side effects) so it can be unit
// tested independently of the OTel pipeline. Unknown providers and unparseable
// values yield no samples.
func parseQuotaSamples(provider string, headers http.Header) []quotaSample {
	if headers == nil {
		return nil
	}
	var samples []quotaSample
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "anthropic":
		for _, window := range []string{"5h", "7d"} {
			raw := strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-" + window + "-Utilization"))
			if raw == "" {
				continue
			}
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil || value < 0 || value > 1 {
				continue
			}
			status := strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-" + window + "-Status"))
			samples = append(samples, quotaSample{window: window, utilization: value, status: status})
		}
	case "codex", "openai":
		// Codex reports used-percent on a 0-100 scale; normalize to 0.0-1.0 to
		// match the Anthropic utilization convention so both providers share one
		// gauge series.
		codexWindows := []struct{ window, header string }{
			{"primary", "x-codex-primary-used-percent"},
			{"weekly", "x-codex-secondary-primary-used-percent"},
		}
		for _, w := range codexWindows {
			raw := strings.TrimSpace(headers.Get(w.header))
			if raw == "" {
				continue
			}
			percent, err := strconv.ParseFloat(raw, 64)
			if err != nil || percent < 0 || percent > 100 {
				continue
			}
			samples = append(samples, quotaSample{window: w.window, utilization: percent / 100.0})
		}
	}
	return samples
}

func RecordWebsocketConnect(ctx context.Context, provider string) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	m.wsConnections.Add(ctx, 1, metric.WithAttributes(attribute.String("cliproxy.provider", emptyDefault(provider, "unknown"))))
}

func RecordWebsocketDisconnect(ctx context.Context, provider string, reason error) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("cliproxy.provider", emptyDefault(provider, "unknown")),
		attribute.Bool("cliproxy.error", reason != nil),
	}
	m.wsDisconnects.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func RecordConfigReload(ctx context.Context, success bool, duration time.Duration) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	m.configReloads.Add(ctx, 1, metric.WithAttributes(
		attribute.Bool("cliproxy.success", success),
		attribute.Int64("cliproxy.duration_ms", duration.Milliseconds()),
	))
}

func RecordAuthRefresh(ctx context.Context, success bool, provider string) {
	if !Enabled() {
		return
	}
	m := currentMetrics()
	if m == nil {
		return
	}
	m.authRefreshes.Add(ctx, 1, metric.WithAttributes(
		attribute.Bool("cliproxy.success", success),
		attribute.String("cliproxy.provider", emptyDefault(provider, "unknown")),
	))
}

func emptyDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		return fallback
	}
	return "unknown"
}

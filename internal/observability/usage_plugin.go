package observability

import (
	"context"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type UsagePlugin struct{}

var registerUsagePluginOnce sync.Once

func RegisterUsagePlugin() {
	registerUsagePluginOnce.Do(func() {
		coreusage.RegisterPlugin(NewUsagePlugin())
	})
}

func NewUsagePlugin() *UsagePlugin { return &UsagePlugin{} }

func (p *UsagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !Enabled() {
		return
	}
	RecordUsage(ctx, record)
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	cacheIdentity := logging.GetCacheIdentity(ctx)
	span.AddEvent("cliproxy.usage", trace.WithAttributes(
		attribute.String("cliproxy.provider", emptyDefault(record.Provider, "unknown")),
		attribute.String("cliproxy.model", emptyDefault(record.Model, "unknown")),
		attribute.String("cliproxy.requested_model", emptyDefault(record.Alias, record.Model)),
		attribute.String("cliproxy.auth_type", emptyDefault(record.AuthType, "unknown")),
		attribute.String("cliproxy.auth_index", emptyDefault(record.AuthIndex, "unknown")),
		attribute.String("cliproxy.api_key_fingerprint", emptyDefault(record.APIKey, "unknown")),
		attribute.String("cliproxy.conversation_id", emptyDefault(cacheIdentity.ConversationID, "unknown")),
		attribute.String("cliproxy.prompt_cache_key", emptyDefault(cacheIdentity.PromptCacheKey, "unknown")),
		attribute.Int64("gen_ai.usage.input_tokens", record.Detail.InputTokens),
		attribute.Int64("gen_ai.usage.output_tokens", record.Detail.OutputTokens),
		attribute.Int64("gen_ai.usage.cache_read_input_tokens", record.Detail.CacheReadTokens),
		attribute.Int64("gen_ai.usage.cache_creation_input_tokens", record.Detail.CacheCreationTokens),
		attribute.Float64("cliproxy.cache_hit_ratio", cacheHitRatio(record.Detail)),
		attribute.Int64("llm.usage.total_tokens", totalTokens(record.Detail)),
		attribute.Int64("cliproxy.ttft_ms", record.TTFT.Milliseconds()),
		attribute.Bool("cliproxy.failed", record.Failed),
	))
}

func totalTokens(detail coreusage.Detail) int64 {
	if detail.TotalTokens != 0 {
		return detail.TotalTokens
	}
	return detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
}

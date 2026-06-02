package observability

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

type RequestSummary struct {
	Model               string
	RequestedModel      string
	EndpointFamily      string
	Provider            string
	Client              string
	ConversationID      string
	PromptCacheKey      string
	Status              int
	TTFTMs              int64
	LatencyMs           int64
	RequestBytes        int64
	ResponseBytes       int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CacheHitRatio       float64
	CacheControlSummary string
	Failed              bool
	ClientCanceled      bool
	ErrorStatus         int
	ErrorMessage        string
	RequestBody         string
	ResponseBody        string
}

func RecordRequestSummary(ctx context.Context, summary RequestSummary) {
	state := active()
	if state == nil || !state.settings.TransportLogs {
		return
	}
	record := otellog.Record{}
	now := time.Now()
	record.SetTimestamp(now)
	record.SetObservedTimestamp(now)
	switch {
	case summary.ClientCanceled:
		record.SetSeverity(otellog.SeverityInfo)
		record.SetSeverityText("INFO")
	case summary.Failed:
		record.SetSeverity(otellog.SeverityError)
		record.SetSeverityText("ERROR")
	default:
		record.SetSeverity(otellog.SeverityInfo)
		record.SetSeverityText("INFO")
	}
	record.SetBody(otellog.StringValue("cliproxy.transport_summary"))
	record.AddAttributes(
		otellog.String("cliproxy.model", emptyDefault(summary.Model, "unknown")),
		otellog.String("cliproxy.requested_model", emptyDefault(summary.RequestedModel, summary.Model)),
		otellog.String("cliproxy.endpoint_family", emptyDefault(summary.EndpointFamily, "unknown")),
		otellog.String("cliproxy.provider", emptyDefault(summary.Provider, "unknown")),
		otellog.String("cliproxy.conversation_id", emptyDefault(summary.ConversationID, "unknown")),
		otellog.String("cliproxy.prompt_cache_key", emptyDefault(summary.PromptCacheKey, "unknown")),
		otellog.Int("http.response.status_code", summary.Status),
		otellog.Int64("cliproxy.ttft_ms", summary.TTFTMs),
		otellog.Int64("cliproxy.latency_ms", summary.LatencyMs),
		otellog.Int64("cliproxy.request_bytes", summary.RequestBytes),
		otellog.Int64("cliproxy.response_bytes", summary.ResponseBytes),
		otellog.Int64("gen_ai.usage.input_tokens", summary.InputTokens),
		otellog.Int64("gen_ai.usage.output_tokens", summary.OutputTokens),
		otellog.Int64("gen_ai.usage.cache_read_input_tokens", summary.CacheReadTokens),
		otellog.Int64("gen_ai.usage.cache_creation_input_tokens", summary.CacheCreationTokens),
		otellog.Float64("cliproxy.cache_hit_ratio", summary.CacheHitRatio),
		otellog.String("cliproxy.cache_control_summary", emptyDefault(summary.CacheControlSummary, "none")),
		otellog.Bool("cliproxy.failed", summary.Failed),
		otellog.Bool("cliproxy.client_canceled", summary.ClientCanceled),
		otellog.Int("cliproxy.error_status", summary.ErrorStatus),
	)
	if summary.Client != "" {
		record.AddAttributes(otellog.String("cliproxy.client", summary.Client))
	}
	if summary.ErrorMessage != "" {
		record.AddAttributes(otellog.String("cliproxy.error_message", RedactStringForLog(summary.ErrorMessage, 512)))
	}
	if state.settings.ServiceName != "" {
		record.AddAttributes(otellog.String("service.name", state.settings.ServiceName))
	}
	if state.settings.Environment != "" {
		record.AddAttributes(otellog.String("deployment.environment", state.settings.Environment))
	}
	for key, value := range state.settings.ResourceAttributes {
		if key == "" || value == "" {
			continue
		}
		record.AddAttributes(otellog.String(key, value))
	}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		record.AddAttributes(
			otellog.String("trace_id", spanContext.TraceID().String()),
			otellog.String("span_id", spanContext.SpanID().String()),
		)
	}
	if state.settings.TransportLogsFullBody {
		record.AddAttributes(
			otellog.String("cliproxy.request_body_redacted", boundedString(summary.RequestBody, 8192)),
			otellog.String("cliproxy.response_body_redacted", boundedString(summary.ResponseBody, 8192)),
		)
	}
	AnnotateServerSpanFromSummary(ctx, summary)
	Logger().Emit(ctx, record)
}

func SummaryFromUsage(ctx context.Context, record coreusage.Record, request logging.RequestSummary) RequestSummary {
	identity := logging.GetCacheIdentity(ctx)
	outcome := logging.GetRequestOutcome(ctx)
	failed := record.Failed || outcome.Failed
	status := logging.GetResponseStatus(ctx)
	if status == 0 {
		status = request.Status
	}
	if !failed && status >= http.StatusBadRequest {
		failed = true
	}
	if outcome.ClientCanceled {
		failed = false
	}
	errorStatus := 0
	errorMessage := ""
	if failed {
		switch {
		case record.Fail.StatusCode > 0:
			errorStatus = record.Fail.StatusCode
		case outcome.ErrorStatus > 0:
			errorStatus = outcome.ErrorStatus
		case status >= http.StatusBadRequest:
			errorStatus = status
		default:
			errorStatus = http.StatusBadGateway
		}
		errorMessage = firstNonEmpty(record.Fail.Body, outcome.ErrorMessage)
		if status < http.StatusBadRequest && errorStatus >= http.StatusBadRequest {
			status = errorStatus
		}
	}
	return RequestSummary{
		Model:               firstNonEmpty(record.Model, request.Model, outcome.Model),
		RequestedModel:      firstNonEmpty(record.Alias, request.RequestedModel, outcome.RequestedModel),
		EndpointFamily:      request.EndpointFamily,
		Provider:            firstNonEmpty(record.Provider, request.Provider, outcome.Provider),
		Client:              outcome.Client,
		ClientCanceled:      outcome.ClientCanceled,
		ConversationID:      identity.ConversationID,
		PromptCacheKey:      identity.PromptCacheKey,
		Status:              status,
		TTFTMs:              record.TTFT.Milliseconds(),
		LatencyMs:           record.Latency.Milliseconds(),
		RequestBytes:        request.RequestBytes,
		ResponseBytes:       request.ResponseBytes,
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		CacheHitRatio:       cacheHitRatio(record.Detail),
		CacheControlSummary: request.CacheControlSummary,
		Failed:              failed,
		ErrorStatus:         errorStatus,
		ErrorMessage:        errorMessage,
		RequestBody:         request.RequestBody,
		ResponseBody:        request.ResponseBody,
	}
}

func AnnotateServerSpanFromSummary(ctx context.Context, summary RequestSummary) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attribute.String("cliproxy.model", emptyDefault(summary.Model, "unknown")),
		attribute.String("cliproxy.requested_model", emptyDefault(summary.RequestedModel, summary.Model)),
		attribute.String("cliproxy.endpoint_family", emptyDefault(summary.EndpointFamily, "unknown")),
		attribute.String("cliproxy.provider", emptyDefault(summary.Provider, "unknown")),
		attribute.String("cliproxy.conversation_id", emptyDefault(summary.ConversationID, "unknown")),
		attribute.String("cliproxy.prompt_cache_key", emptyDefault(summary.PromptCacheKey, "unknown")),
		attribute.Bool("cliproxy.failed", summary.Failed),
	)
	if summary.ClientCanceled {
		span.SetAttributes(attribute.Bool("cliproxy.client_canceled", true))
		return
	}
	if summary.Failed {
		// Redact the upstream error before it lands on the span via
		// RecordError/SetStatus — both export to SigNoz traces and the raw
		// upstream body can echo secrets.
		message := RedactStringForLog(strings.TrimSpace(summary.ErrorMessage), 512)
		if message == "" {
			message = http.StatusText(summary.ErrorStatus)
		}
		if message == "" {
			message = "upstream request failed"
		}
		span.RecordError(httpStatusError{status: summary.ErrorStatus, message: message})
		span.SetStatus(codes.Error, message)
	}
}

type httpStatusError struct {
	status  int
	message string
}

func (e httpStatusError) Error() string {
	if e.message != "" {
		return e.message
	}
	if text := http.StatusText(e.status); text != "" {
		return text
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

func cacheHitRatio(detail coreusage.Detail) float64 {
	total := detail.InputTokens + detail.CacheReadTokens + detail.CacheCreationTokens
	if total <= 0 {
		return 0
	}
	return float64(detail.CacheReadTokens) / float64(total)
}

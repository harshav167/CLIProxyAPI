package observability

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type UpstreamAttributes struct {
	Provider        string
	Model           string
	RequestedModel  string
	AuthType        string
	AuthIndex       string
	AuthFingerprint string
	ReasoningEffort string
	ServiceTier     string
}

func WrapTransport(base http.RoundTripper, attrs UpstreamAttributes) http.RoundTripper {
	if !Enabled() {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	// Nest the annotation transport INSIDE otelhttp. otelhttp.NewTransport
	// creates the client span and injects it onto the request context before
	// it calls its wrapped RoundTripper. By making annotateTransport that
	// wrapped RoundTripper, our SetAttributes/RecordError run with
	// req.Context() already carrying the otelhttp client span — so the
	// provider/model attributes and error status land on the upstream client
	// span, not the parent. (Previously the caller invoked
	// AnnotateUpstreamResponse with the OUTER request whose context never saw
	// otelhttp's child span, so the attributes were written to the wrong span
	// or dropped entirely.)
	annotated := annotateTransport{base: base, attrs: attrs}
	return otelhttp.NewTransport(
		annotated,
		otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
			if req == nil || req.URL == nil {
				return "upstream request"
			}
			return "upstream " + req.Method + " " + req.URL.Host
		}),
		otelhttp.WithSpanOptions(trace.WithSpanKind(trace.SpanKindClient)),
		otelhttp.WithMetricAttributesFn(func(req *http.Request) []attribute.KeyValue {
			return upstreamAttributeList(req, attrs)
		}),
	)
}

// annotateTransport runs as otelhttp's inner RoundTripper, so by the time its
// RoundTrip executes the request context already carries the otelhttp client
// span. It stamps provider/model attributes and failure status onto that span.
type annotateTransport struct {
	base  http.RoundTripper
	attrs UpstreamAttributes
}

func (t annotateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	AnnotateUpstreamResponse(req, resp, err, t.attrs)
	return resp, err
}

func AnnotateUpstreamResponse(req *http.Request, resp *http.Response, err error, attrs UpstreamAttributes) {
	if req == nil || !Enabled() {
		return
	}
	ctx := req.Context()

	// Failure correlation (identity + outcome) must run UNCONDITIONALLY —
	// it feeds the transport-summary log and the server-span annotation that
	// happen later, and those are needed even when this particular client
	// span is not sampled/recording. Do it before the recording check.
	logging.SetRequestIdentity(ctx, attrs.Provider, attrs.Model, attrs.RequestedModel, attrs.AuthFingerprint)

	clientCanceled := false
	if err != nil {
		if isClientCancellation(ctx, err) {
			clientCanceled = true
		} else {
			statusCode := 0
			if resp != nil {
				statusCode = resp.StatusCode
			}
			logging.SetRequestOutcome(ctx, true, statusCode, err.Error())
		}
	} else if resp != nil && resp.StatusCode >= http.StatusInternalServerError {
		logging.SetRequestOutcome(ctx, true, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Span annotation only when the client span is actually recording.
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(upstreamAttributeList(req, attrs)...)

	if resp != nil {
		span.SetAttributes(
			attribute.Int("http.response.status_code", resp.StatusCode),
			attribute.Int("cliproxy.upstream.status_code", resp.StatusCode),
		)
		if resp.StatusCode >= http.StatusInternalServerError {
			msg := http.StatusText(resp.StatusCode)
			span.RecordError(httpStatusError{status: resp.StatusCode, message: msg})
			span.SetStatus(codes.Error, msg)
		}
	}

	if err != nil {
		if clientCanceled {
			span.SetAttributes(attribute.Bool("cliproxy.client_canceled", true))
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// isClientCancellation reports downstream disconnects separately from upstream
// transport failures.
func isClientCancellation(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if ctx == nil {
		return true
	}
	return errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func upstreamAttributeList(req *http.Request, attrs UpstreamAttributes) []attribute.KeyValue {
	values := []attribute.KeyValue{
		attribute.String("cliproxy.provider", emptyDefault(attrs.Provider, "unknown")),
		attribute.String("cliproxy.model", emptyDefault(attrs.Model, "unknown")),
		attribute.String("cliproxy.requested_model", emptyDefault(attrs.RequestedModel, attrs.Model)),
		attribute.String("cliproxy.auth_type", emptyDefault(attrs.AuthType, "unknown")),
		attribute.String("cliproxy.auth_index", emptyDefault(attrs.AuthIndex, "unknown")),
		attribute.String("cliproxy.api_key_fingerprint", emptyDefault(attrs.AuthFingerprint, "unknown")),
		attribute.String("cliproxy.reasoning_effort", emptyDefault(attrs.ReasoningEffort, "unknown")),
		attribute.String("cliproxy.service_tier", emptyDefault(attrs.ServiceTier, "default")),
		attribute.String("gen_ai.system", genAISystem(attrs.Provider)),
		attribute.String("gen_ai.request.model", emptyDefault(attrs.RequestedModel, attrs.Model)),
	}
	if req != nil && req.URL != nil {
		values = append(values,
			attribute.String("server.address", req.URL.Hostname()),
			attribute.String("url.scheme", req.URL.Scheme),
			attribute.String("http.request.method", req.Method),
		)
	}
	return values
}

func genAISystem(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch {
	case strings.Contains(provider, "claude"), strings.Contains(provider, "anthropic"):
		return "anthropic"
	case strings.Contains(provider, "gemini"), strings.Contains(provider, "aistudio"), strings.Contains(provider, "vertex"):
		return "gemini"
	case strings.Contains(provider, "codex"), strings.Contains(provider, "openai"):
		return "openai"
	case strings.Contains(provider, "xai"), strings.Contains(provider, "grok"):
		return "xai"
	default:
		return emptyDefault(provider, "unknown")
	}
}

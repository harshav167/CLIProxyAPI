package observability

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// GinMiddleware records route-aware inbound HTTP spans without inspecting
// request or response bodies.
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Enabled() {
			c.Next()
			return
		}
		start := time.Now()
		path := c.FullPath()
		if path == "" && c.Request != nil && c.Request.URL != nil {
			path = c.Request.URL.Path
		}
		family := endpointFamily(path)
		streaming := isStreamingRequest(c)
		ctx, span := Tracer().Start(
			c.Request.Context(),
			"http "+routeName(c.Request.Method, path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", path),
				attribute.String("cliproxy.endpoint_family", family),
				attribute.Bool("cliproxy.streaming", streaming),
			),
		)
		if requestID := logging.GetGinRequestID(c); requestID != "" {
			span.SetAttributes(attribute.String("cliproxy.request_id", requestID))
		}
		if c.Request != nil {
			if ua := strings.TrimSpace(c.Request.UserAgent()); ua != "" {
				span.SetAttributes(attribute.String("user_agent.original", ua))
			}
			if addr := strings.TrimSpace(c.ClientIP()); addr != "" {
				span.SetAttributes(attribute.String("client.address", addr))
			}
			client := strings.TrimSpace(c.GetHeader("X-Client"))
			if client == "" {
				if ua := strings.TrimSpace(c.Request.UserAgent()); ua != "" {
					client = ua
				}
			}
			if client != "" {
				span.SetAttributes(attribute.String("cliproxy.client", client))
				logging.SetRequestClient(ctx, client)
			}
		}
		c.Request = c.Request.WithContext(ctx)
		AddActiveRequest(ctx, 1, streaming)
		defer func() {
			status := c.Writer.Status()
			duration := time.Since(start)
			requestSummary := logging.GetRequestSummary(ctx)
			outcome := logging.GetRequestOutcome(ctx)

			model := firstNonEmpty(requestSummary.Model, outcome.Model)
			requestedModel := firstNonEmpty(requestSummary.RequestedModel, outcome.RequestedModel, model)
			provider := firstNonEmpty(requestSummary.Provider, outcome.Provider)
			failed := (outcome.Failed || status >= http.StatusInternalServerError) && !outcome.ClientCanceled

			span.SetAttributes(
				attribute.Int("http.response.status_code", status),
				attribute.Int64("cliproxy.duration_ms", duration.Milliseconds()),
				attribute.String("cliproxy.model", emptyDefault(model, "unknown")),
				attribute.String("cliproxy.requested_model", emptyDefault(requestedModel, model)),
				attribute.String("cliproxy.provider", emptyDefault(provider, "unknown")),
				attribute.String("cliproxy.endpoint_family", emptyDefault(requestSummary.EndpointFamily, family)),
				attribute.Bool("cliproxy.failed", failed),
			)
			if outcome.AuthFingerprint != "" {
				span.SetAttributes(attribute.String("cliproxy.api_key_fingerprint", outcome.AuthFingerprint))
			}
			if outcome.Client != "" {
				span.SetAttributes(attribute.String("cliproxy.client", outcome.Client))
			}
			if outcome.ErrorStatus > 0 {
				span.SetAttributes(attribute.Int("cliproxy.error_status", outcome.ErrorStatus))
			}
			if outcome.ErrorMessage != "" {
				span.SetAttributes(attribute.String("cliproxy.error_message", outcome.ErrorMessage))
			}
			if outcome.ClientCanceled {
				span.SetAttributes(attribute.Bool("cliproxy.client_canceled", true))
			}

			switch {
			case outcome.ClientCanceled:
				// Cancellation by the caller is not a proxy or provider failure.
			case len(c.Errors) > 0:
				span.RecordError(httpStatusError{status: status, message: c.Errors.String()})
				span.SetStatus(codes.Error, c.Errors.String())
			case outcome.Failed:
				message := strings.TrimSpace(outcome.ErrorMessage)
				if message == "" {
					message = http.StatusText(outcome.ErrorStatus)
				}
				if message == "" {
					message = "upstream request failed"
				}
				span.RecordError(httpStatusError{status: outcome.ErrorStatus, message: message})
				span.SetStatus(codes.Error, message)
			case status >= http.StatusInternalServerError:
				message := http.StatusText(status)
				span.RecordError(httpStatusError{status: status, message: message})
				span.SetStatus(codes.Error, message)
			}
			RecordHTTPRequest(ctx, c.Request.Method, path, family, status, duration, streaming)
			AddActiveRequest(ctx, -1, streaming)
			span.End()
		}()
		c.Next()
	}
}

func routeName(method, route string) string {
	method = strings.TrimSpace(method)
	route = strings.TrimSpace(route)
	if route == "" {
		route = "unknown"
	}
	if method == "" {
		return route
	}
	return method + " " + route
}

func endpointFamily(path string) string {
	switch {
	case path == "/healthz":
		return "health"
	case strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management"):
		return "management"
	case strings.Contains(path, "callback"):
		return "oauth"
	case strings.HasPrefix(path, "/v1beta"):
		return "gemini"
	case strings.HasPrefix(path, "/v1/messages"):
		return "claude"
	case strings.HasPrefix(path, "/v1/responses") || strings.Contains(path, "/responses"):
		return "responses"
	case strings.HasPrefix(path, "/api/provider"):
		return "amp"
	case strings.HasPrefix(path, "/v1/ws"):
		return "websocket"
	case strings.HasPrefix(path, "/v1"):
		return "openai"
	default:
		return "other"
	}
}

func isStreamingRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(c.Request.Header.Get("Upgrade")), "websocket") {
		return true
	}
	accept := strings.ToLower(c.Request.Header.Get("Accept"))
	if strings.Contains(accept, "text/event-stream") {
		return true
	}
	return strings.Contains(strings.ToLower(c.Request.URL.RawQuery), "stream=true")
}

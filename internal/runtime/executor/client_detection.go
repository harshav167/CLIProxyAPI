package executor

import (
	"context"
	"strings"
)

// eventTypesDroidSupports enumerates the OpenAI Responses API stream event
// types that Droid's bundled openai-node SDK recognizes. Events outside this
// allowlist can crash the SDK's type-tagged union parser with a JavaScriptCore
// TypeError (e.g. "undefined is not an object (evaluating 'C.type')") because
// the parser assumes every inbound event matches one of its known discriminants.
var eventTypesDroidSupports = map[string]struct{}{
	"response.completed":                     {},
	"response.content_part.added":            {},
	"response.created":                       {},
	"response.custom_tool_call_input.delta":  {},
	"response.custom_tool_call_input.done":   {},
	"response.error":                         {},
	"response.failed":                        {},
	"response.function_call_arguments.delta": {},
	"response.function_call_arguments.done":  {},
	"response.in_progress":                   {},
	"response.incomplete":                    {},
	"response.output_item.added":             {},
	"response.output_item.done":              {},
	"response.output_text.delta":             {},
	"response.output_text.done":              {},
	"response.queued":                        {},
	"response.reasoning_summary_part.added":  {},
	"response.reasoning_summary_part.done":   {},
	"response.reasoning_summary_text.delta":  {},
	"response.reasoning_summary_text.done":   {},
	"response.reasoning_text.delta":          {},
}

// IsEventTypeSupportedByDroid reports whether an outbound WS→SSE event type
// is in Droid's openai-node allowlist. Unknown types should be dropped by the
// bridge before reaching the client.
//
// Empty eventType strings return true (forwarded) as a defensive measure.
func IsEventTypeSupportedByDroid(eventType string) bool {
	if eventType == "" {
		return true
	}
	_, ok := eventTypesDroidSupports[eventType]
	return ok
}

type clientUserAgentCtxKey struct{}

// WithClientUserAgent stores the inbound User-Agent header in ctx so downstream
// code can detect client-specific behavior. Handlers should call this once per request.
func WithClientUserAgent(ctx context.Context, ua string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientUserAgentCtxKey{}, ua)
}

func clientUserAgentFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(clientUserAgentCtxKey{}).(string)
	return v
}

// IsDroidClient reports whether the inbound client is Droid (factory-cli/*).
// Defensive default: returns true when no User-Agent was stored.
func IsDroidClient(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v := clientUserAgentFromContext(ctx)
	if v == "" {
		return true
	}
	return strings.HasPrefix(v, "factory-cli/")
}

// IsCursorClient reports whether the inbound client is Cursor (User-Agent
// prefix "Cursor/").
func IsCursorClient(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := clientUserAgentFromContext(ctx)
	if v == "" {
		return false
	}
	return strings.HasPrefix(v, "Cursor/")
}

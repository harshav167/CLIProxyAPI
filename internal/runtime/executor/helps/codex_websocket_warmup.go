package helps

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex WebSocket cache-priming warmup (v2 `generate:false` prewarm).
//
// Rationale: OpenAI's Responses-over-WebSocket "warmup" (`response.create` with
// `generate:false`) primes the connection-local, in-memory previous-response
// cache WITHOUT generating model output. The next real turn chains from the
// warmup's response id via `previous_response_id`, so the large, stable prompt
// prefix (system instructions + tool schemas) is already cached — turning a
// cold first turn (cached_tokens ~0) into a warm one (~80-90% cache hit).
//
// This mirrors codex-rs `stream_responses_websocket(warmup=true)` /
// `prewarm_websocket`: the warmup body is the SAME request body as the turn,
// with `generate:false` added. It is WebSocket-only — HTTP Responses has no
// persistent connection to prime — and best-effort: a warmup failure must never
// break the turn (the caller falls back to a normal, un-chained request).

// CodexWarmupGenerateField is the request field that suppresses model output on
// a `response.create` while still priming request/cache state.
const CodexWarmupGenerateField = "generate"

// BuildCodexWebsocketWarmupBody returns a copy of the already-translated Codex
// WebSocket request body (a `response.create` payload) with `generate:false`
// added, so upstream primes state without generating output.
//
// It returns (nil, false) when the input is empty or not usable, so the caller
// can skip warmup and proceed with the normal turn unchanged.
func BuildCodexWebsocketWarmupBody(wsReqBody []byte) ([]byte, bool) {
	if len(wsReqBody) == 0 || !gjson.ValidBytes(wsReqBody) {
		return nil, false
	}
	out, err := sjson.SetBytes(bytes.Clone(wsReqBody), CodexWarmupGenerateField, false)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// CodexResponsesEvent classifies the lifecycle state of one Responses event.
type CodexResponsesEvent struct {
	Terminal   bool
	Success    bool
	Incomplete bool
	Failure    bool

	IncompleteReason string
	ResponseID       string
}

// ClassifyCodexResponsesEvent accepts a raw JSON event or one SSE data frame.
// Unknown and malformed events remain non-terminal.
func ClassifyCodexResponsesEvent(payload []byte) CodexResponsesEvent {
	trimmed := bytes.TrimSpace(payload)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' || !gjson.ValidBytes(trimmed) {
		return CodexResponsesEvent{}
	}
	switch gjson.GetBytes(trimmed, "type").String() {
	case "response.completed", "response.done":
		return CodexResponsesEvent{Terminal: true, Success: true, ResponseID: codexResponsesEventResponseID(trimmed)}
	case "response.incomplete":
		return CodexResponsesEvent{
			Terminal:         true,
			Incomplete:       true,
			IncompleteReason: gjson.GetBytes(trimmed, "response.incomplete_details.reason").String(),
			ResponseID:       codexResponsesEventResponseID(trimmed),
		}
	case "response.failed", "response.error", "error":
		return CodexResponsesEvent{Terminal: true, Failure: true}
	default:
		return CodexResponsesEvent{}
	}
}

// IsCodexPreviousResponseNotFoundEvent reports whether a terminal failure
// indicates that the upstream connection no longer knows the chained response.
func IsCodexPreviousResponseNotFoundEvent(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if !ClassifyCodexResponsesEvent(trimmed).Failure {
		return false
	}
	for _, path := range []string{"response.error.code", "error.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(trimmed, path).String()), "previous_response_not_found") {
			return true
		}
	}
	message := strings.ToLower(string(trimmed))
	return strings.Contains(message, "previous_response_not_found") ||
		strings.Contains(message, "previous_response_id") && strings.Contains(message, "not found")
}

func codexResponsesEventResponseID(payload []byte) string {
	if id := gjson.GetBytes(payload, "response.id").String(); id != "" {
		return id
	}
	return gjson.GetBytes(payload, "id").String()
}

// ApplyCodexWebsocketPreviousResponseID sets `previous_response_id` on a Codex
// WebSocket request body so the turn chains from a prior (warmup or earlier)
// response's connection-local cached state. Returns the input unchanged when
// respID is empty or the write fails, so chaining is always best-effort.
func ApplyCodexWebsocketPreviousResponseID(wsReqBody []byte, respID string) []byte {
	if len(wsReqBody) == 0 || respID == "" {
		return wsReqBody
	}
	out, err := sjson.SetBytes(bytes.Clone(wsReqBody), "previous_response_id", respID)
	if err != nil || len(out) == 0 {
		return wsReqBody
	}
	return out
}

package helps

import (
	"bytes"

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

// CodexWebsocketWarmupEvent classifies a single upstream SSE payload observed
// while draining a warmup response. It reports whether the event is terminal
// (the warmup completed/failed and the drain loop should stop) and, on a
// successful completion, the response id to chain the next turn from.
type CodexWebsocketWarmupEvent struct {
	// Terminal is true once the warmup response reached a terminal event
	// (completed / failed / incomplete / error), meaning the drain loop should
	// stop reading.
	Terminal bool
	// Completed is true only for a clean `response.completed`.
	Completed bool
	// ResponseID is the warmup response's id, populated on `response.completed`
	// so the next real turn can set `previous_response_id`.
	ResponseID string
}

// ParseCodexWebsocketWarmupEvent inspects one upstream SSE payload (the JSON
// object, with or without the leading `data: ` framing) during a warmup drain.
//
// Terminal events MUST match the WS executor's own terminal contract, which
// treats BOTH `response.completed` and `response.done` as successful terminals
// (the executor even rewrites `response.done` -> `response.completed` in
// normalizeCodexWebsocketCompletion). If this helper recognized only
// `response.completed`, a warmup that terminates with `response.done` would
// never be seen as terminal and the drain loop would block until context
// cancellation or the socket idle timeout, delaying the first real turn.
//
//   - response.completed / response.done -> Terminal + Completed, ResponseID captured
//   - response.failed / response.incomplete / response.error -> Terminal only
//
// All other events are non-terminal (Terminal=false) and should be ignored:
// warmup produces reasoning/output items we intentionally discard.
func ParseCodexWebsocketWarmupEvent(payload []byte) CodexWebsocketWarmupEvent {
	trimmed := bytes.TrimSpace(payload)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return CodexWebsocketWarmupEvent{}
	}
	switch gjson.GetBytes(trimmed, "type").String() {
	case "response.completed", "response.done":
		return CodexWebsocketWarmupEvent{Terminal: true, Completed: true, ResponseID: codexWarmupResponseID(trimmed)}
	case "response.failed", "response.incomplete", "response.error":
		return CodexWebsocketWarmupEvent{Terminal: true}
	default:
		return CodexWebsocketWarmupEvent{}
	}
}

// codexWarmupResponseID extracts the warmup response id, mirroring the
// executor's extractResponseIDFromSSEPayload: prefer `response.id`, fall back to
// a top-level `id` (used by some event shapes).
func codexWarmupResponseID(payload []byte) string {
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

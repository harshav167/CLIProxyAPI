package executor

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// synthIDRegex matches `"id":"msg_synth_<digits>"` as emitted by Droid's
// `msg_synth_${syntheticMsgCounter++}` generator in src/models/converters.ts.
// The counter is module-global and increments on every request rebuild, so
// the same historical assistant message gets a different synthetic ID each
// turn. Anchoring on the `"id":` key prefix avoids rewriting occurrences
// that appear inside text content (e.g. tool_result strings that reference
// the bug).
var synthIDRegex = regexp.MustCompile(`"id":"msg_synth_\d+"`)

// normalizeSynthIDs rewrites every `"id":"msg_synth_N"` occurrence to a
// stable canonical form. Used as the fallback for item types we don't have
// explicit canonical field orders for, and applied post-canonicalization as
// belt-and-suspenders in case a synth id appears nested inside a field value.
func normalizeSynthIDs(data []byte) []byte {
	if !bytes.Contains(data, []byte(`"msg_synth_`)) {
		return data
	}
	return synthIDRegex.ReplaceAll(data, []byte(`"id":"msg_synth_X"`))
}

// needsRawFallback reports whether an upstream Responses-API event type
// SHOULD be force-forwarded as raw JSON to non-Droid clients when the
// translator returns zero chunks. Most events (response.created,
// response.in_progress, response.output_text.done, content_part.*,
// reasoning_summary_part.*, etc.) are intentionally dropped by the
// codex→openai translator because they carry no client-visible content —
// they are pre-content or bookkeeping events. Forwarding them raw would
// inject Responses-API JSON into a chat-completions SSE stream and break
// the client's chunk parser.
//
// The only events the translator has no rule for AND which clients need to
// observe are terminal-failure events: response.failed and response.error.
// Without forwarding those, the client sees buffered=0 and emits
// empty_stream 500.
func needsRawFallback(eventType string) bool {
	switch eventType {
	case "response.failed", "response.error":
		return true
	default:
		return false
	}
}

// synthesizeChatCompletionsErrorChunk converts a Codex Responses-API
// response.failed / response.error event into an OpenAI-streaming-style
// error envelope: {"error":{"message":...,"type":"server_error","code":...}}.
//
// Used by the zero-chunk fallback when the chat-completions
// translator has no rule for terminal failure events. Forwarding the raw
// `{"type":"response.failed",...}` payload would have the openai handler
// wrap it as `data: {"type":"response.failed",...}` — which is not a
// chat.completion.chunk and breaks parsers (Cursor, openai-node, etc).
//
// The OpenAI handler appends `data: [DONE]` at end-of-stream, so the wire
// form becomes the documented chat-completions streaming error pattern:
//
//	data: {"error":{"message":"...","type":"server_error","code":"..."}}
//	data: [DONE]
//
// Synthesized failure events from emitSyntheticFailure use the same shape
// (response.error.{code,message} fields), so this works for both the real
// upstream-failure path AND the synthetic-on-abnormal-exit path.
func synthesizeChatCompletionsErrorChunk(payload []byte) []byte {
	code := gjson.GetBytes(payload, "response.error.code").String()
	msg := gjson.GetBytes(payload, "response.error.message").String()
	if msg == "" {
		msg = gjson.GetBytes(payload, "error.message").String()
	}
	if code == "" {
		code = gjson.GetBytes(payload, "error.code").String()
	}
	if msg == "" {
		msg = "upstream stream ended with failure"
	}
	if code == "" {
		code = "upstream_error"
	}
	out, _ := sjson.SetBytes(nil, "error.message", msg)
	out, _ = sjson.SetBytes(out, "error.type", "server_error")
	out, _ = sjson.SetBytes(out, "error.code", code)
	return out
}

// itemFieldsDroidEcho declares, per Codex request.input item type, the exact
// field set and order Droid emits in its outbound requests. The bridge's
// byte-level prefix comparison fails when Droid's input items and the items
// the bridge captured from the prior turn's response.output differ in fields
// or order. Backend-returned items include fields Droid strips before echoing
// (`id` on function_call, `status` on function_call / message, etc.), so a
// raw byte compare forces `full_send` every turn even for identical history.
//
// canonicalizeBridgeItem rebuilds both sides of the comparison in this
// canonical form so bytes match when semantics do.
//
// For unknown types we fall through to synth-id normalization only — safer to
// leave bytes intact than mis-canonicalize a type we haven't mapped.
var itemFieldsDroidEcho = map[string][]string{
	"function_call":        {"type", "call_id", "name", "arguments"},
	"function_call_output": {"type", "call_id", "output"},
	"reasoning":            {"type", "content", "summary"},
	// Messages: `id` is retained if present. Backend `msg_<hash>` ids are
	// echoed by Droid and must match; `msg_synth_N` is collapsed to
	// `msg_synth_X` below.
	"message": {"type", "role", "content", "id"},
}

// contentBlockFieldsCanonical declares the field set and order for each
// content-block type inside a message's `content` array. Backend response
// output_text blocks carry extra fields (`annotations`, `logprobs`) that
// client converters (both Droid's converters.ts and cpapi-plus's
// ConvertOpenAIRequestToCodex) strip when echoing back. Without
// canonicalizing at this depth, the bridge sees byte-different content
// arrays on every turn and forces full_send.
//
// IMPORTANT: include every field that distinguishes one block from another.
// Stripping a discriminator (e.g. file_data) makes distinct files compare
// equal, which lets the bridge mark a turn as "prefix match" when the user
// actually sent different content — silently dropping their attachment.
var contentBlockFieldsCanonical = map[string][]string{
	"output_text": {"type", "text"},
	"refusal":     {"type", "refusal"},
	"input_text":  {"type", "text"},
	"input_image": {"type", "image_url", "detail"},
	// input_file: translator may emit either file_id (server-stored) or
	// inline file_data + filename (uploaded). Include all three so distinct
	// files can never canonicalize to the same prefix.
	"input_file": {"type", "file_id", "file_data", "filename"},
}

// buildCanonicalObject writes a JSON object containing only the named fields
// from raw, in the given order. Field values are emitted with their raw JSON
// (preserving exact byte representation) so subsequent prefix comparisons
// remain stable. Missing fields are skipped without disturbing ordering.
//
// transform optionally rewrites a field's raw value before emission. Pass
// nil for straight passthrough; non-nil enables per-field rewrites such as
// synth-id collapse on message.id or recursive canonicalization on
// message.content.
func buildCanonicalObject(raw []byte, fields []string, transform func(field string, val gjson.Result) []byte) []byte {
	buf := make([]byte, 0, len(raw))
	buf = append(buf, '{')
	first := true
	for _, field := range fields {
		val := gjson.GetBytes(raw, field)
		if !val.Exists() {
			continue
		}
		var rawValue []byte
		if transform != nil {
			rawValue = transform(field, val)
		} else {
			rawValue = []byte(val.Raw)
		}
		if !first {
			buf = append(buf, ',')
		}
		buf = append(buf, '"')
		buf = append(buf, field...)
		buf = append(buf, '"', ':')
		buf = append(buf, rawValue...)
		first = false
	}
	buf = append(buf, '}')
	return buf
}

// canonicalizeContentBlock rebuilds a single content-block object (inside a
// message's content array) in canonical form, stripping backend-added fields
// like `annotations` and `logprobs` that clients don't echo back.
func canonicalizeContentBlock(raw []byte) []byte {
	if len(raw) == 0 || raw[0] != '{' {
		return raw
	}
	blockType := gjson.GetBytes(raw, "type").String()
	fields, ok := contentBlockFieldsCanonical[blockType]
	if !ok {
		return raw
	}
	return buildCanonicalObject(raw, fields, nil)
}

// canonicalizeContentArray rebuilds the `content` JSON array with each
// element canonicalized via canonicalizeContentBlock.
func canonicalizeContentArray(raw []byte) []byte {
	if len(raw) == 0 || raw[0] != '[' {
		return raw
	}
	arr := gjson.ParseBytes(raw).Array()
	if len(arr) == 0 {
		return raw
	}
	buf := make([]byte, 0, len(raw))
	buf = append(buf, '[')
	for i, elem := range arr {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, canonicalizeContentBlock([]byte(elem.Raw))...)
	}
	buf = append(buf, ']')
	return buf
}

// canonicalizeBridgeItem rebuilds a Codex request.input item in a canonical
// form for bridge prefix comparison. Symmetric on both sides (baseline
// capture + next-turn compare) — produces byte-equal output for semantically
// equal items even when one side includes backend-added fields that the
// other strips before sending.
//
// Handles four drift sources identified in production:
//  1. `msg_synth_\d+` counter thrash (Droid's synthetic message id — patch 11)
//  2. function_call items: backend adds `id` (fc_*) and `status` fields
//     that clients strip when echoing into the next request.input (patch 12)
//  3. Field ordering differences: backend returns `{id, type, status, ...}`,
//     clients emit `{type, call_id, name, arguments}` (patch 12)
//  4. Content-block sub-field drift: backend's output_text has
//     `{type, annotations, logprobs, text}`, clients echo `{type, text}`
//     only. Affects both Droid and Cursor through ConvertOpenAIRequestToCodex.
//     (patch 15)
func canonicalizeBridgeItem(raw []byte) []byte {
	if len(raw) == 0 || raw[0] != '{' {
		return raw
	}

	typeStr := gjson.GetBytes(raw, "type").String()
	if typeStr == "" && gjson.GetBytes(raw, "role").Exists() {
		typeStr = "message"
	}

	order, ok := itemFieldsDroidEcho[typeStr]
	if !ok {
		// Unknown type: preserve raw bytes except for synth-id normalization.
		// Conservative on purpose — never mis-canonicalize a type we haven't
		// mapped, since that would produce false prefix matches.
		return normalizeSynthIDs(raw)
	}

	buf := buildCanonicalObject(raw, order, func(field string, val gjson.Result) []byte {
		switch {
		case field == "id" && strings.HasPrefix(val.String(), "msg_synth_"):
			return []byte(`"msg_synth_X"`)
		case field == "content" && val.IsArray():
			return canonicalizeContentArray([]byte(val.Raw))
		default:
			return []byte(val.Raw)
		}
	})

	return normalizeSynthIDs(buf)
}

// collectOutputItem extracts the `item` field from a
// response.output_item.done event payload and routes it into byIndex (when
// the event carries output_index) or appended to fallback (when not).
// Mutates byIndex in place; returns the (possibly extended) fallback slice.
//
// Used by both the HTTP Codex executor and the bridged WS capture wrapper to
// rebuild response.output when response.completed omits it. Centralizing the
// classification keeps the two paths in lockstep — divergence here would
// produce two different baselines for the same upstream response.
func collectOutputItem(eventData []byte, byIndex map[int64][]byte, fallback [][]byte) [][]byte {
	itemResult := gjson.GetBytes(eventData, "item")
	if !itemResult.Exists() || itemResult.Type != gjson.JSON {
		return fallback
	}
	if outputIndexResult := gjson.GetBytes(eventData, "output_index"); outputIndexResult.Exists() {
		byIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
	} else {
		fallback = append(fallback, []byte(itemResult.Raw))
	}
	return fallback
}

// patchResponseOutputIfMissing rebuilds response.output from collected items
// when the payload omits the array (or has it empty). Returns the patched
// payload and true only when a patch was applied; otherwise returns the input
// untouched and false.
//
// Indexed items are sorted ascending by output_index to preserve upstream
// ordering; un-indexed (fallback) items are appended after, in arrival order.
//
// Codex HTTP executor uses the patched body for the SSE response sent back
// to the client; bridged WS capture uses it as the baseline persisted for
// next-turn delta computation. Both paths must agree on item ordering or
// the bridge's prefix comparison would force full_send.
func patchResponseOutputIfMissing(payload []byte, byIndex map[int64][]byte, fallback [][]byte) ([]byte, bool) {
	if len(byIndex) == 0 && len(fallback) == 0 {
		return payload, false
	}
	outputResult := gjson.GetBytes(payload, "response.output")
	if !outputResult.Exists() {
		outputResult = gjson.GetBytes(payload, "output")
	}
	if outputResult.Exists() && outputResult.IsArray() && len(outputResult.Array()) > 0 {
		return payload, false
	}
	patched := payload
	patched, _ = sjson.SetRawBytes(patched, "response.output", []byte(`[]`))
	indexes := make([]int64, 0, len(byIndex))
	for idx := range byIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	for _, idx := range indexes {
		patched, _ = sjson.SetRawBytes(patched, "response.output.-1", byIndex[idx])
	}
	for _, item := range fallback {
		patched, _ = sjson.SetRawBytes(patched, "response.output.-1", item)
	}
	return patched, true
}

// HTTPToWSBridge tracks per-session response state to enable HTTP→WebSocket
// response chaining via previous_response_id. When a Droid client sends an HTTP
// request and we have a cached response ID from a prior WS turn, we can route
// through the WS executor with only the delta input items, letting the upstream
// use its connection-local response cache instead of re-processing the full context.
type HTTPToWSBridge struct {
	mu       sync.Mutex
	sessions map[string]*bridgeSession
}

type bridgeSession struct {
	lastResponseID string
	requestShape   []byte
	baselineItems  [][]byte
	model          string
	authID         string
	updatedAt      time.Time

	// lastCompletedAt tracks when the most recent response.completed event fired.
	lastCompletedAt time.Time
}

// NewHTTPToWSBridge creates a new bridge with background cleanup.
func NewHTTPToWSBridge() *HTTPToWSBridge {
	b := &HTTPToWSBridge{sessions: make(map[string]*bridgeSession)}
	go b.cleanup()
	return b
}

var (
	httpWSBridge     *HTTPToWSBridge
	httpWSBridgeOnce sync.Once
)

func getHTTPWSBridge() *HTTPToWSBridge {
	httpWSBridgeOnce.Do(func() {
		httpWSBridge = NewHTTPToWSBridge()
	})
	return httpWSBridge
}

// ComputeDelta checks if we can chain via previous_response_id.
// Uses Codex-style validation: non-input fields must match and input must extend
// the previous baseline (previous input + previous response output).
// Returns (deltaInputJSON, previousResponseID). Returns (nil, "") if a full send is needed.
//
// Lock scope: holds b.mu only to snapshot session fields and to delete the
// session entry on mismatch. JSON parsing, payloadShape canonicalization, and
// per-item canonicalization run unlocked, so concurrent sessions don't block
// each other on long input arrays.
func (b *HTTPToWSBridge) ComputeDelta(sessionKey string, payload []byte, authID string) ([]byte, string) {
	// Snapshot the session state under a brief lock.
	b.mu.Lock()
	sess, ok := b.sessions[sessionKey]
	if !ok || sess.lastResponseID == "" {
		b.mu.Unlock()
		return nil, ""
	}
	snap := struct {
		lastResponseID string
		requestShape   []byte
		baselineItems  [][]byte
		model          string
		authID         string
	}{
		lastResponseID: sess.lastResponseID,
		requestShape:   sess.requestShape,
		baselineItems:  sess.baselineItems,
		model:          sess.model,
		authID:         sess.authID,
	}
	b.mu.Unlock()

	// Validation + canonicalization run unlocked. resetWithReason() acquires the
	// lock only when invalidation is necessary, and only deletes the entry if
	// it still matches our snapshot's lastResponseID (no concurrent recapture).
	resetWithReason := func(format string, args ...interface{}) ([]byte, string) {
		log.Debugf("codex http-ws bridge: "+format, args...)
		b.mu.Lock()
		if cur, ok := b.sessions[sessionKey]; ok && cur.lastResponseID == snap.lastResponseID {
			delete(b.sessions, sessionKey)
		}
		b.mu.Unlock()
		return nil, ""
	}

	model := gjson.GetBytes(payload, "model").String()
	if model != snap.model {
		return resetWithReason("model mismatch for session %s (%s vs %s), resetting", sessionKey, model, snap.model)
	}

	if authID != "" && snap.authID != "" && authID != snap.authID {
		return resetWithReason("auth mismatch for session %s (%s vs %s), resetting", sessionKey, authID, snap.authID)
	}

	currentShape := payloadShape(payload)
	if len(snap.requestShape) == 0 || !bytes.Equal(currentShape, snap.requestShape) {
		return resetWithReason("request shape changed for session %s, resetting", sessionKey)
	}

	newInput := gjson.GetBytes(payload, "input")
	if !newInput.IsArray() {
		return nil, ""
	}
	newItems := newInput.Array()
	baselineCount := len(snap.baselineItems)

	if len(newItems) <= baselineCount {
		return resetWithReason("input shrunk/unchanged for session %s (new=%d, baseline=%d), resetting", sessionKey, len(newItems), baselineCount)
	}

	for i := 0; i < baselineCount; i++ {
		candidate := canonicalizeBridgeItem([]byte(newItems[i].Raw))
		if !bytes.Equal(candidate, snap.baselineItems[i]) {
			return resetWithReason("prefix mismatch for session %s at item %d, resetting", sessionKey, i)
		}
	}

	delta := newItems[baselineCount:]
	if len(delta) == 0 {
		return resetWithReason("empty delta for session %s, resetting", sessionKey)
	}

	buf := []byte("[")
	for i, item := range delta {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, item.Raw...)
	}
	buf = append(buf, ']')

	respIDPreview := snap.lastResponseID
	if len(respIDPreview) > 20 {
		respIDPreview = respIDPreview[:20]
	}
	log.Debugf("codex http-ws bridge: delta for session %s, baseline=%d, new=%d, delta=%d, prev_resp=%s",
		sessionKey, baselineCount, len(newItems), len(delta), respIDPreview)

	return buf, snap.lastResponseID
}

// CaptureResponse stores the response state for the next turn.
func (b *HTTPToWSBridge) CaptureResponse(sessionKey, responseID, model, authID string, requestPayload, responsePayload []byte) {
	if sessionKey == "" || responseID == "" {
		return
	}

	chainResponseID := responseID
	if foldedID := strings.TrimSpace(gjson.GetBytes(responsePayload, "response.metadata.proxy_upstream_previous_response_id").String()); foldedID != "" {
		chainResponseID = foldedID
	} else if foldedID := strings.TrimSpace(gjson.GetBytes(responsePayload, "metadata.proxy_upstream_previous_response_id").String()); foldedID != "" {
		chainResponseID = foldedID
	}

	baselineItems := make([][]byte, 0)
	input := gjson.GetBytes(requestPayload, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			baselineItems = append(baselineItems, canonicalizeBridgeItem([]byte(item.Raw)))
		}
	}
	output := gjson.GetBytes(responsePayload, "output")
	if !output.IsArray() {
		output = gjson.GetBytes(responsePayload, "response.output")
	}
	if output.IsArray() {
		for _, item := range output.Array() {
			baselineItems = append(baselineItems, canonicalizeBridgeItem([]byte(item.Raw)))
		}
	}
	requestShape := payloadShape(requestPayload)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[sessionKey] = &bridgeSession{
		lastResponseID: chainResponseID,
		requestShape:   requestShape,
		baselineItems:  baselineItems,
		model:          model,
		authID:         authID,
		updatedAt:      time.Now(),
	}
	respIDPreview := chainResponseID
	if len(respIDPreview) > 20 {
		respIDPreview = respIDPreview[:20]
	}
	log.Debugf("codex http-ws bridge: captured resp=%s for session %s (baseline=%d)",
		respIDPreview, sessionKey, len(baselineItems))
}

// shapeFields are the request fields that define the "shape" of a request.
// If any of these change between turns, the bridge must reset and send a full payload.
// This mirrors Codex CLI's semantic comparison: it compares the parsed request struct
// with input cleared, which is equivalent to comparing these fields.
var shapeFields = []string{
	"model",
	"instructions",
	"tools",
	"tool_choice",
	"parallel_tool_calls",
	"reasoning",
	"stream",
	"include",
	"service_tier",
	"text",
	"temperature",
	"top_p",
	"max_output_tokens",
	"store",
}

// payloadShape extracts a canonical, order-independent representation of
// shape-relevant fields from the request payload. Two payloads with the same
// semantic shape produce identical output regardless of JSON field ordering
// or whitespace differences.
func payloadShape(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	// Build a canonical JSON object with only shape fields, sorted by key.
	// Using sjson to build guarantees consistent output.
	var shape []byte
	shape = append(shape, '{')
	first := true
	for _, field := range shapeFields {
		val := gjson.GetBytes(payload, field)
		if !val.Exists() {
			continue
		}
		if !first {
			shape = append(shape, ',')
		}
		first = false
		shape = append(shape, '"')
		shape = append(shape, field...)
		shape = append(shape, '"', ':')
		shape = append(shape, val.Raw...)
	}
	shape = append(shape, '}')
	return shape
}

// HasSession returns true if the bridge has cached state for this session key.
func (b *HTTPToWSBridge) HasSession(sessionKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.sessions[sessionKey]
	return ok
}

// GapSinceLastCompleted returns the duration since the most recent response.completed on this session.
func (b *HTTPToWSBridge) GapSinceLastCompleted(sessionKey string) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionKey]
	if !ok || sess == nil || sess.lastCompletedAt.IsZero() {
		return 0, true
	}
	return time.Since(sess.lastCompletedAt), false
}

// MarkCompleted records when a response.completed event fired for this session.
func (b *HTTPToWSBridge) MarkCompleted(sessionKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionKey]
	if !ok || sess == nil {
		return
	}
	sess.lastCompletedAt = time.Now()
}

// BaselineCount returns the current baseline item count for a session (0 if none).
func (b *HTTPToWSBridge) BaselineCount(sessionKey string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionKey]
	if !ok || sess == nil {
		return 0
	}
	return len(sess.baselineItems)
}

// Reset removes the session state, forcing a full send on the next request.
func (b *HTTPToWSBridge) Reset(sessionKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, sessionKey)
	log.Debugf("codex http-ws bridge: reset session %s", sessionKey)
}

func (b *HTTPToWSBridge) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		b.mu.Lock()
		now := time.Now()
		for k, s := range b.sessions {
			if now.Sub(s.updatedAt) > 30*time.Minute {
				delete(b.sessions, k)
			}
		}
		b.mu.Unlock()
	}
}

// extractResponseIDFromSSEPayload extracts the response ID from a response.completed SSE event payload.
// The payload is expected to be the raw event data (after "data:" prefix).
func extractResponseIDFromSSEPayload(payload []byte) string {
	// Try response.id first (standard location in response.completed events).
	if id := gjson.GetBytes(payload, "response.id").String(); id != "" {
		return id
	}
	// Fallback: some events use top-level id.
	if id := gjson.GetBytes(payload, "id").String(); id != "" {
		return id
	}
	return ""
}

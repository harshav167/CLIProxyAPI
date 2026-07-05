package helps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeGLMRequestBody adapts an OpenAI-compatible request body to the
// Z.AI GLM API surface (docs.z.ai/api-reference/llm/chat-completion).
//
// The OpenAI-compatibility executor sends bodies in OpenAI Chat Completions
// shape. GLM in particular requires GLM-specific behaviours that the generic
// translation does not provide:
//
//  1. reasoning_effort only takes effect when `thinking.type == "enabled"`.
//     OpenAI clients (Cursor, Codex CLI, etc.) ship reasoning_effort without
//     ever setting thinking.type, which silently disables thinking on GLM.
//  2. Several OpenAI-only fields (service_tier, parallel_tool_calls,
//     prompt_cache_key, prompt_cache_retention) have no GLM counterpart and
//     are rejected or quietly dropped by z.ai depending on the route. Worse,
//     when they vary per turn (e.g. service_tier toggles) they break z.ai's
//     implicit prefix-based prompt cache. Stripping them is BOTH a correctness
//     fix AND a cache-hit-rate fix.
//  3. tool_stream defaults to false on GLM-4.6+, which buffers tool_calls to
//     the end of the stream. OpenAI clients (Cursor) expect per-chunk deltas.
//     Setting tool_stream=true on GLM-4.6+ restores OpenAI-equivalent
//     behaviour.
//  4. Tool definitions form part of the implicit cache prefix. Cursor does
//     not guarantee a stable tool-array order across turns. We sort tools by
//     function name so byte-identical tool sets always produce a byte-identical
//     prefix.
//
// The normaliser is a no-op for non-GLM traffic and is idempotent.
//
// It activates when EITHER the configured provider is "glm" OR the model id is
// GLM-family (e.g. "glm-5.2", "zai-org/GLM-5.2-FP8"). The latter matters because
// the same GLM models are served by multiple OpenAI-compatible vendors (z.ai,
// promo/relay endpoints, etc.); gating only on the provider name would leave
// those vendors broken. The empty-assistant-content sanitizer in particular is
// a GLM API requirement regardless of who serves the model.
func NormalizeGLMRequestBody(payload []byte, provider, model string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	// The reasoning/tool/field rewrites are GLM-profile behaviours; the
	// empty-content sanitizer is a hard GLM API requirement. Both are safe to
	// run whenever the request is GLM-bound (provider OR model family).
	if !isGLMProvider(provider) && !isGLMFamilyModel(model) {
		return payload
	}

	out := payload
	out = ensureGLMThinkingForReasoningEffort(out)
	out = mapGLMReasoningEffortAliases(out)
	out = stripGLMUnsupportedTopLevelFields(out)
	out = enableGLMToolStream(out, model)
	out = stabilizeGLMToolOrder(out)
	out = dropEmptyAssistantContentWithToolCalls(out)
	// Cursor → GLM system-prompt rewrite: GLM receives the same Cursor "AI coding
	// assistant" prompt family as Claude, so apply the shared execution-integrity
	// contract + generic plan/unexpected-changes patches (identity left as-is).
	out = ApplyCursorGLMSystemPromptUpgradeToPayload(out)
	return out
}

// isGLMProvider matches the openai-compatibility provider name configured in
// cliproxy-config.yaml (the canonical z.ai entry).
func isGLMProvider(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "glm")
}

// isGLMFamilyModel reports whether a model id belongs to the GLM family,
// independent of which vendor serves it. Matches bare ids ("glm-5.2"),
// vendor-prefixed ids ("zai-org/GLM-5.2-FP8"), and is case-insensitive. This is
// how a throwaway/relay provider serving GLM gets the same request fixes with
// zero config.
func isGLMFamilyModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	// Strip any vendor/org prefix (e.g. "zai-org/glm-5.2-fp8" -> "glm-5.2-fp8").
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.HasPrefix(name, "glm-") || strings.HasPrefix(name, "glm4") || strings.HasPrefix(name, "glm5")
}

// ensureGLMThinkingForReasoningEffort sets `thinking.type = "enabled"` when
// `reasoning_effort` is present, requests a thinking level, and the caller has
// not already configured `thinking`. Without this, GLM-5.2 silently ignores
// reasoning_effort per the spec ("reasoning_effort takes effect when `thinking`
// is enabled").
//
// Critically, the effort levels "none" and "minimal" mean "skip thinking
// entirely" per the GLM spec. For those we must NOT enable thinking — doing so
// would flip a thinking-off request into a thinking-on upstream call. We leave
// `thinking` untouched in that case (GLM treats absent thinking as disabled).
//
// If `thinking` is already present, the caller wins — we never overwrite it.
func ensureGLMThinkingForReasoningEffort(payload []byte) []byte {
	root := gjson.ParseBytes(payload)
	effort := root.Get("reasoning_effort")
	if !effort.Exists() {
		return payload
	}
	level := strings.TrimSpace(strings.ToLower(effort.String()))
	if level == "" || !glmEffortEnablesThinking(level) {
		return payload
	}
	if thinking := root.Get("thinking"); thinking.Exists() {
		return payload
	}
	thinkingBlock := map[string]string{"type": "enabled"}
	updated, err := sjson.SetBytes(payload, "thinking", thinkingBlock)
	if err != nil {
		return payload
	}
	return updated
}

// glmEffortEnablesThinking reports whether a reasoning_effort level requests
// any thinking at all. Per the GLM spec, "none" and "minimal" mean the model
// skips thinking entirely, so they must not trigger thinking.type=enabled.
// Unknown levels default to enabling thinking, matching the conservative
// "effort was explicitly requested" intent.
func glmEffortEnablesThinking(level string) bool {
	switch level {
	case "none", "minimal":
		return false
	default:
		return true
	}
}

// mapGLMReasoningEffortAliases coerces OpenAI-style reasoning_effort levels
// into the GLM enum.
//
// Per the GLM spec (docs.z.ai/api-reference/llm/chat-completion, the
// `reasoning_effort` field):
//
//	max | xhigh | high | medium | low | minimal | none
//
// And the documented mapping behaviour:
//
//   - "none" / "minimal" => the model skips thinking entirely
//   - "low" / "medium"   => mapped to "high"
//
// We deliberately do NOT remap "xhigh" => "max". Both are valid in z.ai's
// documented enum, so the remap gains nothing on the direct z.ai API — but it
// actively breaks GLM served through OpenAI-style relays (cline.bot → Vercel AI
// Gateway, apikey.fun, etc.) whose effort enum tops out at "xhigh" and rejects
// "max" with a 400 (`Invalid option: expected one of none|minimal|low|medium|
// high|xhigh`, param reasoning.effort). Passing "xhigh" through verbatim works
// on z.ai AND every relay; "max" only works on z.ai direct. Confirmed against a
// prod cline.bot 400 (2026-07-01).
//
// GLM applies the remaining maps server-side anyway, but doing them client-side
// makes outbound captures less confusing and keeps the wire value stable across
// upstream behaviour changes.
func mapGLMReasoningEffortAliases(payload []byte) []byte {
	effort := gjson.GetBytes(payload, "reasoning_effort")
	if !effort.Exists() {
		return payload
	}
	raw := strings.TrimSpace(strings.ToLower(effort.String()))
	if raw == "" {
		return payload
	}
	var mapped string
	switch raw {
	case "low", "medium":
		mapped = "high"
	case "xhigh", "high", "max", "minimal", "none":
		// already a valid GLM enum value; keep as-is. "xhigh" in particular
		// must pass through unchanged so relay gateways (cline/Vercel) that cap
		// at "xhigh" accept it.
		return payload
	default:
		// Unknown levels are passed through unchanged so GLM can reject or
		// alias them itself. Coercing aggressively would mask client bugs.
		return payload
	}
	updated, err := sjson.SetBytes(payload, "reasoning_effort", mapped)
	if err != nil {
		return payload
	}
	return updated
}

// stripGLMUnsupportedTopLevelFields removes top-level fields that GLM does not
// document and that some OpenAI clients include by default. We delete rather
// than leave-and-pray because the GLM Coding endpoint has historically returned
// 4xx for unknown body fields, and the OpenAI general endpoint behaviour is
// undocumented for these. The list is conservative — only fields with no GLM
// analogue are removed.
func stripGLMUnsupportedTopLevelFields(payload []byte) []byte {
	out := payload
	for _, field := range glmUnsupportedTopLevelFields {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		updated, err := sjson.DeleteBytes(out, field)
		if err != nil {
			continue
		}
		out = updated
	}
	return out
}

// glmUnsupportedTopLevelFields enumerates the OpenAI-only request fields we
// strip on the way to z.ai. Add to this list only when a concrete capture or
// the GLM spec shows the field is rejected or undocumented.
var glmUnsupportedTopLevelFields = []string{
	"service_tier",
	"parallel_tool_calls",
	"prompt_cache_key",
	"prompt_cache_retention",
	"store",
	"metadata",
	"logprobs",
	"top_logprobs",
}

// enableGLMToolStream sets `tool_stream: true` for GLM-4.6+ models when tools
// are present and the caller has not already set it. Per the GLM spec, the
// default is false (tool_calls arrive as a single chunk at the end of the
// stream), which breaks OpenAI-streaming-shape expectations in Cursor and
// other clients that consume tool_call.function.arguments deltas chunk-by-
// chunk. Older GLM models (pre-4.6) don't support the flag, so we gate on
// model version.
func enableGLMToolStream(payload []byte, model string) []byte {
	if !glmSupportsToolStream(model) {
		return payload
	}
	root := gjson.ParseBytes(payload)
	tools := root.Get("tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return payload
	}
	if root.Get("tool_stream").Exists() {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "tool_stream", true)
	if err != nil {
		return payload
	}
	return updated
}

// glmSupportsToolStream returns true for GLM models documented to support
// `tool_stream` (GLM-4.6 and above, per docs.z.ai). The match is intentionally
// loose so that `glm-4.6`, `glm-4.6v`, `glm-4.7`, `glm-5`, `glm-5.1`,
// `glm-5.2` all qualify, while `glm-4.5` and older do not.
func glmSupportsToolStream(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	for _, supported := range glmToolStreamSupportedPrefixes {
		if strings.HasPrefix(name, supported) {
			return true
		}
	}
	return false
}

// glmToolStreamSupportedPrefixes is the allowlist of GLM model families that
// support tool_stream. Add new families (e.g. future "glm-6") here.
var glmToolStreamSupportedPrefixes = []string{
	"glm-4.6",
	"glm-4.7",
	"glm-5",
}

// stabilizeGLMToolOrder sorts the top-level `tools` array by the function name
// of each entry. GLM uses implicit, prefix-based prompt caching where the tool
// definitions are part of the cached prefix. Cursor does not guarantee a stable
// ordering of the tool array across turns; even one swapped pair breaks the
// cache for every subsequent turn in that conversation. Sorting deterministically
// (by function.name) gives us a stable prefix as long as the *set* of tools is
// stable, which is the real client invariant.
//
// We only reorder; we never mutate the contents of any individual tool entry.
// If the tools array is missing, empty, or any entry lacks a function name, we
// leave the array alone rather than risk silently breaking a working request.
func stabilizeGLMToolOrder(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.IsArray() {
		return payload
	}
	arr := tools.Array()
	if len(arr) < 2 {
		return payload
	}
	type indexed struct {
		name string
		raw  string
	}
	entries := make([]indexed, 0, len(arr))
	for _, entry := range arr {
		name := entry.Get("function.name").String()
		if strings.TrimSpace(name) == "" {
			// Unknown shape (retrieval/web_search tool, or a tool without a
			// name). Bail out — don't risk partial sort.
			return payload
		}
		entries = append(entries, indexed{name: name, raw: entry.Raw})
	}
	if sort.SliceIsSorted(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	}) {
		return payload
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})
	sortedRaw := make([]string, len(entries))
	for i, e := range entries {
		sortedRaw[i] = e.raw
	}
	sortedJSON := "[" + strings.Join(sortedRaw, ",") + "]"
	updated, err := sjson.SetRawBytes(payload, "tools", []byte(sortedJSON))
	if err != nil {
		return payload
	}
	return updated
}

// dropEmptyAssistantContentWithToolCalls removes the `content` field from any
// assistant message that carries tool_calls but whose content is empty.
//
// Background: OpenAI/Anthropic clients (Cursor included) emit assistant turns
// that made a tool call with no accompanying prose as:
//
//	{"role":"assistant","content":[],"tool_calls":[...]}
//
// GLM/z.ai rejects this with `400001 invalid_request_error` ("API 调用参数有误")
// because it treats an empty content array as an invalid parameter. GLM accepts
// the same message when `content` is a non-empty string/array OR is omitted
// entirely. We omit it (the safe, lossless shape) only when content is empty
// (`[]`, `""`, or null) AND tool_calls is a non-empty array — so we never drop
// real assistant text.
//
// Confirmed against a prod 400 (2026-06-27): 10 of 301 messages had content:[]
// with tool_calls; the other 85 assistant+tool_calls turns had real content and
// were accepted.
func dropEmptyAssistantContentWithToolCalls(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	out := payload
	idx := -1
	messages.ForEach(func(_, msg gjson.Result) bool {
		idx++
		if msg.Get("role").String() != "assistant" {
			return true
		}
		toolCalls := msg.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			return true
		}
		content := msg.Get("content")
		if !isEmptyMessageContent(content) {
			return true
		}
		if updated, err := sjson.DeleteBytes(out, fmt.Sprintf("messages.%d.content", idx)); err == nil {
			out = updated
		}
		return true
	})
	return out
}

// isEmptyMessageContent reports whether a message content value carries no
// usable text: missing, JSON null, empty string, or empty array.
func isEmptyMessageContent(content gjson.Result) bool {
	if !content.Exists() {
		return true
	}
	switch content.Type {
	case gjson.Null:
		return true
	case gjson.String:
		return strings.TrimSpace(content.String()) == ""
	default:
		if content.IsArray() {
			return len(content.Array()) == 0
		}
		return false
	}
}

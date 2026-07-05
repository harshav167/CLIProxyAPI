package helps

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Kimi K2 served through OpenAI-compatible relays (Ollama/Fireworks, model id
// "kimi-k2.7-code" and friends) streams its chain-of-thought in the OpenAI
// `reasoning` delta field, but wraps it in literal `<think>` / `</think>`
// sentinel tokens. Unlike Moonshot's native Kimi API (handled by KimiExecutor),
// these relays emit an UNBALANCED opening `<think>` with no matching close in
// the reasoning channel — the close often lands in a later chunk or is dropped
// entirely.
//
// When passed through verbatim to a client (e.g. Cursor), the stray tags:
//  1. render as raw `<think>` / `</think>` text in the thinking panel, and
//  2. corrupt the reconstructed assistant turn on the NEXT request — the client
//     splits the turn at the unbalanced tag, pushing the closing `</think>` plus
//     real prose into `content`, which in turn desynchronises tool_call
//     boundaries (phantom "it ran a tool" and dropped/stripped tool_calls).
//
// These helpers strip the sentinel tokens from the reasoning channel on the
// OpenAI-compatible response path. They are deliberately scoped to Kimi-family
// models so GLM and other OpenAI-compat providers on the same executor are
// untouched. The native Moonshot Kimi path (KimiExecutor) has its own reasoning
// handling and does not use these.

// IsKimiFamilyModel reports whether a model id belongs to the Kimi/K2 family,
// independent of which vendor serves it. Matches bare ids ("kimi-k2.7-code"),
// vendor-prefixed ids ("accounts/fireworks/models/kimi-k2p7-code"), and is
// case-insensitive.
func IsKimiFamilyModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	// Strip any vendor/org prefix (e.g. "accounts/fireworks/models/kimi-k2p7-code").
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.HasPrefix(name, "kimi-") || strings.HasPrefix(name, "kimi") || strings.HasPrefix(name, "k2")
}

// stripThinkSentinels removes literal <think> and </think> tokens (any casing,
// with or without surrounding whitespace-only artefacts) from a reasoning
// string. It only removes the sentinel tags themselves; the reasoning prose is
// preserved untouched.
func stripThinkSentinels(s string) string {
	if s == "" || !strings.Contains(s, "<") {
		return s
	}
	// Fast path: only touch strings that actually carry a think tag.
	lower := strings.ToLower(s)
	if !strings.Contains(lower, "<think>") && !strings.Contains(lower, "</think>") {
		return s
	}
	// Remove both variants case-insensitively without regex to stay allocation-light.
	for _, tag := range []string{"</think>", "<think>", "</THINK>", "<THINK>"} {
		s = strings.ReplaceAll(s, tag, "")
	}
	// Catch mixed-case variants by a final case-insensitive sweep.
	if strings.Contains(strings.ToLower(s), "think>") {
		s = removeCaseInsensitive(s, "<think>")
		s = removeCaseInsensitive(s, "</think>")
	}
	return s
}

// removeCaseInsensitive removes all case-insensitive occurrences of token from s.
func removeCaseInsensitive(s, token string) string {
	lowerToken := strings.ToLower(token)
	for {
		lower := strings.ToLower(s)
		idx := strings.Index(lower, lowerToken)
		if idx < 0 {
			return s
		}
		s = s[:idx] + s[idx+len(token):]
	}
}

// NormalizeKimiReasoningStreamLine strips <think>/</think> sentinels from the
// `choices[].delta.reasoning` field of a single OpenAI-compatible SSE data
// line. The input may include the leading "data:" framing; it is preserved.
// Non-Kimi models, non-data lines, and lines without a reasoning field are
// returned unchanged.
func NormalizeKimiReasoningStreamLine(model string, line []byte) []byte {
	if !IsKimiFamilyModel(model) || len(line) == 0 {
		return line
	}
	prefix := []byte("data:")
	payload := line
	var framing []byte
	if bytes.HasPrefix(bytes.TrimSpace(line), prefix) {
		// Split "data:" (and any following space) from the JSON payload.
		trimmed := bytes.TrimSpace(line)
		rest := bytes.TrimPrefix(trimmed, prefix)
		framing = []byte("data: ")
		payload = bytes.TrimSpace(rest)
	}
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return line
	}
	root := gjson.ParseBytes(payload)
	choices := root.Get("choices")
	if !choices.IsArray() {
		return line
	}
	out := payload
	changed := false
	choices.ForEach(func(key, choice gjson.Result) bool {
		reasoning := choice.Get("delta.reasoning")
		if !reasoning.Exists() || reasoning.Type != gjson.String {
			return true
		}
		cleaned := stripThinkSentinels(reasoning.String())
		if cleaned == reasoning.String() {
			return true
		}
		path := "choices." + key.String() + ".delta.reasoning"
		if updated, err := sjson.SetBytes(out, path, cleaned); err == nil {
			out = updated
			changed = true
		}
		return true
	})
	if !changed {
		return line
	}
	if framing != nil {
		return append(append([]byte{}, framing...), out...)
	}
	return out
}

// NormalizeKimiReasoningNonStream strips <think>/</think> sentinels from the
// `choices[].message.reasoning` field of a full OpenAI-compatible chat
// completion response body. Non-Kimi models and bodies without reasoning are
// returned unchanged.
func NormalizeKimiReasoningNonStream(model string, body []byte) []byte {
	if !IsKimiFamilyModel(model) || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	choices := gjson.GetBytes(body, "choices")
	if !choices.IsArray() {
		return body
	}
	out := body
	choices.ForEach(func(key, choice gjson.Result) bool {
		reasoning := choice.Get("message.reasoning")
		if !reasoning.Exists() || reasoning.Type != gjson.String {
			return true
		}
		cleaned := stripThinkSentinels(reasoning.String())
		if cleaned == reasoning.String() {
			return true
		}
		path := "choices." + key.String() + ".message.reasoning"
		if updated, err := sjson.SetBytes(out, path, cleaned); err == nil {
			out = updated
		}
		return true
	})
	return out
}

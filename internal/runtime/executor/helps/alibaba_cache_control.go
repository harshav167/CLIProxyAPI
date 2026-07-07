package helps

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// alibabaCacheControlMarker is the cache_control object injected on message
// content blocks bound for Alibaba Token Plan. Alibaba's explicit cache
// supports ONLY type:"ephemeral" with a 5-minute TTL renewable on hit. It has
// no TTL option (unlike Anthropic's "ttl":"1h") and no scope concept (unlike
// Anthropic's "scope":"global"). Keeping the marker minimal avoids sending
// fields Alibaba would reject or ignore.
const alibabaCacheControlMarker = `{"type":"ephemeral"}`

// isAlibabaTokenPlanBaseURL reports whether the resolved provider base-url
// points at Alibaba Cloud Model Studio (Token Plan or pay-as-you-go MaaS).
// Both expose the same explicit-cache contract on /compatible-mode/v1.
//
// Gating on the base-url (not the provider name) is required because both
// z.ai and Alibaba are configured in prod as `name: glm`, and z.ai's implicit
// cache has different semantics — it must NOT receive cache_control markers.
// Gating on the model family alone is insufficient for the same reason
// (z.ai serves GLM-family models too).
func isAlibabaTokenPlanBaseURL(baseURL string) bool {
	return strings.Contains(baseURL, "maas.aliyuncs")
}

// payloadHasAnyCacheControl reports whether any message in the payload already
// carries a cache_control marker. Used for the skip-if-client-marked guard,
// mirroring the Claude path's `if CountClaudeCacheControls(body) == 0` guard
// at claude_executor.go:253. If the client (or an upstream pipeline stage)
// already placed markers, we must not add our own — double-marking would
// either be rejected or produce a second cache block the caller did not intend.
func payloadHasAnyCacheControl(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	found := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					found = true
					return false
				}
				return true
			})
		}
		return !found
	})
	return found
}

// ApplyAlibabaExplicitCache injects cache_control:{type:"ephemeral"} markers
// into an OpenAI chat-completions request body bound for Alibaba Token Plan,
// converting the system+tools prefix and the previous-turn boundary from
// best-effort 20%-price implicit cache hits to deterministic 10%-price
// explicit cache hits.
//
// Mirrors the proven Claude path (InjectClaudeSystemCacheControl +
// InjectClaudeMessagesCacheControl) with two simplifications: Alibaba has no
// TTL option (always 5m ephemeral) and no scope concept (no global-anchor
// stripping needed).
//
// Marker placement:
//   - Marker 1: last content block of the system message (messages.0 where
//     role=="system"). Stable across all conversations on the same model —
//     the cross-conversation/subagent shared anchor.
//   - Marker 2: last content block of the SECOND-TO-LAST user message. This
//     is the stable boundary from the previous turn. The LAST user message is
//     the current turn and changes every request, so caching it is useless.
//     Skipped on single-turn conversations (fewer than 2 user messages).
//
// No-op when:
//   - baseURL does not contain "maas.aliyuncs" (z.ai, Fireworks, Ollama, etc.)
//   - payload is empty or not valid JSON
//   - any message already carries a cache_control marker (skip-if-client-marked)
//   - messages.0 is not role=="system"
//
// Idempotent: f(f(x)) == f(x). The skip-if-client-marked guard plus the
// array-conversion check make a second call a no-op.
func ApplyAlibabaExplicitCache(payload []byte, baseURL string) []byte {
	if len(payload) == 0 || !isAlibabaTokenPlanBaseURL(baseURL) {
		return payload
	}
	if !gjson.ValidBytes(payload) {
		return payload
	}
	if payloadHasAnyCacheControl(payload) {
		return payload
	}

	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return payload
	}

	// Marker 1: system message (messages.0, role=="system").
	first := messages.Array()[0]
	if first.Get("role").String() != "system" {
		return payload
	}
	out, ok := injectMarkerOnMessageContent(payload, 0, first.Get("content"))
	if !ok {
		return payload
	}

	// Marker 2: second-to-last user message.
	out = injectMarkerOnSecondToLastUser(out)

	return out
}

// injectMarkerOnMessageContent sets cache_control on the last content block of
// messages[idx].content. Handles both string and array content shapes:
//   - string content -> converted to [{type:"text", text:<orig>, cache_control:{type:"ephemeral"}}]
//   - array content  -> cache_control set on the last block (idempotent: if the
//     last block already has cache_control, the payload is returned unchanged)
//
// Returns the (possibly modified) payload and a bool indicating whether the
// message had placeable content (false => caller should treat the whole call
// as a no-op and return the original payload).
func injectMarkerOnMessageContent(payload []byte, idx int, content gjson.Result) ([]byte, bool) {
	contentPath := fmt.Sprintf("messages.%d.content", idx)
	if content.IsArray() {
		blocks := content.Array()
		if len(blocks) == 0 {
			return payload, true // empty array: nothing to mark, but not a fatal case
		}
		lastBlockIdx := len(blocks) - 1
		ccPath := fmt.Sprintf("messages.%d.content.%d.cache_control", idx, lastBlockIdx)
		if blocks[lastBlockIdx].Get("cache_control").Exists() {
			return payload, true // idempotent: already marked
		}
		updated, err := sjson.SetRawBytes(payload, ccPath, []byte(alibabaCacheControlMarker))
		if err != nil {
			return payload, true
		}
		return updated, true
	}
	if content.Type == gjson.String {
		text := content.String()
		newContent := fmt.Sprintf(`[{"type":"text","text":%s,"cache_control":%s}]`,
			jsonString(text), alibabaCacheControlMarker)
		updated, err := sjson.SetRawBytes(payload, contentPath, []byte(newContent))
		if err != nil {
			return payload, true
		}
		return updated, true
	}
	// content is null/missing/number/etc — nothing to mark.
	return payload, true
}

// injectMarkerOnSecondToLastUser finds the second-to-last user message and
// places a cache_control marker on the last content block of that message.
// Mirrors InjectClaudeMessagesCacheControl (claude_cache_control.go:401-473):
// the last user message is the current turn (changes every request, useless to
// cache); the second-to-last is the stable boundary from the previous turn.
func injectMarkerOnSecondToLastUser(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	var userIndices []int
	messages.ForEach(func(idx, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userIndices = append(userIndices, int(idx.Int()))
		}
		return true
	})
	if len(userIndices) < 2 {
		return payload
	}
	secondToLastIdx := userIndices[len(userIndices)-2]
	content := gjson.GetBytes(payload, fmt.Sprintf("messages.%d.content", secondToLastIdx))
	out, _ := injectMarkerOnMessageContent(payload, secondToLastIdx, content)
	return out
}

// jsonString returns a JSON-encoded string literal for s, including the
// surrounding double quotes. Used to build the array-form content without
// pulling in encoding/json (matches the gjson/sjson style of the package).
func jsonString(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

package helps

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeOpenAICompatStreamLine rewrites a single OpenAI-compatible SSE
// data line so downstream clients that consume `delta` chunks (Cursor
// primarily) render both the reasoning and the visible reply.
//
// This is model- and provider-agnostic. Two prod stream shapes have been
// observed for the same logical model on the openai-compat path:
//
//	Ollama kimi:  delta:{role:"assistant", content:"Hello"}
//	              — role present on every chunk; `content` and `reasoning`
//	                are absent (not emitted) during the phase they're empty.
//
//	Alibaba MaaS compatible-mode (glm/kimi/anything they serve):
//	              delta:{content:"", reasoning_content:"The"}
//	              delta:{content:"Hello", reasoning_content:""}
//	              — role only on the first chunk; BOTH `content` and
//	                `reasoning_content` are emitted every chunk as explicit
//	                empty strings during the phase they're not populating.
//
// Cursor's chat-completions stream parser renders the Ollama shape
// correctly (thinking visible + text visible) but renders the Alibaba
// shape as: thinking visible, text EMPTY (verified prod 2026-07-05).
// Dropping the empty-string `content`/`reasoning_content` and re-adding
// `role:"assistant"` to content-bearing deltas restores the rendered
// shape Cursor expects. The transformation is information-preserving —
// an empty string in these fields carries no data.
//
// Apply on the openai-compat streaming response path for any provider.
// It is a no-op on deltas that already match the target shape, and on
// non-data lines / [DONE] markers / non-JSON payloads.
//
// Sibling to NormalizeKimiReasoningStreamLine (which is Kimi-gated for an
// unrelated sentinel-strip concern). Same call site, different concern.
func NormalizeOpenAICompatStreamLine(line []byte) []byte {
	if len(line) == 0 {
		return line
	}
	prefix := []byte("data:")
	payload := line
	var framing []byte
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, prefix) {
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
	// Fast path: skip JSON parsing if none of the relevant keys are present.
	if !bytes.Contains(payload, []byte(`"delta"`)) &&
		!bytes.Contains(payload, []byte(`"content"`)) &&
		!bytes.Contains(payload, []byte(`"reasoning_content"`)) {
		return line
	}
	choices := gjson.GetBytes(payload, "choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		return line
	}

	out := payload
	changed := false
	for idx := range choices.Array() {
		deltaPath := "choices." + itoa(idx) + ".delta"
		delta := gjson.GetBytes(out, deltaPath)
		if !delta.Exists() || !delta.IsObject() {
			continue
		}

		// Drop empty-string content. Repeated across every reasoning-phase
		// chunk, this is what makes Cursor treat the assistant turn as
		// contentless even when later chunks carry non-empty content.
		if c := delta.Get("content"); c.Exists() && c.Type == gjson.String && c.String() == "" {
			if updated, err := sjson.DeleteBytes(out, deltaPath+".content"); err == nil {
				out = updated
				changed = true
				delta = gjson.GetBytes(out, deltaPath)
			}
		}

		// Drop empty-string reasoning_content (same reason; this is the
		// mirror case during the content phase).
		if r := delta.Get("reasoning_content"); r.Exists() && r.Type == gjson.String && r.String() == "" {
			if updated, err := sjson.DeleteBytes(out, deltaPath+".reasoning_content"); err == nil {
				out = updated
				changed = true
				delta = gjson.GetBytes(out, deltaPath)
			}
		}

		// Alibaba only emits role:"assistant" on the first chunk of the
		// turn. Cursor's parser keys off role on content chunks; restore
		// it when missing on a content-bearing delta.
		if c := delta.Get("content"); c.Exists() && c.Type == gjson.String && c.String() != "" {
			if r := delta.Get("role"); !r.Exists() {
				if updated, err := sjson.SetBytes(out, deltaPath+".role", "assistant"); err == nil {
					out = updated
					changed = true
				}
			}
		}
	}

	if !changed {
		return line
	}
	if framing != nil {
		return append(append([]byte{}, framing...), out...)
	}
	return out
}

// itoa is a small int->string helper for choices-array indices. Avoids
// pulling strconv into the hot per-chunk loop; we only need decimal
// indices 0..N. No exponent/sign handling required beyond the basics.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

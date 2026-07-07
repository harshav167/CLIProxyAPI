package helps

import (
	"bytes"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeOpenAICompatStreamLine drops explicit empty content fields from
// OpenAI-compatible SSE chunks and restores role:"assistant" on content chunks.
// Alibaba MaaS emits that shape, and Cursor otherwise renders a visible
// thinking block with an empty answer.
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
		deltaPath := "choices." + strconv.Itoa(idx) + ".delta"
		delta := gjson.GetBytes(out, deltaPath)
		if !delta.Exists() || !delta.IsObject() {
			continue
		}

		// Empty content on reasoning chunks makes Cursor treat the turn as
		// contentless even when later chunks carry text.
		if c := delta.Get("content"); c.Exists() && c.Type == gjson.String && c.String() == "" {
			if updated, err := sjson.DeleteBytes(out, deltaPath+".content"); err == nil {
				out = updated
				changed = true
				delta = gjson.GetBytes(out, deltaPath)
			}
		}

		if r := delta.Get("reasoning_content"); r.Exists() && r.Type == gjson.String && r.String() == "" {
			if updated, err := sjson.DeleteBytes(out, deltaPath+".reasoning_content"); err == nil {
				out = updated
				changed = true
				delta = gjson.GetBytes(out, deltaPath)
			}
		}

		// Alibaba emits role only on the first chunk; Cursor needs it on
		// content chunks.
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

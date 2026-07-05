package chat_completions

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// buildScalingOpenAIChatForClaude builds an OpenAI chat-completions request with
// nTurns assistant(tool_call)/tool(result) pairs. Cursor sends this OpenAI shape
// even for native Claude models, so ConvertOpenAIRequestToClaude runs on every
// Opus turn. It appends each translated message with sjson "messages.-1", which
// re-serialises the growing output on each append -> O(n^2) suspect.
func buildScalingOpenAIChatForClaude(nTurns int) []byte {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 60)
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-opus-4-8","messages":[`)
	sb.WriteString(`{"role":"system","content":"You are a coding assistant."},`)
	sb.WriteString(`{"role":"user","content":"start"}`)
	for t := 0; t < nTurns; t++ {
		sb.WriteString(fmt.Sprintf(
			`,{"role":"assistant","content":%q,"tool_calls":[{"id":"call_%d","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}`,
			filler, t))
		sb.WriteString(fmt.Sprintf(
			`,{"role":"tool","tool_call_id":"call_%d","content":%q}`,
			t, filler))
	}
	sb.WriteString(`],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]}`)
	return []byte(sb.String())
}

// TestScaling_ConvertOpenAIRequestToClaude prints wall-clock timing of the
// OpenAI->Claude request build across growing conversation lengths. Diagnostic
// (always passes): ~4x time per 2x turns => O(n^2).
func TestScaling_ConvertOpenAIRequestToClaude(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingOpenAIChatForClaude(nTurns)
		_ = ConvertOpenAIRequestToClaude("claude-opus-4-8", body, true) // warm
		const reps = 5
		start := time.Now()
		for i := 0; i < reps; i++ {
			_ = ConvertOpenAIRequestToClaude("claude-opus-4-8", body, true)
		}
		per := time.Since(start) / reps
		t.Logf("ConvertOpenAIRequestToClaude nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

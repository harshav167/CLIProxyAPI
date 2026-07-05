package chat_completions

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// buildScalingOpenAIChatBody builds an OpenAI chat-completions request with
// nTurns assistant(tool_call)/tool(result) pairs plus filler text, mirroring a
// long Cursor GPT/Codex conversation. Used to measure how ConvertOpenAIRequestToCodex
// scales with conversation length (the OpenAI->Codex Responses request builder
// appends each translated item with sjson "input.-1", which re-serialises the
// growing output array on every append -> O(n^2) suspect).
func buildScalingOpenAIChatBody(nTurns int) []byte {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 60)
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-5.4","messages":[`)
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

// TestScaling_ConvertOpenAIRequestToCodex prints wall-clock timing of the
// OpenAI->Codex request build across growing conversation lengths. Diagnostic
// (always passes): if time ~4x per 2x turns, the pass is O(n^2).
func TestScaling_ConvertOpenAIRequestToCodex(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingOpenAIChatBody(nTurns)
		_ = ConvertOpenAIRequestToCodex("gpt-5.4", body, true) // warm
		const reps = 5
		start := time.Now()
		for i := 0; i < reps; i++ {
			_ = ConvertOpenAIRequestToCodex("gpt-5.4", body, true)
		}
		per := time.Since(start) / reps
		t.Logf("ConvertOpenAIRequestToCodex nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

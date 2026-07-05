package chat_completions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// buildScalingChatBody builds an OpenAI chat body with nTurns tool-call/result
// pairs, matching a long Cursor conversation against an openai-compatible
// provider (GLM, and every other provider added via openai-compatibility).
func buildScalingChatBody(nTurns int) []byte {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 60)
	var sb strings.Builder
	sb.WriteString(`{"model":"glm-5.2","messages":[{"role":"user","content":"start"}`)
	for t := 0; t < nTurns; t++ {
		sb.WriteString(fmt.Sprintf(`,{"role":"assistant","content":%q}`, filler))
		sb.WriteString(fmt.Sprintf(`,{"role":"user","content":%q}`, filler))
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// TestScaling_ConvertOpenAIRequestToOpenAI measures the openai->openai request
// passthrough (used by every openai-compatible provider). Expected O(n): it
// only sets the model field once.
func TestScaling_ConvertOpenAIRequestToOpenAI(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingChatBody(nTurns)
		const reps = 20
		start := time.Now()
		for i := 0; i < reps; i++ {
			_ = ConvertOpenAIRequestToOpenAI("glm-5.2", body, true)
		}
		per := time.Since(start) / reps
		t.Logf("ConvertOpenAIRequestToOpenAI nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

// TestScaling_ConvertOpenAIResponseToOpenAI_Stream measures the per-SSE-line
// streaming response translation cost as the accumulated param state grows.
// Each call translates one chunk; we call it once per simulated output token
// against a param that accumulates, to see if per-chunk cost grows with
// conversation length (O(n^2) over a stream) or stays flat (O(n)).
func TestScaling_ConvertOpenAIResponseToOpenAI_Stream(t *testing.T) {
	ctx := context.Background()
	for _, nChunks := range []int{200, 400, 800, 1600} {
		line := []byte(`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello world "}}]}`)
		var param any
		start := time.Now()
		for i := 0; i < nChunks; i++ {
			_ = ConvertOpenAIResponseToOpenAI(ctx, "glm-5.2", nil, nil, line, &param)
		}
		total := time.Since(start)
		perChunk := total / time.Duration(nChunks)
		t.Logf("ConvertOpenAIResponseToOpenAI nChunks=%-5d total=%-10v  %v/chunk", nChunks, total, perChunk)
	}
}

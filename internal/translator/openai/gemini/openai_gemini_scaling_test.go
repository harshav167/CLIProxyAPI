package gemini

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// buildScalingGeminiReq builds a Gemini-shaped request (contents[]) with nTurns
// model/user turns. ConvertGeminiRequestToOpenAI translates it to OpenAI shape,
// appending each turn with sjson "messages.-1" (O(n^2) suspect).
func buildScalingGeminiReq(nTurns int) []byte {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 60)
	var sb strings.Builder
	sb.WriteString(`{"contents":[{"role":"user","parts":[{"text":"start"}]}`)
	for t := 0; t < nTurns; t++ {
		sb.WriteString(fmt.Sprintf(`,{"role":"model","parts":[{"text":%q}]}`, filler))
		sb.WriteString(fmt.Sprintf(`,{"role":"user","parts":[{"text":%q}]}`, filler))
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// TestScaling_ConvertGeminiRequestToOpenAI measures the gemini->openai request
// build (used when a client sends Gemini format). GC-off recommended.
func TestScaling_ConvertGeminiRequestToOpenAI(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingGeminiReq(nTurns)
		_ = ConvertGeminiRequestToOpenAI("gemini-2.5-pro", body, true) // warm
		const reps = 5
		start := time.Now()
		for i := 0; i < reps; i++ {
			_ = ConvertGeminiRequestToOpenAI("gemini-2.5-pro", body, true)
		}
		per := time.Since(start) / reps
		t.Logf("ConvertGeminiRequestToOpenAI nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

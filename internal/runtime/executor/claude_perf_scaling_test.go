package executor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/sjson"
)

// buildScalingClaudeBody builds a realistic Cursor→Claude Opus payload with
// nTools tool definitions and nTurns assistant tool_use / user tool_result
// message pairs, each carrying a chunk of filler text so the serialized body
// is close to the multi-hundred-KB shape seen in prod. Tool names are the
// lowercase OpenCode-style names that remapOAuthToolNames rewrites, so the
// hot rename loop is exercised on every message.
func buildScalingClaudeBody(nTurns int) []byte {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 60) // ~1.6KB per block
	toolNames := []string{"bash", "read", "write", "edit", "glob", "grep", "task"}

	var sb strings.Builder
	sb.WriteString(`{"model":"claude-opus-4-8","max_tokens":64000,"system":[{"type":"text","text":"You are Claude Code."}],"tools":[`)
	for i, n := range toolNames {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf(`{"name":%q,"description":"tool","input_schema":{"type":"object"}}`, n))
	}
	sb.WriteString(`],"messages":[`)
	sb.WriteString(`{"role":"user","content":[{"type":"text","text":"start"}]}`)
	for t := 0; t < nTurns; t++ {
		name := toolNames[t%len(toolNames)]
		// assistant tool_use turn
		sb.WriteString(fmt.Sprintf(
			`,{"role":"assistant","content":[{"type":"text","text":%q},{"type":"tool_use","id":"tu_%d","name":%q,"input":{"x":1}}]}`,
			filler, t, name))
		// user tool_result turn
		sb.WriteString(fmt.Sprintf(
			`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_%d","content":[{"type":"text","text":%q}]}]}`,
			t, filler))
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// TestScaling_RemapOAuthToolNames prints wall-clock timing of the OAuth
// tool-name remap across growing conversation lengths. If the pass is O(n^2)
// in the number of tool blocks, doubling nTurns roughly quadruples the time.
// This is a diagnostic (always passes) used to prove the hotspot and, after
// the fix, to prove the improvement.
func TestScaling_RemapOAuthToolNames(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingClaudeBody(nTurns)
		// warm one pass (allocs, caches)
		_, _ = remapOAuthToolNames(body)
		const reps = 5
		start := time.Now()
		for i := 0; i < reps; i++ {
			_, _ = remapOAuthToolNames(body)
		}
		per := time.Since(start) / reps
		t.Logf("remapOAuthToolNames nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

// TestScaling_CacheControlChain times the cache-control helper chain that also
// runs on every Claude turn, for comparison against the remap pass.
func TestScaling_CacheControlChain(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingClaudeBody(nTurns)
		const reps = 5
		start := time.Now()
		for i := 0; i < reps; i++ {
			b := body
			b = helps.EnsureClaudeCacheControl(b)
			b = helps.EnsureClaudeUserPromptCacheAnchor(b)
			b = helps.EnforceClaudeCacheControlLimit(b, 4)
			b = helps.NormalizeClaudeCacheControlTTL(b)
			_ = b
		}
		per := time.Since(start) / reps
		t.Logf("cacheControlChain  nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

// TestScaling_SignBody times the billing-signature pass (single xxHash + 2
// full-body sets) as an O(n) reference point.
func TestScaling_SignBody(t *testing.T) {
	for _, nTurns := range []int{50, 100, 200, 400} {
		body := buildScalingClaudeBody(nTurns)
		// give it a billing header so the signer does its full work
		body, _ = sjson.SetBytes(body, "system.0.text", "x-anthropic-billing-header: cc_version=1.0.abc; cc_entrypoint=cli; cch=12345;")
		const reps = 5
		start := time.Now()
		for i := 0; i < reps; i++ {
			_ = signAnthropicMessagesBody(body)
		}
		per := time.Since(start) / reps
		t.Logf("signAnthropicBody  nTurns=%-4d bodyKB=%-5d  %v/op", nTurns, len(body)/1024, per)
	}
}

package helps

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexContinue* constants mirror CodexCont's defaults. The config struct
// itself lives in internal/config to avoid an import cycle.
const (
	CodexContinueDefaultStep       = 518
	CodexContinueDefaultMinN       = 1
	CodexContinueMethodCommentary  = "commentary"
	CodexContinueMethodToolPair    = "tool_pair"
	CodexContinueDefaultMarkerText = "Continue thinking..."
	CodexContinueEncryptedInclude  = "reasoning.encrypted_content"
)

// NormalizeCodexContinueConfig fills defaults. Disabled unless explicitly enabled.
func NormalizeCodexContinueConfig(c *config.CodexContinueConfig) *config.CodexContinueConfig {
	if c == nil {
		return &config.CodexContinueConfig{Enabled: false}
	}
	if c.TruncationStep <= 0 {
		c.TruncationStep = CodexContinueDefaultStep
	}
	if c.MaxContinue < 0 {
		c.MaxContinue = 0
	}
	if c.MinN <= 0 {
		c.MinN = CodexContinueDefaultMinN
	}
	// MaxN: 0 = uncapped (CodexCont semantics) — leave 0 as-is.
	if c.MarkerText == "" {
		c.MarkerText = CodexContinueDefaultMarkerText
	}
	return c
}

// IsTruncationPattern reports whether `tokens` lands exactly on step*n - 2
// (516, 1034, 1552, ...). n=0 (tokens=-2 or below step-2) is excluded.
func IsTruncationPattern(tokens int, step int) bool {
	if step <= 0 {
		step = CodexContinueDefaultStep
	}
	if tokens < step-2 {
		return false
	}
	return (tokens+2)%step == 0
}

// TierN returns the truncation tier n (1-based) for a matching token count,
// else 0. n=1 ↔ 516, n=2 ↔ 1034, ...
func TierN(tokens int, step int) int {
	if step <= 0 {
		step = CodexContinueDefaultStep
	}
	if !IsTruncationPattern(tokens, step) {
		return 0
	}
	return (tokens + 2) / step
}

// ShouldContinue is true iff tokens matches the fingerprint and minN <= n <= maxN.
// maxN=0 means uncapped.
func ShouldContinue(tokens int, step int, minN int, maxN int) bool {
	n := TierN(tokens, step)
	if n == 0 || n < minN {
		return false
	}
	if maxN != 0 && n > maxN {
		return false
	}
	return true
}

// ReasoningTokens extracts usage.output_tokens_details.reasoning_tokens from a
// Codex response.completed event payload. Returns ok=false when absent.
func ReasoningTokens(data []byte) (int, bool) {
	v := gjson.GetBytes(data, "response.usage.output_tokens_details.reasoning_tokens")
	if !v.Exists() {
		return 0, false
	}
	return int(v.Int()), true
}

// CommentaryMessage returns a phase:"commentary" assistant message — the clean
// continuation provocation. phase is an official Responses-API field; agents
// preserve it cross-turn, and it carries no synthetic tool.
func CommentaryMessage(text string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text}},
		"phase":   "commentary",
	}
}

// BuildContinuationPayload shapes the agent's original translated request body
// for one upstream continuation round: force stream=true, set input to the
// original input + replay tail (prior-round reasoning + continue marker),
// ensure reasoning.encrypted_content is in include, and drop
// previous_response_id (we carry state explicitly).
//
// Never invents model/instructions/reasoning/tools — those are the agent's.
func BuildContinuationPayload(baseBody []byte, inputItems []any, forceIncludeEncrypted bool) []byte {
	out := baseBody
	// Force stream — we always stream upstream.
	out, _ = sjson.SetBytes(out, "stream", true)
	// Drop previous_response_id — we carry state explicitly in input.
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	// Replace input.
	inputJSON, _ := json.Marshal(inputItems)
	out, _ = sjson.SetRawBytes(out, "input", inputJSON)
	// Ensure reasoning.encrypted_content is in include when requested.
	include := gjson.GetBytes(out, "include")
	if forceIncludeEncrypted || include.Exists() {
		items := []string{}
		if include.Exists() && include.IsArray() {
			for _, r := range include.Array() {
				items = append(items, r.String())
			}
		}
		hasEnc := false
		for _, s := range items {
			if s == CodexContinueEncryptedInclude {
				hasEnc = true
				break
			}
		}
		if forceIncludeEncrypted && !hasEnc {
			items = append(items, CodexContinueEncryptedInclude)
		}
		out, _ = sjson.SetBytes(out, "include", items)
	}
	return out
}

// HasEncryptedReasoning reports whether a reasoning item carried an
// encrypted_content blob (CodexCont refuses to continue without it — the
// continuation round needs the encrypted reasoning to defeat truncation).
func HasEncryptedReasoning(reasoningItems []map[string]any) bool {
	if len(reasoningItems) == 0 {
		return false
	}
	last := reasoningItems[len(reasoningItems)-1]
	enc, ok := last["encrypted_content"].(string)
	return ok && enc != ""
}

// ReasoningEnabled reports whether the request body has reasoning active.
// CodexCont's contract: reasoning is ON by default — these models reason even
// with no `reasoning` field. Only an explicit opt-out (`reasoning: false`)
// disables it; absent / empty / dict all count as enabled. The fold only
// applies to reasoning requests.
func ReasoningEnabled(body []byte) bool {
	v := gjson.GetBytes(body, "reasoning")
	if !v.Exists() {
		return true
	}
	return v.Type != gjson.False
}

// StoppedReasonWhenTruncated is the inverse of ShouldContinue: the round IS
// truncated but a guard prevented continuation. Used for metadata.
//
// Returns "" when the round was NOT truncated (clean finish). Returns the guard
// name otherwise: "no_encrypted_content", "max_continue", "max_total_output_tokens",
// "tier_out_of_window".
func StoppedReasonWhenTruncated(tokens int, step int, hasEncrypted bool, roundNo int, maxContinue int, totalOutput int, maxTotal int) string {
	if !IsTruncationPattern(tokens, step) {
		return ""
	}
	if !hasEncrypted {
		return "no_encrypted_content"
	}
	if maxContinue != 0 && roundNo > maxContinue {
		return "max_continue"
	}
	if maxTotal != 0 && totalOutput >= maxTotal {
		return "max_total_output_tokens"
	}
	return "tier_out_of_window"
}

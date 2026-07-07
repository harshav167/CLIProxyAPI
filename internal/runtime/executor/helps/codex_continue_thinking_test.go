package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestIsTruncationPattern(t *testing.T) {
	cases := []struct {
		tokens int
		want   bool
	}{
		{516, true},  // 518*1 - 2
		{1034, true}, // 518*2 - 2
		{1552, true}, // 518*3 - 2
		{0, false},
		{100, false},
		{518, false},  // exactly on step, not -2
		{1036, false}, // 518*2 - 0
		{-2, false},   // tokens < step-2 guard
	}
	for _, c := range cases {
		if got := IsTruncationPattern(c.tokens, 518); got != c.want {
			t.Errorf("IsTruncationPattern(%d) = %v, want %v", c.tokens, got, c.want)
		}
	}
}

func TestTierN(t *testing.T) {
	cases := []struct {
		tokens int
		want   int
	}{
		{516, 1},
		{1034, 2},
		{1552, 3},
		{2070, 4},
		{518, 0}, // not a truncation value
		{100, 0},
	}
	for _, c := range cases {
		if got := TierN(c.tokens, 518); got != c.want {
			t.Errorf("TierN(%d) = %d, want %d", c.tokens, got, c.want)
		}
	}
}

func TestShouldContinue(t *testing.T) {
	if !ShouldContinue(516, 518, 1, 6) {
		t.Error("ShouldContinue(516) within window = false, want true")
	}
	if ShouldContinue(516, 518, 2, 6) {
		t.Error("ShouldContinue(516) with minN=2 = true, want false (n=1 below minN)")
	}
	if ShouldContinue(3366, 518, 1, 6) {
		// 518*7 - 2 = 3624; 3366 is 518*6.5 - 1 (not a fingerprint)
		t.Error("ShouldContinue(3366) = true, want false (not a truncation value)")
	}
	if !ShouldContinue(3624, 518, 1, 0) {
		t.Error("ShouldContinue(3624) with maxN=0 (uncapped) = false, want true")
	}
	if ShouldContinue(3624, 518, 1, 6) {
		t.Error("ShouldContinue(3624) with maxN=6 = true, want false (n=7 > maxN)")
	}
}

func TestReasoningTokens(t *testing.T) {
	data := []byte(`{"type":"response.completed","response":{"usage":{"output_tokens_details":{"reasoning_tokens":1034}}}}`)
	tokens, ok := ReasoningTokens(data)
	if !ok {
		t.Fatal("ReasoningTokens returned ok=false on a 1034 fingerprint event")
	}
	if tokens != 1034 {
		t.Errorf("ReasoningTokens = %d, want 1034", tokens)
	}
	if _, ok := ReasoningTokens([]byte(`{"type":"response.completed","response":{"usage":{}}}`)); ok {
		t.Error("ReasoningTokens returned ok=true when reasoning_tokens absent")
	}
}

func TestCommentaryMessage(t *testing.T) {
	msg := CommentaryMessage("Continue thinking...")
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["phase"] != "commentary" {
		t.Errorf("CommentaryMessage wrong shape: %+v", msg)
	}
	content, ok := msg["content"].([]map[string]any)
	if !ok || len(content) != 1 {
		t.Fatalf("CommentaryMessage content wrong: %#v", msg["content"])
	}
	if content[0]["type"] != "output_text" || content[0]["text"] != "Continue thinking..." {
		t.Errorf("CommentaryMessage content[0] wrong: %+v", content[0])
	}
}

func TestBuildContinuationPayloadForcesStreamAndDropsPreviousResponseID(t *testing.T) {
	base := []byte(`{"model":"gpt-5","stream":false,"previous_response_id":"resp_abc","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out := BuildContinuationPayload(base, []any{
		map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
	}, true)
	if gjson.GetBytes(out, "stream").Bool() != true {
		t.Error("BuildContinuationPayload did not force stream=true")
	}
	if gjson.GetBytes(out, "previous_response_id").Exists() {
		t.Error("BuildContinuationPayload did not drop previous_response_id")
	}
	include := gjson.GetBytes(out, "include")
	if !include.IsArray() {
		t.Fatalf("include is not an array after forceIncludeEncrypted=true: %s", include.Raw)
	}
	found := false
	for _, r := range include.Array() {
		if r.String() == CodexContinueEncryptedInclude {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("include missing %s after forceIncludeEncrypted=true: %s", CodexContinueEncryptedInclude, include.Raw)
	}
}

func TestBuildContinuationPayloadPreservesExistingInclude(t *testing.T) {
	base := []byte(`{"model":"gpt-5","include":["reasoning.encrypted_content"]}`)
	out := BuildContinuationPayload(base, []any{}, false)
	include := gjson.GetBytes(out, "include")
	if !include.IsArray() {
		t.Fatalf("include is not an array: %s", include.Raw)
	}
	if len(include.Array()) != 1 || include.Array()[0].String() != CodexContinueEncryptedInclude {
		t.Errorf("include should be preserved as-is when forceIncludeEncrypted=false: %s", include.Raw)
	}
}

func TestHasEncryptedReasoning(t *testing.T) {
	if HasEncryptedReasoning(nil) {
		t.Error("HasEncryptedReasoning(nil) = true")
	}
	if HasEncryptedReasoning([]map[string]any{{"id": "rs_1"}}) {
		t.Error("HasEncryptedReasoning with empty encrypted_content = true")
	}
	if !HasEncryptedReasoning([]map[string]any{{"id": "rs_1", "encrypted_content": "abc123"}}) {
		t.Error("HasEncryptedReasoning with non-empty encrypted_content = false")
	}
}

func TestReasoningEnabled(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{}`, true},               // absent = enabled (default)
		{`{"reasoning":{}}`, true}, // dict = enabled
		{`{"reasoning":{"effort":"high"}}`, true},
		{`{"reasoning":true}`, true},
		{`{"reasoning":false}`, false}, // explicit opt-out
		{`{"reasoning":"high"}`, true}, // string level = enabled
	}
	for _, c := range cases {
		if got := ReasoningEnabled([]byte(c.body)); got != c.want {
			t.Errorf("ReasoningEnabled(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}

func TestStoppedReasonWhenTruncated(t *testing.T) {
	// Clean round — not truncated.
	if got := StoppedReasonWhenTruncated(600, 518, true, 1, 3, 100, 10000); got != "" {
		t.Errorf("not truncated but stopped_reason = %q", got)
	}
	// Truncated, no encrypted content.
	if got := StoppedReasonWhenTruncated(516, 518, false, 1, 3, 100, 10000); got != "no_encrypted_content" {
		t.Errorf("truncated+no-encrypted wrong reason = %q", got)
	}
	// Truncated, max_continue exceeded.
	if got := StoppedReasonWhenTruncated(516, 518, true, 4, 3, 100, 10000); got != "max_continue" {
		t.Errorf("truncated+max-continue wrong reason = %q", got)
	}
	// Truncated, max_total_output_tokens exceeded.
	if got := StoppedReasonWhenTruncated(516, 518, true, 1, 3, 10000, 10000); got != "max_total_output_tokens" {
		t.Errorf("truncated+max-tokens wrong reason = %q", got)
	}
	// Truncated, tier out of window (maxN=0 uncapped won't trigger this;
	// maxN=1 with n=2 does).
	if got := StoppedReasonWhenTruncated(1034, 518, true, 1, 3, 100, 10000); got != "tier_out_of_window" {
		t.Errorf("truncated+tier-out-of-window wrong reason = %q", got)
	}
}

func TestNormalizeCodexContinueConfigDefaults(t *testing.T) {
	// Enabled but all fields empty → defaults for step/method/marker; 0 for
	// max_continue/max_n means "no continuation"/"uncapped" respectively
	// (CodexCont semantics — 0 is a real value, not unset).
	c := &config.CodexContinueConfig{Enabled: true}
	got := NormalizeCodexContinueConfig(c)
	if got.TruncationStep != CodexContinueDefaultStep || got.MinN != CodexContinueDefaultMinN {
		t.Errorf("step/minN defaults wrong: %+v", got)
	}
	if got.MaxContinue != 0 || got.MaxN != 0 {
		t.Errorf("max_continue/max_n should stay 0 (CodexCont: 0=no-cap-or-stop), got %+v", got)
	}
	if got.MarkerText != CodexContinueDefaultMarkerText {
		t.Errorf("marker default wrong: %+v", got)
	}
	// nil → disabled.
	if NormalizeCodexContinueConfig(nil).Enabled {
		t.Error("nil config normalized to enabled")
	}
}

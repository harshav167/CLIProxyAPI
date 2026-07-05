package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsKimiFamilyModel(t *testing.T) {
	cases := map[string]bool{
		"kimi-k2.7-code": true,
		"kimi-k2.6":      true,
		"KIMI-K2":        true,
		"accounts/fireworks/models/kimi-k2p7-code": true,
		"k2-thinking":  true,
		"glm-5.2":      false,
		"glm-5.2-high": false,
		"gpt-5.4":      false,
		"":             false,
	}
	for model, want := range cases {
		if got := IsKimiFamilyModel(model); got != want {
			t.Errorf("IsKimiFamilyModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// TestNormalizeKimiReasoningStreamLine_StripsLeadingThink reproduces the exact
// prod wire shape captured from Ollama/Fireworks (2026-07-03): the first
// reasoning delta of an assistant turn carries a bare "<think>" sentinel.
func TestNormalizeKimiReasoningStreamLine_StripsLeadingThink(t *testing.T) {
	line := []byte(`data: {"id":"chatcmpl-700","object":"chat.completion.chunk","created":1783081154,"model":"kimi-k2.7-code","system_fingerprint":"fp_ollama","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"\u003cthink\u003e"}}]}`)
	out := NormalizeKimiReasoningStreamLine("kimi-k2.7-code", line)
	reasoning := gjson.GetBytes(stripDataPrefix(out), "choices.0.delta.reasoning").String()
	if strings.Contains(reasoning, "<think>") || strings.Contains(reasoning, "think>") {
		t.Fatalf("expected <think> stripped, got reasoning=%q; line=%s", reasoning, out)
	}
	if reasoning != "" {
		t.Fatalf("expected empty reasoning after stripping bare sentinel, got %q", reasoning)
	}
}

func TestNormalizeKimiReasoningStreamLine_PreservesProse(t *testing.T) {
	line := []byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"\u003cthink\u003eGood. My files are staged."}}]}`)
	out := NormalizeKimiReasoningStreamLine("accounts/fireworks/models/kimi-k2p7-code", line)
	reasoning := gjson.GetBytes(stripDataPrefix(out), "choices.0.delta.reasoning").String()
	if reasoning != "Good. My files are staged." {
		t.Fatalf("prose not preserved / tag not stripped: got %q", reasoning)
	}
}

func TestNormalizeKimiReasoningStreamLine_StripsClosingThink(t *testing.T) {
	line := []byte(`data: {"choices":[{"index":0,"delta":{"reasoning":"done reasoning\u003c/think\u003e"}}]}`)
	out := NormalizeKimiReasoningStreamLine("kimi-k2.7-code", line)
	reasoning := gjson.GetBytes(stripDataPrefix(out), "choices.0.delta.reasoning").String()
	if strings.Contains(reasoning, "think>") {
		t.Fatalf("closing tag not stripped: %q", reasoning)
	}
	if reasoning != "done reasoning" {
		t.Fatalf("got %q", reasoning)
	}
}

// GLM must be untouched even though it shares the executor.
func TestNormalizeKimiReasoningStreamLine_GLMUntouched(t *testing.T) {
	line := []byte(`data: {"choices":[{"index":0,"delta":{"reasoning":"\u003cthink\u003ekeep me"}}]}`)
	out := NormalizeKimiReasoningStreamLine("glm-5.2-high", line)
	if string(out) != string(line) {
		t.Fatalf("GLM line must be unchanged; got %s", out)
	}
}

func TestNormalizeKimiReasoningStreamLine_DoneMarkerUntouched(t *testing.T) {
	line := []byte("data: [DONE]")
	out := NormalizeKimiReasoningStreamLine("kimi-k2.7-code", line)
	if string(out) != string(line) {
		t.Fatalf("[DONE] must be unchanged; got %s", out)
	}
}

func TestNormalizeKimiReasoningStreamLine_NoReasoningUntouched(t *testing.T) {
	line := []byte(`data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`)
	out := NormalizeKimiReasoningStreamLine("kimi-k2.7-code", line)
	if string(out) != string(line) {
		t.Fatalf("content-only delta must be unchanged; got %s", out)
	}
}

func TestNormalizeKimiReasoningNonStream_StripsThink(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","reasoning":"\u003cthink\u003eanalysis here\u003c/think\u003e","content":"answer"}}]}`)
	out := NormalizeKimiReasoningNonStream("kimi-k2.7-code", body)
	reasoning := gjson.GetBytes(out, "choices.0.message.reasoning").String()
	if strings.Contains(reasoning, "think>") {
		t.Fatalf("tags not stripped: %q", reasoning)
	}
	if reasoning != "analysis here" {
		t.Fatalf("got %q", reasoning)
	}
	// content must be untouched
	if gjson.GetBytes(out, "choices.0.message.content").String() != "answer" {
		t.Fatalf("content changed unexpectedly")
	}
}

func TestNormalizeKimiReasoningNonStream_GLMUntouched(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"reasoning":"\u003cthink\u003ekeep"}}]}`)
	out := NormalizeKimiReasoningNonStream("glm-5.2", body)
	if string(out) != string(body) {
		t.Fatalf("GLM body must be unchanged")
	}
}

// stripDataPrefix is a test helper mirroring the SSE framing split.
func stripDataPrefix(line []byte) []byte {
	s := strings.TrimSpace(string(line))
	s = strings.TrimPrefix(s, "data:")
	return []byte(strings.TrimSpace(s))
}

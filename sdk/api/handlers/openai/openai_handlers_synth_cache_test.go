package openai

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestMaybeInjectSyntheticPromptCacheKeyInjectsWhenAbsent(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello world"}]}`)
	out := maybeInjectSyntheticPromptCacheKey(c, body)

	got := gjson.GetBytes(out, "prompt_cache_key").String()
	if got == "" {
		t.Fatal("expected prompt_cache_key to be injected, got empty")
	}
	if !strings.HasPrefix(got, "cli-proxy-") {
		t.Errorf("expected cli-proxy- prefix on synthetic key, got %q", got)
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyRespectsExisting(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	clientKey := "d3498f66-5fae-5e1e-9b81-81de4bb1441a"
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"` + clientKey + `","messages":[{"role":"user","content":"hi"}]}`)
	out := maybeInjectSyntheticPromptCacheKey(c, body)

	if got := gjson.GetBytes(out, "prompt_cache_key").String(); got != clientKey {
		t.Errorf("expected client key %q to pass through unchanged, got %q", clientKey, got)
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyStability(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	body1 := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"refactor this function"}]}`)
	body2 := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"refactor this function"}]}`)
	body3 := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"different prompt"}]}`)

	k1 := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, body1), "prompt_cache_key").String()
	k2 := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, body2), "prompt_cache_key").String()
	k3 := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, body3), "prompt_cache_key").String()

	if k1 != k2 {
		t.Errorf("identical first message + model must produce same key: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different first message must produce different keys, both got %q", k1)
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyHandlesMissingFields(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	cases := []struct {
		name string
		body []byte
	}{
		{"empty body", []byte(`{}`)},
		{"no messages", []byte(`{"model":"gpt-5.5"}`)},
		{"empty messages", []byte(`{"model":"gpt-5.5","messages":[]}`)},
		{"no user message", []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"sys"}]}`)},
		{"no model", []byte(`{"messages":[{"role":"user","content":"hi"}]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := maybeInjectSyntheticPromptCacheKey(c, tc.body)
			if !bytes.Equal(out, tc.body) {
				t.Errorf("expected body to pass through unchanged when anchor unavailable; got modified output")
			}
		})
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyContentArrayShape(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":[{"type":"text","text":"array shape prompt"}]}]}`)
	out := maybeInjectSyntheticPromptCacheKey(c, body)

	got := gjson.GetBytes(out, "prompt_cache_key").String()
	if got == "" {
		t.Fatal("array content shape should still produce a key")
	}
	if !strings.HasPrefix(got, "cli-proxy-") {
		t.Errorf("expected cli-proxy- prefix, got %q", got)
	}
}

func TestFirstUserMessageAnchorTruncates(t *testing.T) {
	huge := strings.Repeat("a", 100_000)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"` + huge + `"}]}`)
	const cap = 4096
	got := firstUserMessageAnchor(body, cap)
	if len(got) > cap {
		t.Errorf("anchor length = %d, want <= %d", len(got), cap)
	}
	if got == "" {
		t.Fatal("expected non-empty anchor")
	}
}

// TestMaybeInjectSyntheticPromptCacheKeyPrefersCursorConversationId locks
// the contract: when the body carries metadata.cursorConversationId, the
// synthetic pck MUST be derived from that UUID rather than from the
// first-user-message text anchor. This (a) eliminates content-hash
// collisions between independent chats opened in the same workspace state,
// and (b) lets Cursor subagents that share their parent's
// cursorConversationId automatically share the parent's upstream cache slot.
func TestMaybeInjectSyntheticPromptCacheKeyPrefersCursorConversationId(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")

	// Same first user message, same model, different conversation IDs.
	bodyA := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"shared opening prompt"}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"6e9c5188-53c5-4207-8a7b-c00d7428979c"},"messages":[{"role":"user","content":"shared opening prompt"}]}`)

	kA := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyA), "prompt_cache_key").String()
	kB := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyB), "prompt_cache_key").String()

	if kA == "" || kB == "" {
		t.Fatalf("expected non-empty keys, got A=%q B=%q", kA, kB)
	}
	if kA == kB {
		t.Errorf("two distinct cursorConversationIds with identical first message must produce DIFFERENT keys; both got %q", kA)
	}

	// Same conversation ID across two requests (turn 1 vs turn N) MUST
	// produce the same pck so cache stays warm across turns.
	bodyA2 := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"completely different content for turn 5"}]}`)
	kA2 := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyA2), "prompt_cache_key").String()
	if kA != kA2 {
		t.Errorf("same cursorConversationId across turns must produce same key; got %q vs %q", kA, kA2)
	}

	// A subagent inheriting parent's cursorConversationId MUST share parent's
	// pck even if its first user message is different (proving the new
	// derivation ignores message content when convID is present).
	parentBody := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"abcdef00-1111-2222-3333-444455556666"},"messages":[{"role":"user","content":"parent task description"}]}`)
	subagentBody := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"abcdef00-1111-2222-3333-444455556666"},"messages":[{"role":"user","content":"subtask: research X"}]}`)
	kParent := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, parentBody), "prompt_cache_key").String()
	kSub := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, subagentBody), "prompt_cache_key").String()
	if kParent != kSub {
		t.Errorf("subagent sharing parent's cursorConversationId must share parent's pck; parent=%q sub=%q", kParent, kSub)
	}

	// Falls back to message-anchor derivation when cursorConversationId is
	// absent — keeps existing clients unbroken.
	plainBody := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"shared opening prompt"}]}`)
	kPlain := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, plainBody), "prompt_cache_key").String()
	if kPlain == "" || !strings.HasPrefix(kPlain, "cli-proxy-") {
		t.Errorf("body without cursorConversationId must still get a valid synthetic pck; got %q", kPlain)
	}
	// Anchor-derived key on the same content must NOT collide with either
	// conv-id-derived key (different prefix in the hash domain separator).
	if kPlain == kA || kPlain == kB {
		t.Errorf("anchor-derived key must not collide with conv-id-derived keys; plain=%q A=%q B=%q", kPlain, kA, kB)
	}
}

func TestFirstUserMessageAnchorPicksFirstUserNotSystem(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"system prompt"},{"role":"user","content":"actual user message"}]}`)
	got := firstUserMessageAnchor(body, 4096)
	if got != "actual user message" {
		t.Errorf("anchor = %q, want %q", got, "actual user message")
	}
}

func newTestGinContext(t *testing.T, ua string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build test request: %v", err)
	}
	req.Header.Set("User-Agent", ua)
	c.Request = req
	return c, w
}

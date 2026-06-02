package openai

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
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

func TestMaybeInjectSyntheticPromptCacheKeyPrefersCursorSharedPrefixAnchor(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	c.Request.Header.Set("x-openai-subagent", "true")
	c.Request.Header.Set("x-codex-parent-thread-id", "parent-thread")

	// Same Cursor user, model, system prefix, and tools, but different
	// cursorConversationIds and different first user messages. This is the
	// measured subagent cold-start shape: Cursor gives each subagent a new
	// conversation ID even though their large reusable prefix is the same.
	bodyA := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"2fcda40b"},"messages":[{"role":"system","content":"workspace + tool policy"},{"role":"user","content":"subtask A"}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"b44d7ec2"},"messages":[{"role":"system","content":"workspace + tool policy"},{"role":"user","content":"subtask B"}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`)

	kA := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyA), "prompt_cache_key").String()
	kB := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyB), "prompt_cache_key").String()

	if kA == "" || kB == "" {
		t.Fatalf("expected non-empty keys, got A=%q B=%q", kA, kB)
	}
	if kA != kB {
		t.Fatalf("shared Cursor parent must produce the same prompt_cache_key across subagent conversation IDs: %q vs %q", kA, kB)
	}

	differentTools := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"0212397c"},"messages":[{"role":"system","content":"workspace + tool policy"},{"role":"user","content":"subtask C"}],"tools":[{"type":"function","function":{"name":"different_tool"}}]}`)
	kDifferentTools := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, differentTools), "prompt_cache_key").String()
	if kDifferentTools == "" {
		t.Fatal("expected different-tools request to still get a prompt_cache_key")
	}
	if kDifferentTools != kA {
		t.Fatalf("same explicit subagent parent must share prompt_cache_key even when prefix shifts: %q vs %q", kDifferentTools, kA)
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyKeepsCursorConversationStableAcrossSystemChanges(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	bodyA := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"system","content":"workspace context A"},{"role":"user","content":"turn one"}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"system","content":"workspace context B with lints"},{"role":"user","content":"turn two"}],"tools":[{"type":"function","function":{"name":"read_file"}},{"type":"function","function":{"name":"list_dir"}}]}`)

	kA := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyA), "prompt_cache_key").String()
	kB := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyB), "prompt_cache_key").String()
	if kA == "" || kB == "" {
		t.Fatalf("expected non-empty keys, got A=%q B=%q", kA, kB)
	}
	if kA != kB {
		t.Fatalf("same Cursor conversation must keep prompt_cache_key stable across system/tool changes: %q vs %q", kA, kB)
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyUsesSharedPrefixForExplicitSubagentWithoutParent(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	c.Request.Header.Set("x-openai-subagent", "true")
	bodyA := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"2fcda40b"},"messages":[{"role":"system","content":"workspace + tool policy"},{"role":"user","content":"subtask A"}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"b44d7ec2"},"messages":[{"role":"system","content":"workspace + tool policy"},{"role":"user","content":"subtask B"}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`)

	kA := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyA), "prompt_cache_key").String()
	kB := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyB), "prompt_cache_key").String()
	if kA == "" || kB == "" {
		t.Fatalf("expected non-empty keys, got A=%q B=%q", kA, kB)
	}
	if kA != kB {
		t.Fatalf("same explicit subagent shared prefix must produce stable prompt_cache_key: %q vs %q", kA, kB)
	}

	differentTools := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"0212397c"},"messages":[{"role":"system","content":"workspace + tool policy"},{"role":"user","content":"subtask C"}],"tools":[{"type":"function","function":{"name":"different_tool"}}]}`)
	kDifferentTools := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, differentTools), "prompt_cache_key").String()
	if kDifferentTools == "" {
		t.Fatal("expected different-tools request to still get a prompt_cache_key")
	}
	if kDifferentTools == kA {
		t.Fatalf("different reusable prefix without parent anchor must not share prompt_cache_key: %q", kDifferentTools)
	}
}

func TestMaybeInjectSyntheticPromptCacheKeyFallsBackToCursorConversationIdWithoutSharedPrefix(t *testing.T) {
	c, _ := newTestGinContext(t, "Cursor/1.0")
	bodyA := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"shared opening prompt"}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"6e9c5188-53c5-4207-8a7b-c00d7428979c"},"messages":[{"role":"user","content":"shared opening prompt"}]}`)

	kA := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyA), "prompt_cache_key").String()
	kB := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(c, bodyB), "prompt_cache_key").String()
	if kA == "" || kB == "" {
		t.Fatalf("expected non-empty keys, got A=%q B=%q", kA, kB)
	}
	if kA == kB {
		t.Fatalf("without a reusable prefix, different cursorConversationIds must not share prompt_cache_key: %q", kA)
	}
}

func TestFirstUserMessageAnchorPicksFirstUserNotSystem(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"system prompt"},{"role":"user","content":"actual user message"}]}`)
	got := firstUserMessageAnchor(body, 4096)
	if got != "actual user message" {
		t.Errorf("anchor = %q, want %q", got, "actual user message")
	}
}

func TestDeriveCursorSessionIDPrefersCursorConversationId(t *testing.T) {
	bodyA1 := []byte(`{"model":"gpt-5.4","user":"user-a","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"input":[{"role":"user","content":"turn one"}]}`)
	bodyA2 := []byte(`{"model":"gpt-5.4","user":"user-a","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"input":[{"role":"user","content":"turn five has different text"}]}`)
	bodyB := []byte(`{"model":"gpt-5.4","user":"user-a","metadata":{"cursorConversationId":"6e9c5188-53c5-4207-8a7b-c00d7428979c"},"input":[{"role":"user","content":"turn one"}]}`)

	sessionA1 := deriveCursorSessionID(bodyA1, "")
	sessionA2 := deriveCursorSessionID(bodyA2, "")
	sessionB := deriveCursorSessionID(bodyB, "")

	if sessionA1 == "" || !strings.HasPrefix(sessionA1, "cursor-conv-") {
		t.Fatalf("expected cursorConversationId-derived session, got %q", sessionA1)
	}
	if sessionA1 != sessionA2 {
		t.Fatalf("same cursorConversationId must produce stable execution session: %q vs %q", sessionA1, sessionA2)
	}
	if sessionA1 == sessionB {
		t.Fatalf("different cursorConversationIds must not share execution session: %q", sessionA1)
	}
}

func TestDeriveCursorSessionIDIsolatesByPrincipalSalt(t *testing.T) {
	// Same client-controlled inputs, different tenant principal salt → MUST
	// produce different execution sessions (no cross-tenant WS/connection
	// sharing). Empty salt (auth-disabled) is the unchanged single-tenant path.
	body := []byte(`{"model":"gpt-5.4","user":"user-a","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"input":[{"role":"user","content":"turn one"}]}`)

	tenantA := deriveCursorSessionID(body, "saltA")
	tenantB := deriveCursorSessionID(body, "saltB")
	noSalt := deriveCursorSessionID(body, "")

	if tenantA == "" || tenantB == "" {
		t.Fatalf("expected derivable sessions, got %q / %q", tenantA, tenantB)
	}
	if tenantA == tenantB {
		t.Fatalf("different principals must not share execution session: %q", tenantA)
	}
	if tenantA == noSalt || tenantB == noSalt {
		t.Fatalf("salted sessions must differ from unsalted: A=%q B=%q none=%q", tenantA, tenantB, noSalt)
	}
	// Stable within the same principal.
	if again := deriveCursorSessionID(body, "saltA"); again != tenantA {
		t.Fatalf("same principal+inputs must be stable: %q vs %q", again, tenantA)
	}
}

func TestSyntheticPromptCacheKeyIsolatesByPrincipal(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"hi"}]}`)

	cA, _ := newTestGinContext(t, "Cursor/1.0")
	cA.Set("userApiKey", "tenant-A-key")
	cB, _ := newTestGinContext(t, "Cursor/1.0")
	cB.Set("userApiKey", "tenant-B-key")

	kA := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(cA, body), "prompt_cache_key").String()
	kB := gjson.GetBytes(maybeInjectSyntheticPromptCacheKey(cB, body), "prompt_cache_key").String()

	if kA == "" || kB == "" {
		t.Fatalf("expected injected keys, got %q / %q", kA, kB)
	}
	if kA == kB {
		t.Fatalf("different principals must get different synthetic cache keys: %q", kA)
	}
}

func TestWithCursorExecutionSessionID_WrapsWhenSessionDerivable(t *testing.T) {
	bg := context.Background()
	cursorCtx, _ := newTestGinContext(t, "Cursor/1.0")

	cursorBody := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"hello"}]}`)
	wrapped := withCursorExecutionSessionID(bg, cursorCtx, cursorBody)
	if wrapped == bg {
		t.Fatalf("expected wrapped context to differ from input when session id derivable; got identical context")
	}

	plainBody := []byte(`{"model":"some-other-model","messages":[{"role":"user","content":"hi"}]}`)
	if got := withCursorExecutionSessionID(bg, cursorCtx, plainBody); got != bg {
		t.Fatalf("expected input context to be returned unchanged when session id not derivable; got wrapped context %p (input %p)", got, bg)
	}

	if got := withCursorExecutionSessionID(bg, cursorCtx, cursorBody); got == bg {
		t.Fatalf("expected second wrap to also differ from input")
	}
}

func TestWithCursorExecutionSessionIDRecordsCacheIdentity(t *testing.T) {
	bg := context.Background()
	cursorCtx, _ := newTestGinContext(t, "Cursor/1.0")
	body := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"conv-1"},"prompt_cache_key":"cli-proxy-cache","messages":[{"role":"user","content":"hello"}]}`)

	wrapped := withCursorExecutionSessionID(bg, cursorCtx, body)
	identity := internallogging.GetCacheIdentity(wrapped)
	if identity.ConversationID != "conv-1" {
		t.Fatalf("ConversationID = %q, want conv-1", identity.ConversationID)
	}
	if identity.PromptCacheKey != "cli-proxy-cache" {
		t.Fatalf("PromptCacheKey = %q, want cli-proxy-cache", identity.PromptCacheKey)
	}
}

func TestWithCursorExecutionSessionIDRequiresCursorUserAgent(t *testing.T) {
	bg := context.Background()
	nonCursorCtx, _ := newTestGinContext(t, "factory-cli/0.108.0")
	body := []byte(`{"model":"gpt-5.5","user":"generic-user","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"hello"}]}`)

	if got := withCursorExecutionSessionID(bg, nonCursorCtx, body); got != bg {
		t.Fatalf("non-Cursor chat request must not receive Cursor execution session context")
	}

	if got := withCursorExecutionSessionID(bg, nil, body); got != bg {
		t.Fatalf("nil gin context must not receive Cursor execution session context")
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

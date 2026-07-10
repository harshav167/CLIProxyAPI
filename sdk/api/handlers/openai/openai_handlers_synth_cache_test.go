package openai

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type executionSessionCaptureExecutor struct {
	sessionIDs []string
}

func (e *executionSessionCaptureExecutor) Identifier() string { return "openai" }
func (e *executionSessionCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	sessionID, _ := opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string)
	e.sessionIDs = append(e.sessionIDs, sessionID)
	return coreexecutor.Response{Payload: []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"metadata-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)}, nil
}

func (e *executionSessionCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	sessionID, _ := opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string)
	e.sessionIDs = append(e.sessionIDs, sessionID)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"metadata-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *executionSessionCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *executionSessionCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *executionSessionCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newExecutionSessionCaptureHandler(t *testing.T) (*OpenAIResponsesAPIHandler, *executionSessionCaptureExecutor) {
	t.Helper()
	executor := &executionSessionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "metadata-auth", Provider: "openai", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "metadata-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)), executor
}

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

func TestDeriveCursorSessionIDSaltsExplicitPromptCacheKey(t *testing.T) {
	// An explicit client-supplied prompt_cache_key must be salted per principal
	// for the derived session ID (so two tenants choosing the same key don't
	// collide onto one WebSocket), while staying stable within a principal.
	body := []byte(`{"model":"gpt-5.4","prompt_cache_key":"shared-key-123","input":[{"role":"user","content":"hi"}]}`)

	tenantA := deriveCursorSessionID(body, "saltA")
	tenantB := deriveCursorSessionID(body, "saltB")

	if tenantA == "" || tenantB == "" {
		t.Fatalf("expected derivable sessions, got %q / %q", tenantA, tenantB)
	}
	if tenantA == tenantB {
		t.Fatalf("same explicit prompt_cache_key under different principals must not collide: %q", tenantA)
	}
	// Must not leak the raw key verbatim into the session ID.
	if strings.Contains(tenantA, "shared-key-123") {
		t.Fatalf("derived session ID leaked raw prompt_cache_key: %q", tenantA)
	}
	// Stable within the same principal.
	if again := deriveCursorSessionID(body, "saltA"); again != tenantA {
		t.Fatalf("same principal+key must be stable: %q vs %q", again, tenantA)
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
	wrapped := withOpenAIExecutionSessionID(bg, cursorCtx, cursorBody)
	if wrapped == bg {
		t.Fatalf("expected wrapped context to differ from input when session id derivable; got identical context")
	}

	plainBody := []byte(`{"model":"some-other-model","messages":[{"role":"user","content":"hi"}]}`)
	if got := withOpenAIExecutionSessionID(bg, cursorCtx, plainBody); got != bg {
		t.Fatalf("expected input context to be returned unchanged when session id not derivable; got wrapped context %p (input %p)", got, bg)
	}

	if got := withOpenAIExecutionSessionID(bg, cursorCtx, cursorBody); got == bg {
		t.Fatalf("expected second wrap to also differ from input")
	}
}

func TestWithCursorExecutionSessionIDRecordsCacheIdentity(t *testing.T) {
	bg := context.Background()
	cursorCtx, _ := newTestGinContext(t, "Cursor/1.0")
	body := []byte(`{"model":"gpt-5.5","user":"cursor-user","metadata":{"cursorConversationId":"conv-1"},"prompt_cache_key":"cli-proxy-cache","messages":[{"role":"user","content":"hello"}]}`)

	wrapped := withOpenAIExecutionSessionID(bg, cursorCtx, body)
	identity := internallogging.GetCacheIdentity(wrapped)
	if identity.ConversationID != "conv-1" {
		t.Fatalf("ConversationID = %q, want conv-1", identity.ConversationID)
	}
	if identity.PromptCacheKey != "cli-proxy-cache" {
		t.Fatalf("PromptCacheKey = %q, want cli-proxy-cache", identity.PromptCacheKey)
	}
}

func TestWithOpenAIExecutionSessionIDSupportsNonCursorPrincipals(t *testing.T) {
	bg := context.Background()
	nonCursorCtx, _ := newTestGinContext(t, "factory-cli/0.108.0")
	nonCursorCtx.Set("userApiKey", "tenant-a-key")
	body := []byte(`{"model":"gpt-5.5","user":"generic-user","metadata":{"cursorConversationId":"77a73183-b276-4253-a768-ae20279c9e82"},"messages":[{"role":"user","content":"hello"}]}`)

	if got := withOpenAIExecutionSessionID(bg, nonCursorCtx, body); got == bg {
		t.Fatalf("non-Cursor request with a stable conversation must receive execution session context")
	}

	if got := withOpenAIExecutionSessionID(bg, nil, body); got != bg {
		t.Fatalf("nil gin context must not receive execution session context")
	}
}

func TestWithOpenAIExecutionSessionIDIsolatesSamePromptCacheKeyByPrincipal(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"cli-proxy-shared-client-key","messages":[{"role":"user","content":"hello"}]}`)
	tenantA, _ := newTestGinContext(t, "factory-cli/0.108.0")
	tenantA.Set("userApiKey", "tenant-a-key")
	tenantB, _ := newTestGinContext(t, "factory-cli/0.108.0")
	tenantB.Set("userApiKey", "tenant-b-key")

	sessionA := deriveOpenAIExecutionSessionID(tenantA, body)
	sessionB := deriveOpenAIExecutionSessionID(tenantB, body)
	if sessionA == "" || sessionB == "" {
		t.Fatalf("derived session IDs must be non-empty: %q / %q", sessionA, sessionB)
	}
	if sessionA == sessionB {
		t.Fatalf("different principals must receive distinct session IDs: %q", sessionA)
	}
}

func TestHandleNonStreamingResponseViaChatSetsExecutionSessionID(t *testing.T) {
	handler, executor := newExecutionSessionCaptureHandler(t)
	original := []byte(`{"model":"metadata-model","prompt_cache_key":"cli-proxy-session-key","input":"hello"}`)
	chat := []byte(`{"model":"metadata-model","messages":[{"role":"user","content":"hello"}]}`)
	for _, principal := range []string{"tenant-a-key", "tenant-a-key", "tenant-b-key"} {
		c, _ := newTestGinContext(t, "factory-cli/0.108.0")
		c.Set("userApiKey", principal)
		handler.handleNonStreamingResponseViaChat(c, original, chat)
	}
	assertRepeatedPrincipalExecutionSessions(t, executor.sessionIDs)
}

func TestHandleStreamingResponseViaChatSetsExecutionSessionID(t *testing.T) {
	handler, executor := newExecutionSessionCaptureHandler(t)
	original := []byte(`{"model":"metadata-model","prompt_cache_key":"cli-proxy-session-key","input":"hello"}`)
	chat := []byte(`{"model":"metadata-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	for _, principal := range []string{"tenant-a-key", "tenant-a-key", "tenant-b-key"} {
		c, _ := newTestGinContext(t, "factory-cli/0.108.0")
		c.Set("userApiKey", principal)
		handler.handleStreamingResponseViaChat(c, original, chat)
	}
	assertRepeatedPrincipalExecutionSessions(t, executor.sessionIDs)
}

func assertRepeatedPrincipalExecutionSessions(t *testing.T, sessionIDs []string) {
	t.Helper()
	if len(sessionIDs) != 3 {
		t.Fatalf("captured session IDs = %q, want three calls", sessionIDs)
	}
	if sessionIDs[0] == "" || sessionIDs[1] == "" || sessionIDs[2] == "" {
		t.Fatalf("session IDs must be non-empty: %q", sessionIDs)
	}
	if sessionIDs[0] != sessionIDs[1] {
		t.Fatalf("same principal/request IDs differ: %q", sessionIDs)
	}
	if sessionIDs[0] == sessionIDs[2] {
		t.Fatalf("different principal IDs collide: %q", sessionIDs)
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

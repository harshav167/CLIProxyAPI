package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func newOpenAIChatStreamTestHandler(t *testing.T) (*OpenAIAPIHandler, *httptest.ResponseRecorder, *gin.Context, http.Flusher) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	return h, recorder, c, flusher
}

func TestWriteConvertedResponsesChunkStillMapsReasoningForChatClients(t *testing.T) {
	_, recorder, c, _ := newOpenAIChatStreamTestHandler(t)

	var param any
	originalChat := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	responsesRequest := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}],"stream":true}`)
	chunk := []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`)

	writeConvertedResponsesChunk(c, context.Background(), "gpt-5.4", originalChat, responsesRequest, chunk, &param)
	body := recorder.Body.String()

	if !strings.Contains(body, `"reasoning_content":"thinking"`) {
		t.Fatalf("expected converted chat stream reasoning_content, got %s", body)
	}
	if strings.Contains(body, `"type":"response.reasoning_summary_text.delta"`) {
		t.Fatalf("converted chat stream should not expose raw Responses event, got %s", body)
	}
}

func TestShouldTreatAsResponsesFormatDetectsBodyDialect(t *testing.T) {
	responsesStyle := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}],"stream":true}`)
	chatStyle := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if !shouldTreatAsResponsesFormat(responsesStyle) {
		t.Fatal("Responses-shaped request should be detected")
	}
	if shouldTreatAsResponsesFormat(chatStyle) {
		t.Fatal("true chat-completions request should not be detected as Responses")
	}
}

func TestShouldRouteResponsesBodyViaCodexResponsesRequiresDialectAndProvider(t *testing.T) {
	modelName := "test-codex-route-model"
	registry.GetGlobalRegistry().RegisterClient("test-codex-route-client", "codex", []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient("test-codex-route-client")
	})

	codexResponsesStyle := []byte(`{"model":"test-codex-route-model","input":[{"role":"user","content":"hi"}],"stream":true}`)
	codexChatStyle := []byte(`{"model":"test-codex-route-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	claudeResponsesStyle := []byte(`{"model":"claude-opus-4-7","input":[{"role":"user","content":"hi"}],"stream":true}`)

	if !shouldRouteResponsesBodyViaCodexResponses(modelName, codexResponsesStyle) {
		t.Fatal("Responses-shaped Codex/OpenAI model request should route via Responses")
	}
	if shouldRouteResponsesBodyViaCodexResponses(modelName, codexChatStyle) {
		t.Fatal("true chat-completions Codex/OpenAI model request should stay on chat conversion path")
	}
	if shouldRouteResponsesBodyViaCodexResponses("claude-opus-4-7", claudeResponsesStyle) {
		t.Fatal("non-Codex provider request should not be routed through Codex Responses")
	}
}

func TestShouldRouteResponsesBodyViaCodexResponsesStripsThinkingSuffix(t *testing.T) {
	const baseModel = "gpt-5-codex"
	registry.GetGlobalRegistry().RegisterClient("test-codex-suffix-client", "codex", []*registry.ModelInfo{{ID: baseModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient("test-codex-suffix-client")
	})

	// A suffixed model (thinking suffix uses parenthesis syntax, e.g.
	// gpt-5-codex(high)) must still resolve to the codex provider and route via
	// Codex Responses. Before the suffix strip, GetProviderName looked up the
	// raw suffixed name, found no provider, and bypassed Codex Responses routing
	// + prompt-cache handling.
	const suffixedModel = "gpt-5-codex(high)"
	suffixed := []byte(`{"model":"gpt-5-codex(high)","input":[{"role":"user","content":"hi"}],"stream":true}`)
	if !shouldRouteResponsesBodyViaCodexResponses(suffixedModel, suffixed) {
		t.Fatal("suffixed Codex model request should route via Responses after suffix strip")
	}

	// And the bare base model still routes (no regression).
	bare := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"hi"}],"stream":true}`)
	if !shouldRouteResponsesBodyViaCodexResponses(baseModel, bare) {
		t.Fatal("bare Codex model request should still route via Responses")
	}
}

func TestStripCursorMetadataForCodexResponsesOnlyForCursor(t *testing.T) {
	_, _, cursorCtx, _ := newOpenAIChatStreamTestHandler(t)
	cursorCtx.Request.Header.Set("User-Agent", "Cursor/1.0")
	body := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"conv","cursorRequestId":"req"},"input":[{"role":"user","content":"hi"}],"stream":true}`)

	stripped := stripCursorMetadataForCodexResponses(cursorCtx, body)
	if gjson.GetBytes(stripped, "metadata").Exists() {
		t.Fatalf("Cursor Codex Responses payload should not forward unsupported metadata: %s", string(stripped))
	}
	if !gjson.GetBytes(body, "metadata.cursorConversationId").Exists() {
		t.Fatal("test fixture should retain metadata for local cache/session derivation before stripping")
	}

	_, _, otherCtx, _ := newOpenAIChatStreamTestHandler(t)
	otherCtx.Request.Header.Set("User-Agent", "factory-cli/0.108.0")
	unchanged := stripCursorMetadataForCodexResponses(otherCtx, body)
	if !gjson.GetBytes(unchanged, "metadata").Exists() {
		t.Fatalf("non-Cursor payload metadata should be left unchanged: %s", string(unchanged))
	}
}

func TestCursorMetadataFeedsCacheKeyBeforeUpstreamStrip(t *testing.T) {
	_, _, cursorCtx, _ := newOpenAIChatStreamTestHandler(t)
	cursorCtx.Request.Header.Set("User-Agent", "Cursor/1.0")

	body := []byte(`{"model":"gpt-5.5","metadata":{"cursorConversationId":"conv","cursorRequestId":"req"},"input":[{"role":"user","content":"turn one"}],"stream":true}`)
	withCacheKey := maybeInjectSyntheticPromptCacheKey(cursorCtx, body)
	cacheKey := gjson.GetBytes(withCacheKey, "prompt_cache_key").String()
	if cacheKey == "" {
		t.Fatalf("metadata.cursorConversationId should produce prompt_cache_key before upstream strip: %s", string(withCacheKey))
	}
	if !gjson.GetBytes(withCacheKey, "metadata.cursorConversationId").Exists() {
		t.Fatalf("metadata must remain available until the upstream compatibility strip runs: %s", string(withCacheKey))
	}

	stripped := stripCursorMetadataForCodexResponses(cursorCtx, withCacheKey)
	if gjson.GetBytes(stripped, "metadata").Exists() {
		t.Fatalf("metadata should be stripped from upstream Cursor Codex payload: %s", string(stripped))
	}
	if got := gjson.GetBytes(stripped, "prompt_cache_key").String(); got != cacheKey {
		t.Fatalf("prompt_cache_key must survive metadata strip; got %q want %q", got, cacheKey)
	}
}

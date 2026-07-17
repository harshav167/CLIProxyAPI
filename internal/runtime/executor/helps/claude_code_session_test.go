package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestExtractClaudeCodeSessionIDFromPayloadJSON(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"cache-session-1\"}"}}`)
	got := ExtractClaudeCodeSessionID(context.Background(), payload, nil)
	if got != "cache-session-1" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want cache-session-1", got)
	}
}

func TestExtractClaudeCodeSessionIDFromHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set(ClaudeCodeSessionHeader, "header-session-1")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	got := ExtractClaudeCodeSessionID(ctx, []byte(`{"model":"gpt-5.4"}`), nil)
	if got != "header-session-1" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want header-session-1", got)
	}
}

func TestExtractClaudeCodeCacheScopeIDPrefersDedicatedHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set(ClaudeCodeCacheSessionHeader, "shared-cache-scope")
	ginCtx.Request.Header.Set(ClaudeCodeSessionHeader, "child-session")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	got := ExtractClaudeCodeCacheScopeID(ctx, []byte(`{"metadata":{"user_id":"{\"session_id\":\"payload-session\"}"}}`), nil)
	if got != "shared-cache-scope" {
		t.Fatalf("ExtractClaudeCodeCacheScopeID() = %q, want shared-cache-scope", got)
	}
}

func TestExtractClaudeCodeCacheScopeIDFallsBackToSessionID(t *testing.T) {
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "session-fallback")

	got := ExtractClaudeCodeCacheScopeID(context.Background(), []byte(`{"metadata":{"user_id":"{\"session_id\":\"payload-session\"}"}}`), headers)
	if got != "session-fallback" {
		t.Fatalf("ExtractClaudeCodeCacheScopeID() = %q, want session-fallback", got)
	}
}

func TestClaudeCodePromptCacheIDStableAcrossRequests(t *testing.T) {
	ctx := context.Background()
	payload := []byte(`{"metadata":{"user_id":"{\"session_id\":\"cache-session-2\"}"}}`)
	first, ok := ClaudeCodePromptCacheID(ctx, "grok-composer-2.5-fast", payload, nil)
	if !ok || first == "" {
		t.Fatalf("ClaudeCodePromptCacheID first = %q, ok=%v, want cached id", first, ok)
	}
	second, ok := ClaudeCodePromptCacheID(ctx, "grok-composer-2.5-fast", payload, nil)
	if !ok || second != first {
		t.Fatalf("second cache id = %q, want %q", second, first)
	}
}

func TestClaudeCodePromptCacheIDSharedAcrossChildSessions(t *testing.T) {
	ctx := context.Background()
	parentHeaders := http.Header{}
	parentHeaders.Set(ClaudeCodeCacheSessionHeader, "root-cache-scope")
	parentHeaders.Set(ClaudeCodeSessionHeader, "parent-session")
	childHeaders := http.Header{}
	childHeaders.Set(ClaudeCodeCacheSessionHeader, "root-cache-scope")
	childHeaders.Set(ClaudeCodeSessionHeader, "workflow-child-session")

	parent, ok := ClaudeCodePromptCacheID(ctx, "gpt-5.6-sol", []byte(`{"metadata":{"user_id":"{\"session_id\":\"parent-session\"}"}}`), parentHeaders)
	if !ok || parent == "" {
		t.Fatalf("ClaudeCodePromptCacheID parent = %q, ok=%v, want cached id", parent, ok)
	}
	child, ok := ClaudeCodePromptCacheID(ctx, "gpt-5.6-sol", []byte(`{"metadata":{"user_id":"{\"session_id\":\"workflow-child-session\"}"}}`), childHeaders)
	if !ok || child != parent {
		t.Fatalf("child cache id = %q, want parent cache id %q", child, parent)
	}
}

func TestClaudeCodePromptCacheIDConcurrentRequestsUseOneID(t *testing.T) {
	const requestCount = 32
	headers := http.Header{}
	headers.Set(ClaudeCodeCacheSessionHeader, "concurrent-cache-scope")

	ids := make(chan string, requestCount)
	errs := make(chan error, requestCount)
	var wg sync.WaitGroup
	for range requestCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cacheID, ok := ClaudeCodePromptCacheID(context.Background(), "gpt-5.6-sol", nil, headers)
			if !ok || cacheID == "" {
				errs <- fmt.Errorf("ClaudeCodePromptCacheID() = %q, ok=%v, want cached id", cacheID, ok)
				return
			}
			ids <- cacheID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
			continue
		}
		if id != want {
			t.Fatalf("concurrent cache id = %q, want %q", id, want)
		}
	}
}

func TestExtractClaudeCodeSessionIDPrefersHeaderOverPayload(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"{"session_id":"payload-session"}"}}`)
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "header-session")

	got := ExtractClaudeCodeSessionID(context.Background(), payload, headers)
	if got != "header-session" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want header-session", got)
	}
}

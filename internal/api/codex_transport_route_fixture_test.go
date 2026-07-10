package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type codexRouteUpstreamMode int

const (
	codexRouteUpstreamSuccess codexRouteUpstreamMode = iota
	codexRouteUpstreamDone
	codexRouteUpstreamCloseBeforeOutput
	codexRouteUpstreamIncompleteMaxOutputTokens
	codexRouteUpstreamIncompleteContentFilter
	codexRouteUpstreamFailed
	codexRouteUpstreamResponseError
	codexRouteUpstreamTopLevelError
)

type codexTransportRouteFixture struct {
	server       *Server
	wsRequests   atomic.Int32
	httpRequests atomic.Int32
	mu           sync.Mutex
	upstreamBody [][]byte
}

func newCodexTransportRouteFixture(t *testing.T, configYAML string, mode codexRouteUpstreamMode) *codexTransportRouteFixture {
	t.Helper()
	fixture := &codexTransportRouteFixture{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			fixture.wsRequests.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("read websocket request: %v", errRead)
				return
			}
			fixture.recordUpstreamBody(payload)
			if mode == codexRouteUpstreamCloseBeforeOutput {
				return
			}
			for _, event := range codexRouteEvents(mode) {
				if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
					t.Errorf("write websocket event: %v", errWrite)
					return
				}
			}
			return
		}

		fixture.httpRequests.Add(1)
		body, errRead := ioReadAllAndClose(r)
		if errRead != nil {
			t.Errorf("read HTTP request: %v", errRead)
			return
		}
		fixture.recordUpstreamBody(body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range codexRouteEvents(mode) {
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(event)
			_, _ = w.Write([]byte("\n\n"))
		}
	}))
	t.Cleanup(upstream.Close)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	cfg, err := proxyconfig.ParseConfigBytes([]byte(configYAML))
	if err != nil {
		t.Fatalf("parse fixture config: %v", err)
	}
	cfg.SDKConfig.APIKeys = []string{"test-key"}
	cfg.SDKConfig.DisableImageGeneration = proxyconfig.DisableImageGenerationAll
	cfg.Port = 0
	cfg.AuthDir = authDir
	cfg.Debug = true
	cfg.LoggingToFile = false
	cfg.UsageStatisticsEnabled = false
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	auth := &coreauth.Auth{
		ID:         "codex-route-auth",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-test", "base_url": upstream.URL, "websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5-codex"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	fixture.server = NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(tmpDir, "config.yaml"))
	return fixture
}

func (f *codexTransportRouteFixture) recordUpstreamBody(payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upstreamBody = append(f.upstreamBody, bytes.Clone(payload))
}

func (f *codexTransportRouteFixture) request(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cursor/1.0")
	recorder := httptest.NewRecorder()
	f.server.engine.ServeHTTP(recorder, req)
	return recorder
}

func codexRouteEvents(mode codexRouteUpstreamMode) [][]byte {
	switch mode {
	case codexRouteUpstreamDone:
		return [][]byte{[]byte(`{"type":"response.done","response":{"id":"resp-done","status":"completed","output":[{"type":"message","id":"msg-done","role":"assistant","content":[{"type":"output_text","text":"done answer"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`)}
	case codexRouteUpstreamIncompleteMaxOutputTokens:
		return [][]byte{[]byte(`{"type":"response.incomplete","response":{"id":"resp-incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`)}
	case codexRouteUpstreamIncompleteContentFilter:
		return [][]byte{[]byte(`{"type":"response.incomplete","response":{"id":"resp-filtered","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}}`)}
	case codexRouteUpstreamFailed:
		return [][]byte{[]byte(`{"type":"response.failed","response":{"id":"resp-failed","status":"failed","error":{"code":"server_error","message":"failed turn"}}}`)}
	case codexRouteUpstreamResponseError:
		return [][]byte{[]byte(`{"type":"response.error","response":{"id":"resp-error","status":"failed","error":{"code":"server_error","message":"response error"}}}`)}
	case codexRouteUpstreamTopLevelError:
		return [][]byte{[]byte(`{"type":"error","error":{"code":"server_error","message":"top-level error"}}`)}
	}
	return [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-route","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs-1","summary":[]}}`),
		[]byte(`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs-1","delta":"route reasoning"}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs-1","summary":[{"type":"summary_text","text":"route reasoning"}]}}`),
		[]byte(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":""}}`),
		[]byte(`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc-1","delta":"{}"}`),
		[]byte(`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc-1","arguments":"{}"}`),
		[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}}`),
		[]byte(`{"type":"response.output_item.added","output_index":2,"item":{"type":"message","id":"msg-1","role":"assistant","content":[]}}`),
		[]byte(`{"type":"response.output_text.delta","output_index":2,"item_id":"msg-1","delta":"route answer"}`),
		[]byte(`{"type":"response.output_item.done","output_index":2,"item":{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"route answer"}]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp-route","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`),
	}
}

func ioReadAllAndClose(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

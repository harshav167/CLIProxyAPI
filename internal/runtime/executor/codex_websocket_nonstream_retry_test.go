package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketExecuteSendErrorRetryRebindsReader(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	firstAccepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if connections.Add(1) == 1 {
			close(firstAccepted)
			_, _, _ = conn.ReadMessage()
			return
		}
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read retried request: %v", errRead)
			return
		}
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-retried","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed response: %v", errWrite)
		}
	}))
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	staleConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial stale websocket: %v", err)
	}
	<-firstAccepted
	if errClose := staleConn.Close(); errClose != nil {
		t.Fatalf("close stale websocket: %v", errClose)
	}

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-send-retry", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	sessionID := "execute-send-error-redial"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = staleConn
	sess.connCreatedAt = time.Now()
	sess.readerConn = staleConn
	sess.wsURL = wsURL
	sess.authID = auth.ID
	sess.connGeneration = 1
	sess.windowGen = 1
	sess.connMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"retry"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "resp-retried" {
		t.Fatalf("response id = %q, want resp-retried; payload=%s", got, resp.Payload)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want stale plus one retry", got)
	}
	sess.activeMu.Lock()
	active := sess.active
	sess.activeMu.Unlock()
	if active != nil {
		t.Fatal("replacement reader remained active after Execute returned")
	}
}

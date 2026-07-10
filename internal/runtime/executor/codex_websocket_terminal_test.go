package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketFailureTerminalClosesTurn(t *testing.T) {
	for _, eventType := range []string{"response.failed", "response.incomplete", "response.error"} {
		t.Run(eventType, func(t *testing.T) {
			release := make(chan struct{})
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					t.Errorf("read websocket request: %v", errRead)
					return
				}
				payload := []byte(`{"type":"` + eventType + `","response":{"id":"resp-terminal"}}`)
				if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
					t.Errorf("write terminal event: %v", errWrite)
					return
				}
				<-release
			}))
			t.Cleanup(func() {
				close(release)
				server.Close()
			})

			exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
			auth := &cliproxyauth.Auth{ID: "auth-terminal", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
			sessionID := "failure-terminal-" + eventType
			t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"test"}]}`)}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
			}

			result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, req, opts)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			select {
			case chunk := <-result.Chunks:
				if got := gjson.GetBytes(chunk.Payload, "type").String(); got != eventType {
					t.Fatalf("terminal payload type = %q, want %q", got, eventType)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for terminal payload")
			}
			select {
			case _, open := <-result.Chunks:
				if open {
					t.Fatal("stream remained open after failure terminal")
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("failure terminal did not close the turn")
			}
		})
	}
}

func TestCodexWebsocketExecuteReturnsIncompleteTerminal(t *testing.T) {
	payload := []byte(`{"type":"response.incomplete","response":{"id":"resp-incomplete","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	server := newCodexNonStreamTerminalServer(t, payload)
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	sessionID := "execute-incomplete"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := exec.Execute(ctx,
		&cliproxyauth.Auth{ID: "auth-incomplete", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}},
		cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"test"}]}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "resp-incomplete" {
		t.Fatalf("response id = %q, want resp-incomplete; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "status").String(); got != "incomplete" {
		t.Fatalf("response status = %q, want incomplete; payload=%s", got, resp.Payload)
	}
	if gjson.GetBytes(resp.Payload, "type").Exists() {
		t.Fatalf("raw event wrapper leaked: %s", resp.Payload)
	}
}

func TestCodexWebsocketExecuteRejectsUnknownResponseIncompleteReasonForChatCompletion(t *testing.T) {
	payload := []byte(`{"type":"response.incomplete","response":{"id":"resp-incomplete","status":"incomplete","incomplete_details":{"reason":"provider_limit"},"output":[]}}`)
	server := newCodexNonStreamTerminalServer(t, payload)
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	sessionID := "execute-unknown-incomplete"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := exec.Execute(ctx,
		&cliproxyauth.Auth{ID: "auth-incomplete", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}},
		cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"test"}]}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}},
	)
	if err == nil {
		t.Fatal("Execute() error = nil, want untranslatable incomplete error")
	}
	statusCoder, ok := err.(interface{ StatusCode() int })
	if !ok || statusCoder.StatusCode() != http.StatusBadGateway {
		t.Fatalf("Execute() error = %v, want status 502", err)
	}
	want := "codex terminal event response.incomplete could not be translated to openai"
	if err.Error() != want {
		t.Fatalf("Execute() error = %q, want %q", err.Error(), want)
	}
}

func newCodexNonStreamTerminalServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read websocket request: %v", errRead)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
			t.Errorf("write terminal event: %v", errWrite)
			return
		}
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

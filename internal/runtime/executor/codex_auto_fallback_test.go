package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexConnectionLimitEvent = []byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"}}`)

type codexAutoFallbackFixture struct {
	exec         *CodexAutoExecutor
	auth         *cliproxyauth.Auth
	req          cliproxyexecutor.Request
	opts         cliproxyexecutor.Options
	wsRequests   atomic.Int32
	httpRequests atomic.Int32
	wsPayloads   chan []byte
}

func newCodexAutoFallbackFixture(t *testing.T, wsEvents func(int32) [][]byte) *codexAutoFallbackFixture {
	t.Helper()
	fixture := &codexAutoFallbackFixture{}
	fixture.wsPayloads = make(chan []byte, 8)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			attempt := fixture.wsRequests.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, payload, errRead := conn.ReadMessage(); errRead != nil {
				t.Errorf("read websocket request: %v", errRead)
				return
			} else {
				fixture.wsPayloads <- bytes.Clone(payload)
			}
			for _, event := range wsEvents(attempt) {
				if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
					t.Errorf("write websocket event: %v", errWrite)
					return
				}
			}
			return
		}

		fixture.httpRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg-http\",\"delta\":\"http fallback\"}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexResponseChaining.Enabled = true
	fixture.exec = NewCodexAutoExecutor(cfg)
	fixture.auth = &cliproxyauth.Auth{ID: "auth-" + t.Name(), Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	fixture.req = cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`),
	}
	fixture.opts = cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "session-" + t.Name()},
	}
	t.Cleanup(func() { fixture.exec.CloseExecutionSession("session-" + t.Name()) })
	return fixture
}

func TestCodexAutoExecuteStreamConnectionLimitRetriesFreshWebsocket(t *testing.T) {
	fixture := newCodexAutoFallbackFixture(t, func(attempt int32) [][]byte {
		if attempt == 1 {
			return [][]byte{codexConnectionLimitEvent}
		}
		return [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp-ws","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)}
	})

	result, err := fixture.exec.ExecuteStream(context.Background(), fixture.auth, fixture.req, fixture.opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)
	if got := fixture.wsRequests.Load(); got != 2 {
		t.Fatalf("websocket requests = %d, want 2", got)
	}
	if got := fixture.httpRequests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestCodexAutoExecuteStreamConnectionLimitFallsBackToHTTPAfterRetry(t *testing.T) {
	fixture := newCodexAutoFallbackFixture(t, func(int32) [][]byte {
		return [][]byte{codexConnectionLimitEvent}
	})

	result, err := fixture.exec.ExecuteStream(context.Background(), fixture.auth, fixture.req, fixture.opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads := drainStreamPayloads(t, result)
	if !bytes.Contains(bytes.Join(payloads, nil), []byte("http fallback")) {
		t.Fatalf("fallback payloads = %q, want HTTP response", payloads)
	}
	if got := fixture.wsRequests.Load(); got != 2 {
		t.Fatalf("websocket requests = %d, want 2", got)
	}
	if got := fixture.httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestCodexAutoExecuteStreamReadCloseBeforeOutputFallsBackToHTTP(t *testing.T) {
	fixture := newCodexAutoFallbackFixture(t, func(int32) [][]byte { return nil })

	result, err := fixture.exec.ExecuteStream(context.Background(), fixture.auth, fixture.req, fixture.opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)
	if got := fixture.wsRequests.Load(); got != 1 {
		t.Fatalf("websocket requests = %d, want 1", got)
	}
	if got := fixture.httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestCodexAutoExecuteStreamPostStartErrorDoesNotRetryOrFallback(t *testing.T) {
	delta := []byte(`{"type":"response.output_text.delta","output_index":0,"item_id":"msg-1","delta":"started"}`)
	fixture := newCodexAutoFallbackFixture(t, func(int32) [][]byte {
		return [][]byte{delta, codexConnectionLimitEvent}
	})

	result, err := fixture.exec.ExecuteStream(context.Background(), fixture.auth, fixture.req, fixture.opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	select {
	case chunk := <-result.Chunks:
		if !bytes.Contains(chunk.Payload, []byte("started")) {
			t.Fatalf("first payload = %q, want started delta", chunk.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first payload")
	}
	select {
	case chunk := <-result.Chunks:
		if chunk.Err == nil {
			t.Fatalf("post-start chunk error = nil; payload=%q", chunk.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-start error")
	}
	select {
	case chunk, ok := <-result.Chunks:
		if ok {
			t.Fatalf("unexpected third stream chunk: payload=%q error=%v", chunk.Payload, chunk.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-start stream closure")
	}
	if got := fixture.wsRequests.Load(); got != 1 {
		t.Fatalf("websocket requests = %d, want 1", got)
	}
	if got := fixture.httpRequests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestWrapBridgedStreamForCaptureStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input := make(chan cliproxyexecutor.StreamChunk)
	exec := NewCodexAutoExecutor(&config.Config{})
	result := exec.wrapBridgedStreamForCapture(
		ctx,
		"capture-cancel",
		"gpt-5-codex",
		"auth-cancel",
		[]byte(`{"model":"gpt-5-codex","input":[]}`),
		&cliproxyexecutor.StreamResult{Chunks: input},
		bridgeTurnTelemetry{},
	)

	input <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.output_text.delta","delta":"started"}`)}
	cancel()

	select {
	case _, ok := <-result.Chunks:
		if ok {
			t.Fatal("wrapped stream remained open after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for capture wrapper to stop")
	}
}

func TestCodexAutoExecuteStreamPreviousResponseMissRetriesExactFullContextOnce(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()
	fixture := newCodexAutoFallbackFixture(t, func(attempt int32) [][]byte {
		if attempt == 1 {
			return [][]byte{[]byte(`{"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"No response found for previous_response_id resp-stale"}}}`)}
		}
		return [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp-recovered","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)}
	})
	sessionKey := fixture.opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string)
	bridge := getHTTPWSBridge()
	bridge.Reset(sessionKey)
	fullInput := []byte(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"reasoning","id":"rs_1","encrypted_content":"` + validEncryptedContent + `"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok","phase":"final"}
	]`)
	fixture.req.Payload, _ = sjson.SetRawBytes(fixture.req.Payload, "input", fullInput)
	baselinePayload, _ := sjson.SetRawBytes(fixture.req.Payload, "input", []byte(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"reasoning","id":"rs_1","encrypted_content":"`+validEncryptedContent+`"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool","arguments":"{}"}
	]`))
	bridge.CaptureResponse(sessionKey, "resp-stale", fixture.req.Model, fixture.auth.ID, baselinePayload, nil)

	result, err := fixture.exec.ExecuteStream(context.Background(), fixture.auth, fixture.req, fixture.opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	firstPayload := waitPayload(t, fixture.wsPayloads)
	secondPayload := waitPayload(t, fixture.wsPayloads)
	if got := gjson.GetBytes(firstPayload, "previous_response_id").String(); got != "resp-stale" {
		t.Fatalf("first previous_response_id = %q, want resp-stale; payload=%s", got, firstPayload)
	}
	if got := gjson.GetBytes(secondPayload, "previous_response_id").String(); got != "" {
		t.Fatalf("recovery leaked previous_response_id=%q; payload=%s", got, secondPayload)
	}
	if got := gjson.GetBytes(secondPayload, "input").Raw; got != gjson.GetBytes(fixture.req.Payload, "input").Raw {
		t.Fatalf("recovery input changed\n got: %s\nwant: %s", got, gjson.GetBytes(fixture.req.Payload, "input").Raw)
	}
	if got := fixture.wsRequests.Load(); got != 2 {
		t.Fatalf("websocket requests = %d, want one delta plus one full retry", got)
	}
	if delta, previousResponseID := bridge.ComputeDelta(sessionKey, fixture.req.Payload, fixture.auth.ID); delta != nil || previousResponseID == "resp-stale" {
		t.Fatalf("recovered bridge state retained stale previous response: delta=%s previous_response_id=%q", delta, previousResponseID)
	}
}

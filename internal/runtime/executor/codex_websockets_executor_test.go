package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body, nil)

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestCodexWebsocketsExecutePreservesPreviousResponseIDUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %s, want /responses", r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream websocket message: %v", err)
		}
		if msgType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", msgType)
		}
		capturedPayload <- bytes.Clone(payload)

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("upstream type = %s, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-1" {
			t.Fatalf("upstream previous_response_id = %s, want resp-1; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestCodexWebsocketsExecuteStreamPassesThroughUpstreamWebsocketPayloadForDownstreamWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	delta := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)
		if errWrite := conn.WriteMessage(websocket.TextMessage, delta); errWrite != nil {
			t.Errorf("write delta websocket message: %v", errWrite)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"prolite/gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before first chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("first chunk error = %v", chunk.Err)
		}
		if !bytes.Equal(bytes.TrimSpace(chunk.Payload), delta) {
			t.Fatalf("first chunk = %q, want raw upstream websocket payload %q", chunk.Payload, delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first stream chunk")
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5-codex" {
			t.Fatalf("upstream model = %s, want gpt-5-codex; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestCodexWebsocketFirstFrameUsesFinalIdentityWindow(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedHeaders := make(chan http.Header, 1)
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-window","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.Codex.IdentityConfuse = true
	cfg.Routing.SessionAffinity = true
	exec := NewCodexWebsocketsExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-window", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	sessionID := "window-session"
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"client-cache","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	headers := <-capturedHeaders
	payload := <-capturedPayload
	headerWindow := headers.Get("X-Codex-Window-Id")
	frameWindow := gjson.GetBytes(payload, "client_metadata.x-codex-window-id").String()
	if headerWindow == "" || frameWindow == "" {
		t.Fatalf("window id missing: header=%q frame=%q payload=%s", headerWindow, frameWindow, payload)
	}
	if headerWindow != frameWindow {
		t.Fatalf("window id mismatch: header=%q frame=%q payload=%s", headerWindow, frameWindow, payload)
	}
	expectedPromptCacheKey := codexIdentityConfuseUUID(auth.ID, "prompt-cache", "client-cache")
	if got := gjson.GetBytes(payload, "prompt_cache_key").String(); got != expectedPromptCacheKey {
		t.Fatalf("first frame prompt_cache_key = %q, want %q payload=%s", got, expectedPromptCacheKey, payload)
	}
	if headerWindow != expectedPromptCacheKey+":1" {
		t.Fatalf("identity-confused window = %q, want %q", headerWindow, expectedPromptCacheKey+":1")
	}
}

func TestCodexWebsocketReconnectFirstFrameUsesNewWindow(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedHeaders := make(chan http.Header, 2)
	capturedPayloads := make(chan []byte, 3)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders <- r.Header.Clone()
		responseHeaders := http.Header{}
		if connections.Add(1) == 1 {
			responseHeaders.Set("X-Codex-Turn-State", "stale-turn-state")
		}
		conn, err := upgrader.Upgrade(w, r, responseHeaders)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			capturedPayloads <- bytes.Clone(payload)
			completed := []byte(`{"type":"response.completed","response":{"id":"resp-window","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.Codex.IdentityConfuse = true
	cfg.Routing.SessionAffinity = true
	exec := NewCodexWebsocketsExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-reconnect", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	sessionID := "reconnect-window-session"
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"client-reconnect-cache","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.connCreatedAt = time.Now().Add(-codexWebsocketConnectionMaxAge - time.Second)
	sess.connMu.Unlock()

	result, err = exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("second ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	result, err = exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("third ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	firstHeaders := <-capturedHeaders
	secondHeaders := <-capturedHeaders
	firstPayload := <-capturedPayloads
	secondPayload := <-capturedPayloads
	thirdPayload := <-capturedPayloads
	firstHeader := firstHeaders.Get("X-Codex-Window-Id")
	secondHeader := secondHeaders.Get("X-Codex-Window-Id")
	firstFrame := gjson.GetBytes(firstPayload, "client_metadata.x-codex-window-id").String()
	secondFrame := gjson.GetBytes(secondPayload, "client_metadata.x-codex-window-id").String()
	thirdFrame := gjson.GetBytes(thirdPayload, "client_metadata.x-codex-window-id").String()
	if firstHeader != firstFrame || secondHeader != secondFrame || secondHeader != thirdFrame {
		t.Fatalf("header/frame mismatch: first=%q/%q second=%q/%q third=%q", firstHeader, firstFrame, secondHeader, secondFrame, thirdFrame)
	}
	expectedPromptCacheKey := codexIdentityConfuseUUID(auth.ID, "prompt-cache", "client-reconnect-cache")
	if firstHeader != expectedPromptCacheKey+":1" {
		t.Fatalf("first window = %q, want %q", firstHeader, expectedPromptCacheKey+":1")
	}
	if secondHeader != expectedPromptCacheKey+":2" {
		t.Fatalf("reconnect window = %q, want %q", secondHeader, expectedPromptCacheKey+":2")
	}
	if got := secondHeaders.Get("X-Codex-Turn-State"); got != "" {
		t.Fatalf("replacement handshake turn state = %q, want empty", got)
	}
	if got := gjson.GetBytes(secondPayload, "client_metadata.x-codex-turn-state").String(); got != "" {
		t.Fatalf("replacement frame turn state = %q, want empty", got)
	}
}

func TestCodexWebsocketsExecuteStreamRequestedAliasPayloadOverrideWinsOverRequestReasoning(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-5.5-extra", Protocol: "codex"}},
					Params: map[string]any{
						"reasoning.context": "all_turns",
						"reasoning.effort":  "xhigh",
						"reasoning.summary": "detailed",
					},
				},
			},
		},
	}
	exec := NewCodexWebsocketsExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model: "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],` +
			`"reasoning":{"effort":"medium","summary":"auto"}}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.5-extra",
		},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	payload := waitPayload(t, capturedPayload)
	if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5.5" {
		t.Fatalf("upstream model = %q, want gpt-5.5; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning effort = %q, want xhigh; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "reasoning.summary").String(); got != "detailed" {
		t.Fatalf("reasoning summary = %q, want detailed; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "reasoning.context").String(); got != "all_turns" {
		t.Fatalf("reasoning context = %q, want all_turns; payload=%s", got, payload)
	}
}

func TestCodexWebsocketsExecuteStreamPropagatesUpstreamErrorForDownstreamWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	errorPayload := []byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, errorPayload); errWrite != nil {
			t.Errorf("write error websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before error chunk")
		}
		if len(bytes.TrimSpace(chunk.Payload)) != 0 {
			t.Fatalf("error chunk payload = %q, want empty", chunk.Payload)
		}
		if chunk.Err == nil {
			t.Fatal("error chunk Err = nil, want upstream error")
		}
		statusErr, ok := chunk.Err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("error type %T does not expose StatusCode", chunk.Err)
		}
		if got := statusErr.StatusCode(); got != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for error stream chunk")
	}
}

func TestCodexAutoExecuteStreamDownstreamWebsocketUsesSharedBridgeDelta(t *testing.T) {
	sessionID := "auto-direct-ws-delta"
	getHTTPWSBridge().Reset(sessionID)
	defer getHTTPWSBridge().Reset(sessionID)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayloads := make(chan []byte, 4)
	var requests atomic.Int32
	delta := []byte(`{"type":"response.output_text.delta","delta":"answer one"}`)
	outputDone := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]}}`)
	completed1 := []byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":10,"cached_tokens":0,"output_tokens":2,"total_tokens":12}}}`)
	completed2 := []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":4,"cached_tokens":4,"output_tokens":1,"total_tokens":5}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			capturedPayloads <- bytes.Clone(payload)

			switch requests.Add(1) {
			case 1:
				for _, event := range [][]byte{delta, outputDone, completed1} {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						t.Errorf("write first-turn websocket message: %v", errWrite)
						return
					}
				}
			case 2:
				if errWrite := conn.WriteMessage(websocket.TextMessage, completed2); errWrite != nil {
					t.Errorf("write second-turn websocket message: %v", errWrite)
				}
				return
			default:
				t.Errorf("unexpected request count")
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexResponseChaining.Enabled = true
	exec := NewCodexAutoExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-direct-delta", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	firstReq := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}]}`),
	}
	firstResult, err := exec.ExecuteStream(ctx, auth, firstReq, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream() error = %v", err)
	}
	firstPayloads := drainStreamPayloads(t, firstResult)
	if len(firstPayloads) == 0 || !bytes.Equal(bytes.TrimSpace(firstPayloads[0]), delta) {
		t.Fatalf("first downstream chunk = %q, want raw upstream websocket delta %q", firstPayloads, delta)
	}
	if !getHTTPWSBridge().HasSession(sessionID) {
		t.Fatal("shared bridge did not capture first downstream-WS turn")
	}

	secondReq := cliproxyexecutor.Request{
		Model: "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}` +
			`]}`),
	}
	secondResult, err := exec.ExecuteStream(ctx, auth, secondReq, opts)
	if err != nil {
		t.Fatalf("second ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, secondResult)

	firstPayload := waitPayload(t, capturedPayloads)
	if got := gjson.GetBytes(firstPayload, "previous_response_id").String(); got != "" {
		t.Fatalf("first upstream payload unexpectedly chained with previous_response_id=%q; payload=%s", got, firstPayload)
	}
	secondPayload := waitPayload(t, capturedPayloads)
	if got := gjson.GetBytes(secondPayload, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("second upstream previous_response_id = %q, want resp-1; payload=%s", got, secondPayload)
	}
	input := gjson.GetBytes(secondPayload, "input").Array()
	if len(input) != 1 {
		t.Fatalf("second upstream input length = %d, want 1 suffix item; payload=%s", len(input), secondPayload)
	}
	if got := gjson.GetBytes([]byte(input[0].Raw), "content.0.text").String(); got != "two" {
		t.Fatalf("delta suffix text = %q, want two; payload=%s", got, secondPayload)
	}
}

func TestCodexAutoExecuteStreamDownstreamWebsocketShapeMismatchFallsBackToFullSend(t *testing.T) {
	sessionID := "auto-direct-ws-shape-mismatch"
	getHTTPWSBridge().Reset(sessionID)
	defer getHTTPWSBridge().Reset(sessionID)
	getHTTPWSBridge().CaptureResponse(
		sessionID,
		"resp-old",
		"gpt-5-codex",
		"auth-shape",
		[]byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}]}`),
		[]byte(`{"response":{"id":"resp-old","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]}]}}`),
	)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-new","output":[],"usage":{"input_tokens":3,"cached_tokens":0,"output_tokens":1,"total_tokens":4}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write websocket completed: %v", errWrite)
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexResponseChaining.Enabled = true
	exec := NewCodexAutoExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-shape", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	req := cliproxyexecutor.Request{
		Model: "gpt-5-codex-extra",
		Payload: []byte(`{"model":"gpt-5-codex-extra","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}` +
			`]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}
	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	payload := waitPayload(t, capturedPayload)
	if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "" {
		t.Fatalf("shape mismatch must not send previous_response_id, got %q; payload=%s", got, payload)
	}
	if got := len(gjson.GetBytes(payload, "input").Array()); got != 3 {
		t.Fatalf("shape mismatch must send full input, got %d items; payload=%s", got, payload)
	}
}

func TestCodexAutoExecuteStreamDownstreamWebsocketContinueFoldOpensContinuation(t *testing.T) {
	sessionID := "auto-direct-ws-continue-fold"
	getHTTPWSBridge().Reset(sessionID)
	defer getHTTPWSBridge().Reset(sessionID)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayloads := make(chan []byte, 4)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			capturedPayloads <- bytes.Clone(payload)

			switch requests.Add(1) {
			case 1:
				events := [][]byte{
					[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1"}}`),
					[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1"}}`),
					[]byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}`),
					[]byte(`{"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"truncated answer"}}`),
					[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"truncated answer"}]}}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"output_tokens":516,"output_tokens_details":{"reasoning_tokens":516}}}}`),
				}
				for _, event := range events {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						t.Errorf("write first-round websocket message: %v", errWrite)
						return
					}
				}
			case 2:
				events := [][]byte{
					[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant"}}`),
					[]byte(`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_2","delta":"final answer"}}`),
					[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp-2","usage":{"output_tokens":20,"output_tokens_details":{"reasoning_tokens":20}}}}`),
				}
				for _, event := range events {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						t.Errorf("write continuation websocket message: %v", errWrite)
						return
					}
				}
				return
			default:
				t.Errorf("unexpected request count")
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexContinueThinking = &config.CodexContinueConfig{Enabled: true, MaxContinue: 1}
	exec := NewCodexAutoExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-direct-continue", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads := drainStreamPayloads(t, result)

	firstPayload := waitPayload(t, capturedPayloads)
	secondPayload := waitPayload(t, capturedPayloads)
	if got := gjson.GetBytes(firstPayload, "previous_response_id").String(); got != "" {
		t.Fatalf("first upstream previous_response_id = %q, want empty; payload=%s", got, firstPayload)
	}
	input := gjson.GetBytes(secondPayload, "input").Array()
	if len(input) != 3 {
		t.Fatalf("continuation input length = %d, want original + reasoning + marker; payload=%s", len(input), secondPayload)
	}
	if got := gjson.GetBytes([]byte(input[1].Raw), "encrypted_content").String(); got != "enc-1" {
		t.Fatalf("continuation did not replay encrypted reasoning, got %q; payload=%s", got, secondPayload)
	}
	if got := gjson.GetBytes([]byte(input[2].Raw), "phase").String(); got != "commentary" {
		t.Fatalf("continuation marker phase = %q, want commentary; payload=%s", got, secondPayload)
	}

	joined := string(bytes.Join(payloads, []byte("\n")))
	if strings.Contains(joined, "truncated answer") {
		t.Fatalf("truncated tentative answer leaked downstream:\n%s", joined)
	}
	if !strings.Contains(joined, "final answer") {
		t.Fatalf("final continuation answer missing downstream:\n%s", joined)
	}
	if strings.Contains(joined, `"response":{"id":"resp-1"`) {
		t.Fatalf("first truncated lifecycle leaked downstream:\n%s", joined)
	}
	if !strings.Contains(joined, `proxy_upstream_previous_response_id":"resp-2"`) {
		t.Fatalf("folded terminal missing final upstream id metadata:\n%s", joined)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream request count = %d, want 2", got)
	}
}

func TestCodexAutoExecuteStreamChatCompletionsBridgeContinueFoldOpensContinuation(t *testing.T) {
	sessionID := "auto-chat-completions-continue-fold"
	getHTTPWSBridge().Reset(sessionID)
	defer getHTTPWSBridge().Reset(sessionID)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayloads := make(chan []byte, 4)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			capturedPayloads <- bytes.Clone(payload)

			switch requests.Add(1) {
			case 1:
				events := [][]byte{
					[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-bridge"}}`),
					[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-bridge"}}`),
					[]byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}`),
					[]byte(`{"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"bridge truncated"}}`),
					[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"bridge truncated"}]}}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp-bridge-1","usage":{"output_tokens":516,"output_tokens_details":{"reasoning_tokens":516}}}}`),
				}
				for _, event := range events {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						t.Errorf("write first-round websocket message: %v", errWrite)
						return
					}
				}
			case 2:
				events := [][]byte{
					[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant"}}`),
					[]byte(`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_2","delta":"bridge final"}}`),
					[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"bridge final"}]}}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp-bridge-2","usage":{"output_tokens":20,"output_tokens_details":{"reasoning_tokens":20}}}}`),
				}
				for _, event := range events {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						t.Errorf("write continuation websocket message: %v", errWrite)
						return
					}
				}
				return
			default:
				t.Errorf("unexpected request count")
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexResponseChaining.Enabled = true
	cfg.CodexContinueThinking = &config.CodexContinueConfig{Enabled: true, MaxContinue: 1}
	exec := NewCodexAutoExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-bridge-continue", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"one"}],"stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads := drainStreamPayloads(t, result)

	waitPayload(t, capturedPayloads)
	secondPayload := waitPayload(t, capturedPayloads)
	input := gjson.GetBytes(secondPayload, "input").Array()
	if len(input) < 3 {
		t.Fatalf("continuation input length = %d, want original + reasoning + marker; payload=%s", len(input), secondPayload)
	}
	if got := gjson.GetBytes([]byte(input[len(input)-2].Raw), "encrypted_content").String(); got != "enc-bridge" {
		t.Fatalf("continuation did not replay encrypted reasoning, got %q; payload=%s", got, secondPayload)
	}
	if got := gjson.GetBytes([]byte(input[len(input)-1].Raw), "phase").String(); got != "commentary" {
		t.Fatalf("continuation marker phase = %q, want commentary; payload=%s", got, secondPayload)
	}

	joined := string(bytes.Join(payloads, []byte("\n")))
	if strings.Contains(joined, "bridge truncated") {
		t.Fatalf("truncated tentative answer leaked downstream:\n%s", joined)
	}
	if !strings.Contains(joined, "bridge final") {
		t.Fatalf("final continuation answer missing downstream:\n%s", joined)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream request count = %d, want 2", got)
	}
}

func TestCodexAutoExecuteStreamDownstreamWebsocketContinueFoldDisabledDoesNotContinue(t *testing.T) {
	sessionID := "auto-direct-ws-continue-disabled"
	getHTTPWSBridge().Reset(sessionID)
	defer getHTTPWSBridge().Reset(sessionID)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayloads := make(chan []byte, 2)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		requests.Add(1)
		capturedPayloads <- bytes.Clone(payload)
		events := [][]byte{
			[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-disabled"}}`),
			[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-disabled"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp-disabled","usage":{"output_tokens":516,"output_tokens_details":{"reasoning_tokens":516}}}}`),
		}
		for _, event := range events {
			if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
				t.Errorf("write websocket message: %v", errWrite)
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexContinueThinking = &config.CodexContinueConfig{Enabled: false, MaxContinue: 1}
	exec := NewCodexAutoExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-continue-disabled", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads := drainStreamPayloads(t, result)
	waitPayload(t, capturedPayloads)
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream request count = %d, want 1", got)
	}
	if !strings.Contains(string(bytes.Join(payloads, []byte("\n"))), "resp-disabled") {
		t.Fatalf("disabled fold should forward first terminal downstream, payloads=%q", payloads)
	}
}

func TestCodexAutoExecuteStreamDownstreamWebsocketContinueFoldMissingEncryptedDoesNotContinue(t *testing.T) {
	sessionID := "auto-direct-ws-continue-no-encrypted"
	getHTTPWSBridge().Reset(sessionID)
	defer getHTTPWSBridge().Reset(sessionID)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayloads := make(chan []byte, 2)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		requests.Add(1)
		capturedPayloads <- bytes.Clone(payload)
		events := [][]byte{
			[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`),
			[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`),
			[]byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}`),
			[]byte(`{"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"fallback answer"}}`),
			[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback answer"}]}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp-no-enc","usage":{"output_tokens":516,"output_tokens_details":{"reasoning_tokens":516}}}}`),
		}
		for _, event := range events {
			if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
				t.Errorf("write websocket message: %v", errWrite)
				return
			}
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexContinueThinking = &config.CodexContinueConfig{Enabled: true, MaxContinue: 1}
	exec := NewCodexAutoExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-continue-no-encrypted", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads := drainStreamPayloads(t, result)
	waitPayload(t, capturedPayloads)
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream request count = %d, want 1", got)
	}
	joined := string(bytes.Join(payloads, []byte("\n")))
	if !strings.Contains(joined, "fallback answer") {
		t.Fatalf("no-encrypted stop should flush current round answer downstream:\n%s", joined)
	}
	if !strings.Contains(joined, "resp-no-enc") {
		t.Fatalf("no-encrypted stop should forward current terminal downstream:\n%s", joined)
	}
}

func TestCodexCacheReadStatsUsesPromptTotalDenominator(t *testing.T) {
	promptTotal, cachePct := codexCacheReadStats(coreusage.Detail{
		InputTokens:     227,
		CacheReadTokens: 441408,
	})
	if promptTotal != 441635 {
		t.Fatalf("prompt total = %d, want 441635", promptTotal)
	}
	if cachePct < 99.9 || cachePct >= 100 {
		t.Fatalf("cache pct = %.3f, want about 99.9", cachePct)
	}
}

func TestCodexCacheReadStatsZeroWithoutPromptTokens(t *testing.T) {
	promptTotal, cachePct := codexCacheReadStats(coreusage.Detail{OutputTokens: 12})
	if promptTotal != 0 {
		t.Fatalf("prompt total = %d, want 0", promptTotal)
	}
	if cachePct != 0 {
		t.Fatalf("cache pct = %.3f, want 0", cachePct)
	}
}

func drainStreamPayloads(t *testing.T, result *cliproxyexecutor.StreamResult) [][]byte {
	t.Helper()
	if result == nil || result.Chunks == nil {
		t.Fatal("stream result/chunks is nil")
	}
	var payloads [][]byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Errorf("stream chunk error = %v", chunk.Err)
				return
			}
			if len(chunk.Payload) > 0 {
				payloads = append(payloads, bytes.Clone(chunk.Payload))
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining stream")
	}
	return payloads
}

func waitPayload(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case payload := <-ch:
		return payload
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
		return nil
	}
}

func TestCodexWebsocketRecoverableRecycleKeepsDisconnectChannelOpen(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	for _, reason := range []string{"send_error", "auth_or_url_changed", "connection_max_age", "ping_write_failed", "upstream_disconnected"} {
		t.Run(reason, func(t *testing.T) {
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				t.Fatalf("dial websocket: %v", err)
			}

			exec := NewCodexWebsocketsExecutor(&config.Config{})
			sessionID := "recoverable-" + reason
			disconnectCh := exec.UpstreamDisconnectChan(sessionID)
			sess := exec.getOrCreateSession(sessionID)
			sess.connMu.Lock()
			sess.conn = conn
			sess.authID = "auth-1"
			sess.wsURL = wsURL
			sess.readerConn = conn
			sess.connMu.Unlock()

			recycleUpstreamConn(sess, conn, reason, errors.New("recoverable"))
			assertCodexDisconnectChannelOpen(t, disconnectCh)
		})
	}
}

func TestCodexWebsocketSendErrorRedialKeepsDisconnectChannelOpen(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connectionNumber := connections.Add(1)
		if connectionNumber == 1 {
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
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	staleConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial stale websocket: %v", err)
	}
	if errClose := staleConn.Close(); errClose != nil {
		t.Fatalf("close stale websocket: %v", errClose)
	}

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-send-retry", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL, "websockets": "true"}}
	sessionID := "send-error-redial"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = staleConn
	sess.connCreatedAt = time.Now()
	sess.readerConn = staleConn
	sess.wsURL = wsURL
	sess.authID = auth.ID
	sess.connGeneration = 1
	sess.connMu.Unlock()

	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"retry"}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drainStreamPayloads(t, result)

	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want stale plus one retry", got)
	}
	assertCodexDisconnectChannelOpen(t, disconnectCh)
}

func TestCodexWebsocketOldReaderCannotCloseNewGeneration(t *testing.T) {
	sess := &codexWebsocketSession{}
	oldConn := &websocket.Conn{}
	newConn := &websocket.Conn{}
	oldReader := sess.setActive(oldConn, 1)
	newReader := sess.setActive(newConn, 2)

	if sess.closeActive(oldReader) {
		t.Fatal("old reader closed a newer active generation")
	}
	select {
	case _, open := <-newReader.ch:
		if !open {
			t.Fatal("new generation channel was closed by old reader teardown")
		}
	default:
	}
	if !sess.closeActive(newReader) {
		t.Fatal("current reader did not close its own generation")
	}
	if _, open := <-newReader.ch; open {
		t.Fatal("current generation channel remained open after close")
	}
}

func TestCodexWebsocketTerminateUpstreamSessionNotifiesOnce(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "terminal-auth-removal"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	terminalErr := errors.New("auth removed")

	terminateUpstreamSession(sess, "auth_removed", terminalErr)
	terminateUpstreamSession(sess, "auth_removed", terminalErr)

	errRead, ok := <-disconnectCh
	if !ok {
		t.Fatal("disconnect channel closed before delivering terminal notification")
	}
	if !errors.Is(errRead, terminalErr) {
		t.Fatalf("disconnect error = %v, want %v", errRead, terminalErr)
	}
	if _, open := <-disconnectCh; open {
		t.Fatal("disconnect channel remained open after terminal notification")
	}
}

func TestCloseCodexWebsocketSessionsForAuthIDNotifiesOnce(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "terminal-auth-removal-global"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.authID = "auth-remove-me"
	sess.connMu.Unlock()

	CloseCodexWebsocketSessionsForAuthID("auth-remove-me", "auth_removed")
	CloseCodexWebsocketSessionsForAuthID("auth-remove-me", "auth_removed")

	errRead, ok := <-disconnectCh
	if !ok || errRead == nil {
		t.Fatalf("disconnect notification = (%v, %v), want one terminal error", errRead, ok)
	}
	if _, open := <-disconnectCh; open {
		t.Fatal("disconnect channel remained open after auth removal")
	}
}

func TestCodexWebsocketCloseExecutionSessionKeepsDisconnectChannelOpen(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "downstream-close"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)

	exec.CloseExecutionSession(sessionID)

	assertCodexDisconnectChannelOpen(t, disconnectCh)
}

func assertCodexDisconnectChannelOpen(t *testing.T, disconnectCh <-chan error) {
	t.Helper()
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}
	select {
	case _, ok := <-disconnectCh:
		if !ok {
			t.Fatal("recoverable recycle closed disconnect channel")
		}
		t.Fatal("recoverable recycle sent disconnect notification")
	default:
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
	if !strings.HasPrefix(codexUserAgent, codexOriginator+"/") {
		t.Fatalf("default Codex User-Agent = %s, want prefix %s/", codexUserAgent, codexOriginator)
	}
	if !strings.HasPrefix(codexUserAgent, "codex-tui/") {
		t.Fatalf("default Codex User-Agent = %s, want codex-tui prefix", codexUserAgent)
	}
	if !strings.Contains(codexUserAgent, "(codex-tui;") {
		t.Fatalf("default Codex User-Agent = %s, want codex-tui suffix", codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"User-Agent":            "codex_cli_rs/0.1.0",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
		"session-id":            "legacy-session",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := headers.Get("User-Agent"); got != "codex_cli_rs/0.1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "codex_cli_rs/0.1.0")
	}
	if got := headers.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
	if got := headers["session_id"]; len(got) != 1 || got[0] != "legacy-session" {
		t.Fatalf("session_id = %#v, want [legacy-session]", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersCanonicalizesLegacyUnderscoreSessionHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator": "Codex Desktop",
		"User-Agent": "codex_cli_rs/0.1.0",
		"Session_id": "legacy-underscore-session",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers["session_id"]; len(got) != 1 || got[0] != "legacy-underscore-session" {
		t.Fatalf("session_id = %#v, want [legacy-underscore-session]", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigDefaultsForOAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "my-codex-client/1.0",
			BetaFeatures: "feature-a,feature-b",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "my-codex-client/1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "my-codex-client/1.0")
	}
	if got := headers.Get("x-codex-beta-features"); got != "feature-a,feature-b" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "feature-a,feature-b")
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersPrefersExistingHeadersOverClientAndConfig(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")
	headers.Set("X-Codex-Beta-Features", "existing-beta")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "existing-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "existing-ua")
	}
	if gotVal := got.Get("x-codex-beta-features"); gotVal != "existing-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", gotVal, "existing-beta")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesClientHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := headers.Get("x-codex-beta-features"); got != "client-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "client-beta")
	}
}

func TestApplyCodexWebsocketHeadersIgnoresConfigForAPIKeyAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "sk-test", cfg)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("Originator"); got != "" {
		t.Fatalf("Originator = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPreservesExplicitAPIKeyUserAgent(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "api-key-client/1.0", "Originator": "explicit-origin"})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "sk-test", nil)

	if got := headers.Get("User-Agent"); got != "api-key-client/1.0" {
		t.Fatalf("User-Agent = %s, want api-key-client/1.0", got)
	}
	if got := headers.Get("Originator"); got != "explicit-origin" {
		t.Fatalf("Originator = %s, want explicit-origin", got)
	}
}

func TestApplyCodexWebsocketHeadersUsesCanonicalAccountHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1"}}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", nil)

	if got := headerValueCaseInsensitive(headers, "ChatGPT-Account-ID"); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %s, want acct-1", got)
	}
	values, ok := headers["ChatGPT-Account-ID"]
	if !ok {
		t.Fatalf("expected exact ChatGPT-Account-ID key, got %#v", headers)
	}
	if len(values) != 1 || values[0] != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID values = %#v, want [acct-1]", values)
	}
}

func TestApplyCodexPromptCacheHeadersSetsSessionIDAndLegacyConversation(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}

	_, headers := applyCodexPromptCacheHeaders("openai-response", req, []byte(`{"model":"gpt-5-codex"}`))

	if got := headers["session_id"]; len(got) != 1 || got[0] != "cache-1" {
		t.Fatalf("session_id = %#v, want [cache-1]", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %s, want empty", got)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %s, want cache-1", got)
	}
}

func TestApplyCodexPromptCacheHeadersClaudeUsesClaudeCodeSessionID(t *testing.T) {
	firstReq := cliproxyexecutor.Request{
		Model: "gpt-5-codex-claude-ws-cache-session",
		Payload: []byte(`{
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"account_uuid\":\"\",\"session_id\":\"ws-cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]
		}`),
	}
	secondReq := cliproxyexecutor.Request{
		Model: "gpt-5-codex-claude-ws-cache-session",
		Payload: []byte(`{
			"metadata":{"user_id":"{\"device_id\":\"device-b\",\"account_uuid\":\"\",\"session_id\":\"ws-cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]
		}`),
	}

	firstBody, firstHeaders := applyCodexPromptCacheHeaders("claude", firstReq, []byte(`{"model":"gpt-5-codex"}`))
	secondBody, secondHeaders := applyCodexPromptCacheHeaders("claude", secondReq, []byte(`{"model":"gpt-5-codex"}`))

	firstKey := gjson.GetBytes(firstBody, "prompt_cache_key").String()
	secondKey := gjson.GetBytes(secondBody, "prompt_cache_key").String()
	if firstKey == "" {
		t.Fatalf("first prompt_cache_key is empty; body=%s", string(firstBody))
	}
	if secondKey != firstKey {
		t.Fatalf("same Claude Code session_id produced different websocket prompt_cache_key: first=%q second=%q", firstKey, secondKey)
	}
	if got := firstHeaders["session_id"]; len(got) != 1 || got[0] != firstKey {
		t.Fatalf("first session_id = %#v, want [%q]", got, firstKey)
	}
	if got := secondHeaders["session_id"]; len(got) != 1 || got[0] != firstKey {
		t.Fatalf("second session_id = %#v, want [%q]", got, firstKey)
	}
}

func TestApplyCodexPromptCacheHeadersClaudeRejectsBareUserID(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex-claude-ws-cache-bare-user",
		Payload: []byte(`{"metadata":{"user_id":"same-user-across-chats"},"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
	}

	body, headers := applyCodexPromptCacheHeaders("claude", req, []byte(`{"model":"gpt-5-codex"}`))

	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "" {
		t.Fatalf("bare metadata.user_id must not create websocket prompt_cache_key, got %q; body=%s", got, string(body))
	}
	if got := headers["session_id"]; len(got) != 0 {
		t.Fatalf("bare metadata.user_id must not create websocket session_id, got %#v", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("bare metadata.user_id must not create websocket Session-Id, got %q", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("bare metadata.user_id must not create websocket Conversation_id, got %q", got)
	}
}

func TestApplyCodexWebsocketHeadersIdentityConfuseRemapsPromptCacheKey(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{SessionAffinity: true},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-ws-1", Provider: "codex"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"prompt_cache_key":"cache-ws-1","client_metadata":{"x-codex-installation-id":"install-ws-1"}}`),
	}

	body, headers := applyCodexPromptCacheHeaders("openai-response", req, []byte(`{"model":"gpt-5-codex"}`))
	body, identityState := applyCodexIdentityConfuseBody(cfg, auth, req.Payload, body)
	ctx := contextWithGinHeaders(map[string]string{
		"X-Codex-Turn-Metadata": `{"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1","window_id":"cache-ws-1:0"}`,
		"X-Client-Request-Id":   "client-request-1",
	})
	headers = applyCodexWebsocketHeaders(ctx, headers, auth, "oauth-token", cfg)
	applyCodexIdentityConfuseHeaders(headers, &identityState)

	expectedPromptCacheKey := codexIdentityConfuseUUID("auth-ws-1", "prompt-cache", "cache-ws-1")
	expectedTurnID := codexIdentityConfuseUUID("auth-ws-1", "turn", "turn-ws-1")
	if gotKey := gjson.GetBytes(body, "prompt_cache_key").String(); gotKey != expectedPromptCacheKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedPromptCacheKey)
	}
	if gotSession := headers["session_id"]; len(gotSession) != 1 || gotSession[0] != expectedPromptCacheKey {
		t.Fatalf("session_id = %#v, want [%q]", gotSession, expectedPromptCacheKey)
	}
	if gotCanonicalSession := headers.Get("Session-Id"); gotCanonicalSession != "" {
		t.Fatalf("Session-Id = %q, want empty", gotCanonicalSession)
	}
	if gotRequestID := headers.Get("X-Client-Request-Id"); gotRequestID != expectedPromptCacheKey {
		t.Fatalf("X-Client-Request-Id = %q, want %q", gotRequestID, expectedPromptCacheKey)
	}
	if gotThreadID := headers.Get("Thread-Id"); gotThreadID != expectedPromptCacheKey {
		t.Fatalf("Thread-Id = %q, want %q", gotThreadID, expectedPromptCacheKey)
	}
	if gotConversation := headers.Get("Conversation_id"); gotConversation != expectedPromptCacheKey {
		t.Fatalf("Conversation_id = %q, want %q", gotConversation, expectedPromptCacheKey)
	}
	if gotWindowID := headers.Get("X-Codex-Window-Id"); gotWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", gotWindowID, expectedPromptCacheKey+":0")
	}
	gotMetadata := headers.Get("X-Codex-Turn-Metadata")
	if gotMetadataPromptCacheKey := gjson.Get(gotMetadata, "prompt_cache_key").String(); gotMetadataPromptCacheKey != expectedPromptCacheKey {
		t.Fatalf("X-Codex-Turn-Metadata.prompt_cache_key = %q, want %q", gotMetadataPromptCacheKey, expectedPromptCacheKey)
	}
	if gotMetadataTurnID := gjson.Get(gotMetadata, "turn_id").String(); gotMetadataTurnID != expectedTurnID {
		t.Fatalf("X-Codex-Turn-Metadata.turn_id = %q, want %q", gotMetadataTurnID, expectedTurnID)
	}
	if gotMetadataWindowID := gjson.Get(gotMetadata, "window_id").String(); gotMetadataWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("X-Codex-Turn-Metadata.window_id = %q, want %q", gotMetadataWindowID, expectedPromptCacheKey+":0")
	}
	expectedInstallationID := codexIdentityConfuseUUID("auth-ws-1", "installation", "install-ws-1")
	if gotInstallationID := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); gotInstallationID != expectedInstallationID {
		t.Fatalf("installation id = %q, want %q", gotInstallationID, expectedInstallationID)
	}
}

func TestCodexIdentityConfuseResponsePayloadHidesUpstreamAndRestoresClient(t *testing.T) {
	state := codexIdentityConfuseState{
		enabled:                true,
		authID:                 "auth-ws-1",
		originalPromptCacheKey: "cache-ws-1",
		promptCacheKey:         codexIdentityConfuseUUID("auth-ws-1", "prompt-cache", "cache-ws-1"),
	}
	expectedTurnID := state.confuseTurnID("turn-ws-1")
	rawPayload := []byte(`{"type":"response.completed","response":{"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1"},"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1"}`)

	upstreamPayload := applyCodexIdentityConfuseResponsePayload(rawPayload, state)
	if bytes.Contains(upstreamPayload, []byte(`cache-ws-1`)) {
		t.Fatalf("upstream payload still contains original prompt_cache_key: %s", string(upstreamPayload))
	}
	if bytes.Contains(upstreamPayload, []byte(`turn-ws-1`)) {
		t.Fatalf("upstream payload still contains original turn_id: %s", string(upstreamPayload))
	}
	if !bytes.Contains(upstreamPayload, []byte(state.promptCacheKey)) {
		t.Fatalf("upstream payload missing confused prompt_cache_key: %s", string(upstreamPayload))
	}
	if !bytes.Contains(upstreamPayload, []byte(expectedTurnID)) {
		t.Fatalf("upstream payload missing confused turn_id: %s", string(upstreamPayload))
	}

	clientPayload := applyCodexIdentityExposeResponsePayload(upstreamPayload, state)
	if bytes.Contains(clientPayload, []byte(state.promptCacheKey)) {
		t.Fatalf("client payload still contains confused prompt_cache_key: %s", string(clientPayload))
	}
	if bytes.Contains(clientPayload, []byte(expectedTurnID)) {
		t.Fatalf("client payload still contains confused turn_id: %s", string(clientPayload))
	}
	if !bytes.Contains(clientPayload, []byte(`cache-ws-1`)) {
		t.Fatalf("client payload missing original prompt_cache_key: %s", string(clientPayload))
	}
	if !bytes.Contains(clientPayload, []byte(`turn-ws-1`)) {
		t.Fatalf("client payload missing original turn_id: %s", string(clientPayload))
	}

	rawSSE := []byte(`data: {"type":"response.completed","response":{"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1"}}`)
	upstreamSSE := applyCodexIdentityConfuseResponsePayload(rawSSE, state)
	if bytes.Contains(upstreamSSE, []byte(`cache-ws-1`)) {
		t.Fatalf("upstream SSE still contains original prompt_cache_key: %s", string(upstreamSSE))
	}
	if bytes.Contains(upstreamSSE, []byte(`turn-ws-1`)) {
		t.Fatalf("upstream SSE still contains original turn_id: %s", string(upstreamSSE))
	}
	clientSSE := applyCodexIdentityExposeResponsePayload(upstreamSSE, state)
	if !bytes.Contains(clientSSE, []byte(`cache-ws-1`)) || bytes.Contains(clientSSE, []byte(state.promptCacheKey)) {
		t.Fatalf("client SSE prompt_cache_key was not restored: %s", string(clientSSE))
	}
	if !bytes.Contains(clientSSE, []byte(`turn-ws-1`)) || bytes.Contains(clientSSE, []byte(expectedTurnID)) {
		t.Fatalf("client SSE turn_id was not restored: %s", string(clientSSE))
	}
}

func TestBuildCodexResponsesWebsocketURLRequiresHTTPURL(t *testing.T) {
	if got, err := buildCodexResponsesWebsocketURL("https://example.com/backend/responses"); err != nil || got != "wss://example.com/backend/responses" {
		t.Fatalf("https URL = %q, %v; want wss URL", got, err)
	}
	if _, err := buildCodexResponsesWebsocketURL("ftp://example.com/responses"); err == nil {
		t.Fatalf("expected unsupported scheme error")
	}
	if _, err := buildCodexResponsesWebsocketURL("https:///responses"); err == nil {
		t.Fatalf("expected empty host error")
	}
}

func TestParseCodexWebsocketErrorMarksConnectionLimitRetryable(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"},"headers":{"retry-after":"1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", err)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable websocket connection limit error")
	}
	if got := *retryable.RetryAfter(); got != 0 {
		t.Fatalf("retryAfter = %v, want connection-limit fallback 0", got)
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("retry-after") != "1" {
		t.Fatalf("headers = %#v, want retry-after", err)
	}
}

func TestParseCodexWebsocketErrorUsesUsageLimitRetryMetadata(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":7}}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable usage limit websocket error")
	}
	if got := *retryable.RetryAfter(); got != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", got)
	}
}

func TestParseCodexWebsocketErrorPreservesWrappedBodyAndHeaders(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"code":"websocket_connection_limit_reached","type":"server_error","message":"too many websocket connections"}},"headers":{"x-request-id":"req-1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	parsed := gjson.Parse(err.Error())
	if got := parsed.Get("status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("wrapped status = %d, want 429; payload=%s", got, err.Error())
	}
	if got := parsed.Get("body.error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("wrapped body error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	if got := parsed.Get("error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("surface error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected body.error.code websocket connection limit to be retryable")
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("x-request-id") != "req-1" {
		t.Fatalf("headers = %#v, want x-request-id", err)
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForOAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
}

func TestApplyCodexHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := req.Header.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexHeadersDoesNotInjectClientOnlyHeadersByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
	}
}

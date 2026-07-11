package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type websocketReplayCaptureExecutor struct {
	mu             sync.Mutex
	payloads       [][]byte
	authIDs        []string
	failureEvent   string
	replayFailures int
	done           chan struct{}
	doneOnce       sync.Once
}

func (e *websocketReplayCaptureExecutor) Identifier() string { return "codex" }

func (e *websocketReplayCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketReplayCaptureExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	e.authIDs = append(e.authIDs, auth.ID)
	call := len(e.payloads)
	e.mu.Unlock()
	if call >= 4 && e.done != nil {
		e.doneOnce.Do(func() { close(e.done) })
	}

	chunks := make(chan coreexecutor.StreamChunk, 2)
	switch call {
	case 1:
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}}`)}
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`)}
	case 2:
		if e.failureEvent == "response.error" {
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.error","response":{"error":{"code":"previous_response_not_found","message":"missing response"}}}`)}
		} else {
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"missing response"}}}`)}
		}
	case 3:
		if e.replayFailures > 0 {
			e.replayFailures--
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"replay failed"}}}`)}
		} else {
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-3","output":[{"type":"message","id":"assistant-2","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}]}}`)}
		}
	default:
		chunks <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","output":[]}}`, call))}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestResponsesWebsocketPassthroughKeepsTranscriptReplayArmedUntilSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &websocketReplayCaptureExecutor{
		done:           make(chan struct{}),
		failureEvent:   "response.failed",
		replayFailures: 1,
	}
	manager := coreauth.NewManager(nil, &orderedWebsocketSelector{order: []string{"auth-replay-a", "auth-replay-a", "auth-replay-b", "auth-replay-b"}}, nil)
	manager.RegisterExecutor(executor)
	auths := []*coreauth.Auth{
		{ID: "auth-replay-a", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"websockets": "true"}},
		{ID: "auth-replay-b", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"websockets": "true"}},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register auth %s: %v", auth.ID, err)
		}
	}
	modelName := "test-replay-failure-model"
	for _, auth := range auths {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelName}})
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		}
	})

	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses/ws", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	requests := [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`, modelName)),
		[]byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2","role":"user","content":"second"}]}`),
		[]byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2","role":"user","content":"second"}]}`),
		[]byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2","role":"user","content":"second"}]}`),
	}
	for i, request := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
			t.Fatalf("write request %d: %v", i+1, errWrite)
		}
		for {
			_, response, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatalf("read response %d: %v", i+1, errRead)
			}
			eventType := gjson.GetBytes(response, "type").String()
			if eventType == "response.completed" || eventType == "response.failed" || eventType == "response.error" {
				break
			}
		}
	}

	payloads := executor.Payloads()
	if len(payloads) != 4 {
		t.Fatalf("upstream payload count = %d, want 4", len(payloads))
	}
	if gjson.GetBytes(payloads[2], "previous_response_id").Exists() {
		t.Fatalf("forced replay retained stale previous_response_id: %s", payloads[2])
	}
	if got := len(gjson.GetBytes(payloads[2], "input").Array()); got != 3 {
		t.Fatalf("forced replay input length = %d, want full transcript: %s", got, payloads[2])
	}
	if got := gjson.GetBytes(payloads[3], "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("request after failed replay previous_response_id = %q, want passthrough resp-1: %s", got, payloads[3])
	}
	if got := len(gjson.GetBytes(payloads[3], "input").Array()); got != 1 {
		t.Fatalf("request after failed replay input length = %d, want incremental input: %s", got, payloads[3])
	}
}

func (e *websocketReplayCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketReplayCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketReplayCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketReplayCaptureExecutor) Payloads() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	payloads := make([][]byte, len(e.payloads))
	for i := range e.payloads {
		payloads[i] = bytes.Clone(e.payloads[i])
	}
	return payloads
}

func (e *websocketReplayCaptureExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

func TestCanonicalizeResponsesWebsocketShadowInput(t *testing.T) {
	for _, requestType := range []string{wsRequestTypeCreate, wsRequestTypeAppend} {
		t.Run(strings.TrimPrefix(requestType, "response."), func(t *testing.T) {
			upstream := []byte(fmt.Sprintf(`{"type":%q,"model":"gpt-5.5","stream":true,"input":"hello \"quoted\" world"}`, requestType))

			shadow := canonicalizeResponsesWebsocketShadowInput(upstream)

			if got := gjson.GetBytes(upstream, "input").String(); got != `hello "quoted" world` {
				t.Fatalf("upstream input changed = %q", got)
			}
			input := gjson.GetBytes(shadow, "input")
			if !input.IsArray() || len(input.Array()) != 1 {
				t.Fatalf("shadow input = %s, want one canonical message", input.Raw)
			}
			if got := input.Get("0.content.0.text").String(); got != `hello "quoted" world` {
				t.Fatalf("shadow text = %q", got)
			}
		})
	}
}

func TestResponsesWebsocketPassthroughReplaysTranscriptAfterPreviousResponseNotFound(t *testing.T) {
	for _, eventType := range []string{"response.failed", "response.error"} {
		t.Run(eventType, func(t *testing.T) {
			testResponsesWebsocketPassthroughReplay(t, eventType)
		})
	}
}

func testResponsesWebsocketPassthroughReplay(t *testing.T, failureEvent string) {
	gin.SetMode(gin.TestMode)
	executor := &websocketReplayCaptureExecutor{done: make(chan struct{}), failureEvent: failureEvent}
	manager := coreauth.NewManager(nil, &orderedWebsocketSelector{order: []string{"auth-replay-a", "auth-replay-a", "auth-replay-b", "auth-replay-b"}}, nil)
	manager.RegisterExecutor(executor)
	auths := []*coreauth.Auth{
		{
			ID:         "auth-replay-a",
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"websockets": "true"},
		},
		{
			ID:         "auth-replay-b",
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"websockets": "true"},
		},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register auth %s: %v", auth.ID, err)
		}
	}
	modelName := "test-replay-model"
	for _, auth := range auths {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelName}})
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		}
	})

	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses/ws", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	requests := [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`, modelName)),
		[]byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"result"},{"type":"message","id":"msg-2","role":"user","content":"second"}]}`),
		[]byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"result"},{"type":"message","id":"msg-2","role":"user","content":"second"}]}`),
		[]byte(`{"type":"response.create","previous_response_id":"resp-3","input":[{"type":"message","id":"msg-3","role":"user","content":"third"}]}`),
	}
	for i, request := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
			t.Fatalf("write request %d: %v", i+1, errWrite)
		}
		for {
			_, response, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatalf("read response %d: %v", i+1, errRead)
			}
			eventType := gjson.GetBytes(response, "type").String()
			if eventType == "response.completed" || eventType == "response.failed" || eventType == "response.error" {
				break
			}
		}
	}
	select {
	case <-executor.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for four upstream requests")
	}

	payloads := executor.Payloads()
	if len(payloads) != 4 {
		t.Fatalf("upstream payload count = %d, want 4", len(payloads))
	}
	if got := gjson.GetBytes(payloads[0], "type").String(); got != wsRequestTypeCreate {
		t.Fatalf("first request type = %q, want passthrough response.create", got)
	}
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("failed turn previous_response_id = %q, want resp-1", got)
	}
	if got := executor.AuthIDs(); len(got) != 4 || got[0] != "auth-replay-a" || got[1] != "auth-replay-a" || got[2] != "auth-replay-b" || got[3] != "auth-replay-b" {
		t.Fatalf("selected auth IDs = %v, want [auth-replay-a auth-replay-a auth-replay-b auth-replay-b]", got)
	}

	replay := payloads[2]
	if gjson.GetBytes(replay, "previous_response_id").Exists() {
		t.Fatalf("replay retained stale previous_response_id: %s", replay)
	}
	items := gjson.GetBytes(replay, "input").Array()
	if len(items) != 4 {
		t.Fatalf("replay input length = %d, want 4: %s", len(items), replay)
	}
	wantIDs := []string{"msg-1", "fc-1", "fco-1", "msg-2"}
	for i, wantID := range wantIDs {
		if got := items[i].Get("id").String(); got != wantID {
			t.Fatalf("replay input %d id = %q, want %q: %s", i, got, wantID, replay)
		}
	}
	if got := bytes.Count(replay, []byte(`"call_id":"call-1"`)); got != 2 {
		t.Fatalf("replay call_id count = %d, want paired call/output exactly once: %s", got, replay)
	}

	resumed := payloads[3]
	if got := gjson.GetBytes(resumed, "type").String(); got != wsRequestTypeCreate {
		t.Fatalf("resumed request type = %q, want passthrough response.create: %s", got, resumed)
	}
	if got := gjson.GetBytes(resumed, "previous_response_id").String(); got != "resp-3" {
		t.Fatalf("resumed previous_response_id = %q, want resp-3: %s", got, resumed)
	}
	if got := len(gjson.GetBytes(resumed, "input").Array()); got != 1 {
		t.Fatalf("resumed passthrough input length = %d, want 1: %s", got, resumed)
	}
}

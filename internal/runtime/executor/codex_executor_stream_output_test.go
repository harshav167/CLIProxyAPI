package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecute_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String()
	if gotContent != "ok" {
		t.Fatalf("choices.0.message.content = %q, want %q; payload=%s", gotContent, "ok", string(resp.Payload))
	}
}

func TestCodexExecutorExecuteAcceptsResponseDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.done","response":{"id":"resp_done","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer server.Close()

	resp, err := executeCodexNonStreamFixture(t, server.URL)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "resp_done" {
		t.Fatalf("id = %q, want resp_done; payload=%s", got, resp.Payload)
	}
	if gjson.GetBytes(resp.Payload, "response").Exists() || gjson.GetBytes(resp.Payload, "type").Exists() {
		t.Fatalf("raw event envelope leaked: %s", resp.Payload)
	}
}

func TestCodexExecutorExecuteReturnsResponseIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}` + "\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}` + "\n\n"))
	}))
	defer server.Close()

	resp, err := executeCodexNonStreamFixture(t, server.URL)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "resp_incomplete" {
		t.Fatalf("id = %q, want resp_incomplete; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "partial" {
		t.Fatalf("output text = %q, want partial; payload=%s", got, resp.Payload)
	}
	if gjson.GetBytes(resp.Payload, "response").Exists() || gjson.GetBytes(resp.Payload, "type").Exists() {
		t.Fatalf("raw event envelope leaked: %s", resp.Payload)
	}
}

func TestCodexExecutorExecuteTranslatesResponseIncompleteToChatCompletion(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{reason: "max_output_tokens", want: "length"},
		{reason: "content_filter", want: "content_filter"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"` + tt.reason + `"},"output":[]}}` + "\n\n"))
			}))
			defer server.Close()

			resp, err := executeCodexNonStreamFixtureFormat(t, server.URL, sdktranslator.FromString("openai"))
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if got := gjson.GetBytes(resp.Payload, "choices.0.finish_reason").String(); got != tt.want {
				t.Fatalf("finish_reason = %q, want %q; payload=%s", got, tt.want, resp.Payload)
			}
			if got := gjson.GetBytes(resp.Payload, "choices.0.native_finish_reason").String(); got != tt.want {
				t.Fatalf("native_finish_reason = %q, want %q; payload=%s", got, tt.want, resp.Payload)
			}
			if gjson.GetBytes(resp.Payload, "response").Exists() || gjson.GetBytes(resp.Payload, "type").Exists() {
				t.Fatalf("raw event envelope leaked: %s", resp.Payload)
			}
		})
	}
}

func TestCodexExecutorExecuteRejectsUnknownResponseIncompleteReasonForChatCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"upstream_unknown"},"output":[]}}` + "\n\n"))
	}))
	defer server.Close()

	_, err := executeCodexNonStreamFixtureFormat(t, server.URL, sdktranslator.FromString("openai"))
	if err == nil {
		t.Fatal("expected translation error")
	}
	if statusCodeFromTestError(t, err) != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; err=%v", statusCodeFromTestError(t, err), http.StatusBadGateway, err)
	}
	want := "codex terminal event response.incomplete could not be translated to openai"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestCodexExecutorExecuteReturnsClassifiedFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.error","response":{"error":{"code":"server_error","message":"upstream exploded"}}}` + "\n\n"))
	}))
	defer server.Close()

	_, err := executeCodexNonStreamFixture(t, server.URL)
	if err == nil {
		t.Fatal("expected classified terminal failure")
	}
	if statusCodeFromTestError(t, err) != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; err=%v", statusCodeFromTestError(t, err), http.StatusBadGateway, err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("failure message missing upstream error: %v", err)
	}
}

func executeCodexNonStreamFixture(t *testing.T, baseURL string) (cliproxyexecutor.Response, error) {
	t.Helper()
	return executeCodexNonStreamFixtureFormat(t, baseURL, sdktranslator.FromString("openai-response"))
}

func executeCodexNonStreamFixtureFormat(t *testing.T, baseURL string, responseFormat sdktranslator.Format) (cliproxyexecutor.Response, error) {
	t.Helper()
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": baseURL, "api_key": "test"}}
	return executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: responseFormat,
		Stream:         false,
	})
}

func TestCodexExecutorExecuteSurfacesTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.failed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected terminal stream error, got nil")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_length_exceeded")
	if !strings.Contains(err.Error(), "Your input exceeds the context window") {
		t.Fatalf("error message missing upstream context text: %v", err)
	}
}

func TestCodexExecutorExecuteStreamSurfacesTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("missing stream terminal error")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, streamErr)
	}
	assertCodexErrorCode(t, streamErr.Error(), "invalid_request_error", "context_length_exceeded")
}

func TestCodexTerminalStreamContextLengthErrFromResponseFailed(t *testing.T) {
	err, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`))
	if !ok {
		t.Fatal("expected context length terminal error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_length_exceeded")
}

func TestCodexTerminalStreamContextLengthErrFromTopLevelError(t *testing.T) {
	err, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","sequence_number":2}`))
	if !ok {
		t.Fatal("expected top-level context length terminal error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_length_exceeded")
	if !strings.Contains(err.Error(), "Your input exceeds the context window") {
		t.Fatalf("error message missing upstream context text: %v", err)
	}
}

func TestCodexTerminalStreamContextLengthErrIgnoresOtherTerminalErrors(t *testing.T) {
	_, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`))
	if ok {
		t.Fatal("rate limit terminal error should not be handled by context length fix")
	}
}

func TestCodexTerminalStreamErrIgnoresRateLimitTerminalErrors(t *testing.T) {
	_, _, ok := codexTerminalStreamErr([]byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`))
	if ok {
		t.Fatal("rate limit terminal error should not be handled by replay terminal error path")
	}
}

func TestCodexTerminalStreamErrHandlesUsageLimitErrorEvent(t *testing.T) {
	streamErr, _, ok := codexTerminalStreamErr([]byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_in_seconds":300}}`))
	if !ok {
		t.Fatal("expected usage_limit_reached terminal error to be handled")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	retryAfter := streamErr.RetryAfter()
	if retryAfter == nil {
		t.Fatal("expected retryAfter from usage_limit_reached terminal error")
	}
	if *retryAfter != 300*time.Second {
		t.Fatalf("retryAfter = %v, want %v", *retryAfter, 300*time.Second)
	}
}

func TestCodexTerminalStreamErrHandlesUsageLimitResponseFailed(t *testing.T) {
	streamErr, _, ok := codexTerminalStreamErr([]byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":60}}}`))
	if !ok {
		t.Fatal("expected usage_limit_reached response.failed terminal error to be handled")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if streamErr.RetryAfter() == nil {
		t.Fatal("expected retryAfter from usage_limit_reached response.failed terminal error")
	}
}

func statusCodeFromTestError(t *testing.T, err error) int {
	t.Helper()

	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode(): %v", err, err)
	}
	return statusErr.StatusCode()
}

func TestCodexExecutorExecuteStream_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var completed []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if !bytes.HasPrefix(payload, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(payload[5:])
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			completed = append([]byte(nil), data...)
		}
	}

	if len(completed) == 0 {
		t.Fatal("missing response.completed chunk")
	}

	gotContent := gjson.GetBytes(completed, "response.output.0.content.0.text").String()
	if gotContent != "ok" {
		t.Fatalf("response.output[0].content[0].text = %q, want %q; completed=%s", gotContent, "ok", string(completed))
	}
}

func TestCodexExecutorExecuteStreamContinueFoldOpensHTTPContinuation(t *testing.T) {
	capturedPayloads := make(chan []byte, 4)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("request path = %s, want /responses", r.URL.Path)
			return
		}
		payload, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
			return
		}
		capturedPayloads <- bytes.Clone(payload)

		w.Header().Set("Content-Type", "text/event-stream")
		switch requests.Add(1) {
		case 1:
			events := append([]string{
				foldCreated("resp-http-visible", 0),
				foldReasoningDone(0, "rs-http-1", "enc-http-1", 2),
			}, foldMessageEvents(1, "msg-http-discard", "http truncated", 3)...)
			events = append(events, foldCompleted("resp-http-1", 10, 0, 516, 516, 9))
			_, _ = w.Write([]byte(foldSSE(events...)))
		case 2:
			events := append([]string{foldCreated("resp-http-2", 0)}, foldMessageEvents(0, "msg-http-final", "http final", 1)...)
			events = append(events, foldCompleted("resp-http-2", 3, 0, 6, 2, 9))
			_, _ = w.Write([]byte(foldSSE(events...)))
		default:
			t.Errorf("unexpected request count")
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.CodexContinueThinking = &config.CodexContinueConfig{Enabled: true, MaxContinue: 1}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(foldBaseBody(foldUserInput("one"))),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
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
	if got := gjson.GetBytes([]byte(input[1].Raw), "encrypted_content").String(); got != "enc-http-1" {
		t.Fatalf("continuation did not replay encrypted reasoning, got %q; payload=%s", got, secondPayload)
	}
	if got := gjson.GetBytes([]byte(input[2].Raw), "phase").String(); got != "commentary" {
		t.Fatalf("continuation marker phase = %q, want commentary; payload=%s", got, secondPayload)
	}

	joined := string(bytes.Join(payloads, []byte("\n")))
	if strings.Contains(joined, "http truncated") {
		t.Fatalf("truncated tentative answer leaked downstream:\n%s", joined)
	}
	if !strings.Contains(joined, "http final") {
		t.Fatalf("final continuation answer missing downstream:\n%s", joined)
	}
	terminal := lastFoldPayloadOfType(t, payloads, "response.completed")
	if got := gjson.GetBytes(terminal, "response.id").String(); got != "resp-http-visible" {
		t.Fatalf("folded terminal response.id = %q, want visible first-round id; terminal=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.metadata.proxy_upstream_previous_response_id").String(); got != "resp-http-2" {
		t.Fatalf("folded terminal upstream id = %q, want resp-http-2; terminal=%s", got, terminal)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream request count = %d, want 2", got)
	}
}

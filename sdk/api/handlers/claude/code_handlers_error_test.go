package claude

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestClaudeErrorExtractsOpenAIStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Type != "error" {
		t.Fatalf("type = %q, want error", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Message != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorExtractsClaudeStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_123"}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Message != "This request would exceed your account's rate limit. Please try again later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseUsesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestPendingClaudeStreamErrorUsesBufferedError(t *testing.T) {
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	gotErr, ok := pendingClaudeStreamError(errs)
	if !ok {
		t.Fatal("expected pending stream error")
	}
	if gotErr != wantErr {
		t.Fatalf("pending error = %p, want %p", gotErr, wantErr)
	}
}

func TestAwaitClaudeStreamBootstrapWritesHeartbeatBeforeSlowFirstChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler := &ClaudeCodeAPIHandler{
		BaseAPIHandler: handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
			Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1},
		}, nil),
	}
	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	go func() {
		time.Sleep(1100 * time.Millisecond)
		data <- []byte("event: message_start\ndata: {}\n\n")
	}()

	chunk, errMsg, committed := handler.awaitClaudeStreamBootstrap(c, recorder, data, errs, nil, false)
	if errMsg != nil {
		t.Fatalf("bootstrap error = %v", errMsg.Error)
	}
	if !committed {
		t.Fatal("bootstrap did not commit SSE headers after heartbeat")
	}
	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("bootstrap body = %q, want keep-alive heartbeat", recorder.Body.String())
	}
	if string(chunk) != "event: message_start\ndata: {}\n\n" {
		t.Fatalf("first chunk = %q", chunk)
	}
}

func TestClaudeBootstrapKeepAliveDefaultsToFifteenSeconds(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{
		BaseAPIHandler: handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil),
	}
	if got := handler.claudeBootstrapKeepAliveInterval(); got != 15*time.Second {
		t.Fatalf("bootstrap keep-alive interval = %v, want 15s", got)
	}
}

func TestAwaitClaudeStreamExecutionWritesHeartbeatWhileExecutionBlocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler := &ClaudeCodeAPIHandler{
		BaseAPIHandler: handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
			Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1},
		}, nil),
	}
	heartbeatWritten := make(chan struct{}, 1)
	executionRelease := make(chan struct{})
	executionDone := make(chan struct{})
	var result claudeStreamExecutionResult
	var committed, canceled bool
	writer := &heartbeatSignalWriter{ResponseWriter: c.Writer, heartbeatWritten: heartbeatWritten}
	c.Writer = writer
	go func() {
		defer close(executionDone)
		result, committed, canceled = handler.awaitClaudeStreamExecution(c, writer, func() claudeStreamExecutionResult {
			<-executionRelease
			return claudeStreamExecutionResult{Data: make(chan []byte), Errs: make(chan *interfaces.ErrorMessage)}
		})
	}()

	select {
	case <-heartbeatWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bootstrap heartbeat")
	}
	close(executionRelease)
	<-executionDone

	if canceled {
		t.Fatal("blocked execution was canceled")
	}
	if !committed {
		t.Fatal("default heartbeat did not commit SSE response")
	}
	if result.Data == nil || result.Errs == nil {
		t.Fatal("execution result was not returned")
	}
	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("execution body = %q, want default heartbeat", recorder.Body.String())
	}
}

type heartbeatSignalWriter struct {
	gin.ResponseWriter
	heartbeatWritten chan<- struct{}
}

func (w *heartbeatSignalWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if bytes.Contains(p, []byte(": keep-alive\n\n")) {
		select {
		case w.heartbeatWritten <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestAwaitClaudeStreamBootstrapReturnsImmediateErrorWithoutCommitting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler := &ClaudeCodeAPIHandler{
		BaseAPIHandler: handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
			Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1},
		}, nil),
	}
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	wantErr := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("upstream unavailable")}
	errs <- wantErr

	chunk, errMsg, committed := handler.awaitClaudeStreamBootstrap(c, recorder, data, errs, nil, false)
	if len(chunk) != 0 {
		t.Fatalf("bootstrap chunk = %q, want empty", chunk)
	}
	if errMsg != wantErr {
		t.Fatalf("bootstrap error = %p, want %p", errMsg, wantErr)
	}
	if committed {
		t.Fatal("immediate upstream error committed SSE response")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("bootstrap body = %q, want empty", recorder.Body.String())
	}
}

func TestAwaitClaudeStreamBootstrapReturnsPostHeartbeatErrorAsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler := &ClaudeCodeAPIHandler{
		BaseAPIHandler: handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
			Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1},
		}, nil),
	}
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	wantErr := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("slow upstream failed")}
	go func() {
		time.Sleep(1100 * time.Millisecond)
		errs <- wantErr
	}()

	chunk, errMsg, committed := handler.awaitClaudeStreamBootstrap(c, recorder, data, errs, nil, false)
	if len(chunk) != 0 {
		t.Fatalf("bootstrap chunk = %q, want empty", chunk)
	}
	if errMsg != wantErr {
		t.Fatalf("bootstrap error = %p, want %p", errMsg, wantErr)
	}
	if !committed {
		t.Fatal("post-heartbeat error reported response as uncommitted")
	}
	handler.writeClaudeStreamTerminalError(c, errMsg)
	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("bootstrap body = %q, want keep-alive", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "event: error\n") || !strings.Contains(recorder.Body.String(), "slow upstream failed") {
		t.Fatalf("bootstrap body = %q, want Claude SSE error", recorder.Body.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", recorder.Code)
	}
}

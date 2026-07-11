package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

func TestCodexAutoExecuteStreamPreviousResponseRetryTransportFailureFallsBackToHTTP(t *testing.T) {
	fixture := newCodexAutoFallbackFixture(t, func(attempt int32) [][]byte {
		if attempt == 1 {
			return [][]byte{[]byte(`{"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"No response found for previous_response_id resp-stale"}}}`)}
		}
		return nil
	})
	sessionKey := fixture.opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string)
	bridge := getHTTPWSBridge()
	bridge.Reset(sessionKey)
	bridge.CaptureResponse(sessionKey, "resp-stale", fixture.req.Model, fixture.auth.ID, fixture.req.Payload, nil)
	fixture.req.Payload, _ = sjson.SetRawBytes(fixture.req.Payload, "input", []byte(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}
	]`))

	result, err := fixture.exec.ExecuteStream(context.Background(), fixture.auth, fixture.req, fixture.opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads := drainStreamPayloads(t, result)
	if !bytes.Contains(bytes.Join(payloads, nil), []byte("http fallback")) {
		t.Fatalf("fallback payloads = %q, want HTTP response", payloads)
	}
	if got := fixture.wsRequests.Load(); got != 2 {
		t.Fatalf("websocket requests = %d, want delta plus full-context retry", got)
	}
	if got := fixture.httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestCodexWebsocketTransportBootstrapErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "connection limit", ctx: context.Background(), err: errors.New(`{"error":{"code":"websocket_connection_limit_reached"}}`), want: true},
		{name: "EOF", ctx: context.Background(), err: io.EOF, want: true},
		{name: "websocket close", ctx: context.Background(), err: &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "lost"}, want: true},
		{name: "handshake", ctx: context.Background(), err: errors.New("websocket: bad handshake"), want: true},
		{name: "caller cancellation", ctx: canceledContext(), err: context.Canceled, want: false},
		{name: "caller deadline", ctx: deadlineExceededContext(), err: context.DeadlineExceeded, want: false},
		{name: "upstream timeout", ctx: context.Background(), err: timeoutError{}, want: true},
		{name: "wrapped upstream deadline", ctx: context.Background(), err: fmt.Errorf("upstream read: %w", context.DeadlineExceeded), want: true},
		{name: "quota", ctx: context.Background(), err: errors.New(`{"error":{"code":"insufficient_quota"}}`), want: false},
		{name: "context length", ctx: context.Background(), err: errors.New(`{"error":{"code":"context_length_exceeded"}}`), want: false},
		{name: "invalid request", ctx: context.Background(), err: errors.New(`{"error":{"code":"invalid_request_error"}}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCodexWebsocketTransportBootstrapError(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("classification = %v, want %v for %v", got, tt.want, tt.err)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "upstream timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func deadlineExceededContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	return ctx
}

func TestBootstrapCodexStreamReplaysFirstPayloadAndHeaders(t *testing.T) {
	input := make(chan cliproxyexecutor.StreamChunk, 2)
	input <- cliproxyexecutor.StreamChunk{}
	input <- cliproxyexecutor.StreamChunk{Payload: []byte("visible")}
	close(input)
	headers := http.Header{"X-Test": []string{"preserved"}}

	result, err := bootstrapCodexStream(context.Background(), &cliproxyexecutor.StreamResult{Headers: headers, Chunks: input})
	if err != nil {
		t.Fatalf("bootstrapCodexStream() error = %v", err)
	}
	if got := result.Headers.Get("X-Test"); got != "preserved" {
		t.Fatalf("header = %q, want preserved", got)
	}
	var payloads [][]byte
	for chunk := range result.Chunks {
		if len(chunk.Payload) > 0 {
			payloads = append(payloads, chunk.Payload)
		}
	}
	if len(payloads) != 1 || string(payloads[0]) != "visible" {
		t.Fatalf("payloads = %q, want [visible]", payloads)
	}
}

func TestBootstrapCodexStreamAcceptsNilContext(t *testing.T) {
	input := make(chan cliproxyexecutor.StreamChunk, 1)
	input <- cliproxyexecutor.StreamChunk{Payload: []byte("visible")}
	close(input)

	result, err := bootstrapCodexStream(nil, &cliproxyexecutor.StreamResult{Chunks: input})
	if err != nil {
		t.Fatalf("bootstrapCodexStream() error = %v", err)
	}
	chunk, ok := <-result.Chunks
	if !ok {
		t.Fatal("replayed stream closed before first payload")
	}
	if got := string(chunk.Payload); got != "visible" {
		t.Fatalf("payload = %q, want visible", got)
	}
}

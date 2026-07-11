package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexPreviousResponseNotFoundError struct {
	cause error
}

func (e codexPreviousResponseNotFoundError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return "previous_response_not_found"
}

func (e codexPreviousResponseNotFoundError) Unwrap() error {
	return e.cause
}

func bootstrapCodexStream(ctx context.Context, result *cliproxyexecutor.StreamResult) (*cliproxyexecutor.StreamResult, error) {
	if result == nil || result.Chunks == nil {
		return nil, fmt.Errorf("codex websocket bootstrap: stream result is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok := <-result.Chunks:
			if !ok {
				return nil, io.EOF
			}
			buffered = append(buffered, chunk)
			if payload := bytes.TrimSpace(chunk.Payload); len(payload) > 0 {
				if helps.IsCodexPreviousResponseNotFoundEvent(payload) {
					return nil, codexPreviousResponseNotFoundError{}
				}
				return replayCodexStream(ctx, result, buffered), nil
			}
			if chunk.Err != nil {
				if isCodexPreviousResponseNotFoundError(chunk.Err) {
					return nil, codexPreviousResponseNotFoundError{cause: chunk.Err}
				}
				return nil, chunk.Err
			}
		}
	}
}

func isCodexPreviousResponseNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var miss codexPreviousResponseNotFoundError
	if errors.As(err, &miss) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "previous_response_not_found") ||
		strings.Contains(message, "previous_response_id") && strings.Contains(message, "not found")
}

func replayCodexStream(ctx context.Context, result *cliproxyexecutor.StreamResult, buffered []cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		for _, chunk := range buffered {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

func isCodexWebsocketTransportBootstrapError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if isCodexWebsocketConnectionLimitBootstrapError(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var closeErr *net.OpError
	if errors.As(err, &closeErr) {
		return true
	}
	var websocketClose *websocket.CloseError
	if errors.As(err, &websocketClose) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"websocket: close", "websocket: bad handshake", "connection reset", "broken pipe", "unexpected eof"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func isCodexWebsocketConnectionLimitBootstrapError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "websocket_connection_limit_reached")
}

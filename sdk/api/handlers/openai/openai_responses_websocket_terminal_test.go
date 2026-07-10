package openai

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesWebsocketReconstructsOutputItemDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	result := make(chan []byte, 1)
	terminalPayload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r
		data := make(chan []byte, 2)
		errs := make(chan *interfaces.ErrorMessage)
		data <- []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"assistant-1","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`)
		data <- terminalPayload
		close(data)
		close(errs)
		output, _, _, _, _ := handler.forwardResponsesWebsocket(ctx, conn, func(...interface{}) {}, data, errs, newInMemoryWebsocketTimelineLog(), "collector-session")
		result <- output
	}))
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var downstream [][]byte
	for range 2 {
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read payload: %v", errRead)
		}
		downstream = append(downstream, payload)
	}
	if !bytes.Equal(downstream[1], terminalPayload) {
		t.Fatalf("terminal payload changed = %s, want %s", downstream[1], terminalPayload)
	}
	output := <-result
	if !bytes.Contains(output, []byte(`"id":"assistant-1"`)) {
		t.Fatalf("completed output = %s, want reconstructed output item", output)
	}
}

func TestForwardResponsesWebsocketFailureTerminalPreservedOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, eventType := range []string{"response.failed", "response.incomplete", "response.error"} {
		t.Run(eventType, func(t *testing.T) {
			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			serverErrCh := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					serverErrCh <- err
					return
				}
				defer func() { _ = conn.Close() }()
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = r
				data := make(chan []byte, 1)
				errs := make(chan *interfaces.ErrorMessage)
				data <- []byte(`{"type":"` + eventType + `","response":{"id":"resp-terminal"}}`)
				close(data)
				close(errs)

				_, _, _, errMsg, errForward := handler.forwardResponsesWebsocket(
					ctx,
					conn,
					func(...interface{}) {},
					data,
					errs,
					newInMemoryWebsocketTimelineLog(),
					"terminal-session",
				)
				if errForward != nil {
					serverErrCh <- errForward
					return
				}
				if errMsg != nil {
					serverErrCh <- fmt.Errorf("unexpected synthetic error: %v", errMsg.Error)
					return
				}
				serverErrCh <- nil
			}))

			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				server.Close()
				t.Fatalf("dial websocket: %v", err)
			}
			var payloads [][]byte
			for len(payloads) < 2 {
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				_, payload, errRead := conn.ReadMessage()
				if errRead != nil {
					break
				}
				payloads = append(payloads, payload)
			}
			_ = conn.Close()
			server.Close()

			if len(payloads) != 1 {
				t.Fatalf("downstream payload count = %d, want 1; payloads=%q", len(payloads), payloads)
			}
			if got := gjson.GetBytes(payloads[0], "type").String(); got != eventType {
				t.Fatalf("downstream event type = %q, want %q", got, eventType)
			}
			if errServer := <-serverErrCh; errServer != nil {
				t.Fatalf("server error: %v", errServer)
			}
		})
	}
}

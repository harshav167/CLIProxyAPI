package openai

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	codexconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/openai/chat-completions"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertResponsesObjectToChatCompletion(ctx context.Context, modelName string, originalChatJSON, responsesRequestJSON, responsesPayload []byte) []byte {
	if len(responsesPayload) == 0 {
		return nil
	}
	wrapped := wrapResponsesPayloadAsCompleted(responsesPayload)
	if len(wrapped) == 0 {
		return nil
	}
	var param any
	converted := codexconverter.ConvertCodexResponseToOpenAINonStream(ctx, modelName, originalChatJSON, responsesRequestJSON, wrapped, &param)
	if len(converted) == 0 {
		return nil
	}
	return converted
}

func wrapResponsesPayloadAsCompleted(payload []byte) []byte {
	if gjson.GetBytes(payload, "type").Exists() {
		return payload
	}
	if gjson.GetBytes(payload, "object").String() != "response" {
		return payload
	}
	wrapped := `{"type":"response.completed","response":{}}`
	wrapped, _ = sjson.SetRaw(wrapped, "response", string(payload))
	return []byte(wrapped)
}

func writeConvertedResponsesChunk(c *gin.Context, ctx context.Context, modelName string, originalChatJSON, responsesRequestJSON, chunk []byte, param *any) {
	outputs := codexconverter.ConvertCodexResponseToOpenAI(ctx, modelName, originalChatJSON, responsesRequestJSON, chunk, param)
	for _, out := range outputs {
		if len(out) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", out)
	}
}

func (h *OpenAIAPIHandler) forwardResponsesAsChatStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, ctx context.Context, modelName string, originalChatJSON, responsesRequestJSON []byte, param *any) {
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			outputs := codexconverter.ConvertCodexResponseToOpenAI(ctx, modelName, originalChatJSON, responsesRequestJSON, chunk, param)
			for _, out := range outputs {
				if len(out) == 0 {
					continue
				}
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", out)
			}
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && errMsg.Error.Error() != "" {
				errText = errMsg.Error.Error()
			}
			body := handlers.BuildErrorResponseBody(status, errText)
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(body))
		},
		WriteDone: func() {
			_, _ = fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
		},
	})
}

// Deliberately session-less: see b5cd8425 for the Cursor GPT-5.4 regression
// that gated wiring withCursorExecutionSessionID on the *ViaResponses paths.
func (h *OpenAIAPIHandler) handleNonStreamingResponseViaResponses(c *gin.Context, rawJSON []byte, originalChatJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withCursorCacheIdentity(cliCtx, originalChatJSON)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, OpenaiResponse, modelName, rawJSON, h.GetAlt(c))
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	converted := convertResponsesObjectToChatCompletion(cliCtx, modelName, originalChatJSON, rawJSON, resp)
	if converted == nil {
		h.WriteErrorResponse(c, &interfaces.ErrorMessage{
			StatusCode: http.StatusInternalServerError,
			Error:      fmt.Errorf("failed to convert response to chat completion format"),
		})
		cliCancel(fmt.Errorf("response conversion failed"))
		return
	}
	_, _ = c.Writer.Write(converted)
	cliCancel()
}

// Streaming counterpart of handleNonStreamingResponseViaResponses; same
// session-less constraint (see b5cd8425).
func (h *OpenAIAPIHandler) handleStreamingResponseViaResponses(c *gin.Context, rawJSON []byte, originalChatJSON []byte) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withCursorCacheIdentity(cliCtx, originalChatJSON)
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, OpenaiResponse, modelName, rawJSON, h.GetAlt(c))
	var param any

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
				flusher.Flush()
				cliCancel(nil)
				return
			}

			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			writeConvertedResponsesChunk(c, cliCtx, modelName, originalChatJSON, rawJSON, chunk, &param)
			flusher.Flush()

			h.forwardResponsesAsChatStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, cliCtx, modelName, originalChatJSON, rawJSON, &param)
			return
		}
	}
}

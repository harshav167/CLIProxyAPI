package helps

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const httpStatusAnthropicOverloaded = 529

type claudeStreamError struct {
	code    int
	errType string
	message string
}

func (e claudeStreamError) Error() string {
	if e.errType == "" {
		return e.message
	}
	if e.message == "" {
		return fmt.Sprintf("claude stream error: %s", e.errType)
	}
	return fmt.Sprintf("claude stream error: %s: %s", e.errType, e.message)
}

func (e claudeStreamError) StatusCode() int { return e.code }

// DetectClaudeStreamError returns a status-coded error when an Anthropic SSE
// line carries a typed error event.
func DetectClaudeStreamError(line []byte) error {
	data := bytes.TrimSpace(line)
	if !bytes.HasPrefix(data, []byte("data:")) {
		return nil
	}
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
		return nil
	}

	root := gjson.ParseBytes(data)
	if root.Get("type").String() != "error" {
		return nil
	}
	errObj := root.Get("error")
	if !errObj.Exists() {
		return nil
	}

	errType := strings.TrimSpace(errObj.Get("type").String())
	message := strings.TrimSpace(errObj.Get("message").String())
	if errType == "" && message == "" {
		return nil
	}

	return claudeStreamError{
		code:    ClaudeStreamErrorStatus(errType),
		errType: errType,
		message: message,
	}
}

// ClaudeStreamErrorStatus maps Anthropic stream error types to HTTP status codes.
func ClaudeStreamErrorStatus(errType string) int {
	switch errType {
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "invalid_request_error":
		return http.StatusBadRequest
	case "not_found_error":
		return http.StatusNotFound
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return httpStatusAnthropicOverloaded
	default:
		return http.StatusBadGateway
	}
}

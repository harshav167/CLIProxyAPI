package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestClientDetectionReadsUserAgentFromGinContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("User-Agent", "Cursor/1.0")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	if !IsCursorClient(ctx) {
		t.Fatal("Cursor User-Agent from gin context should be detected")
	}
	if IsDroidClient(ctx) {
		t.Fatal("Cursor User-Agent from gin context should not be treated as Droid")
	}
}

func TestShouldForwardEventToClientAppliesDroidAllowlistOnlyToDroid(t *testing.T) {
	unsupportedEvent := "response.content_part.done"
	cursorCtx := WithClientUserAgent(context.Background(), "Cursor/1.0")
	droidCtx := WithClientUserAgent(context.Background(), "factory-cli/0.108.0")

	if !shouldForwardEventToClient(cursorCtx, unsupportedEvent) {
		t.Fatal("non-Droid clients should receive unsupported-by-Droid events")
	}
	if shouldForwardEventToClient(droidCtx, unsupportedEvent) {
		t.Fatal("Droid clients should not receive events outside the allowlist")
	}
	if shouldForwardEventToClient(context.Background(), unsupportedEvent) {
		t.Fatal("missing User-Agent should keep Droid-protective filtering")
	}
	if !shouldForwardEventToClient(droidCtx, "response.output_text.delta") {
		t.Fatal("Droid clients should receive allowlisted events")
	}
}

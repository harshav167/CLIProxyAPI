package helps

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	ClaudeCodeSessionHeader      = "X-Claude-Code-Session-Id"
	ClaudeCodeCacheSessionHeader = "X-Claude-Code-Cache-Session-Id"
)

var claudeCodeSessionSuffixPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// ExtractClaudeCodeSessionID resolves a Claude Code session ID, preferring X-Claude-Code-Session-Id over payload metadata.
func ExtractClaudeCodeSessionID(ctx context.Context, payload []byte, headers http.Header) string {
	if headers != nil {
		if sessionID := strings.TrimSpace(headers.Get(ClaudeCodeSessionHeader)); sessionID != "" {
			return sessionID
		}
	}
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			if sessionID := strings.TrimSpace(ginCtx.Request.Header.Get(ClaudeCodeSessionHeader)); sessionID != "" {
				return sessionID
			}
		}
	}
	return extractClaudeCodeSessionIDFromPayload(payload)
}

// ExtractClaudeCodeCacheScopeID resolves the stable cache-routing scope for a
// Claude Code launch. A launcher-provided cache scope lets main, workflow, and
// subagent requests share prompt-cache routing while their execution session
// IDs remain isolated.
func ExtractClaudeCodeCacheScopeID(ctx context.Context, payload []byte, headers http.Header) string {
	if headers != nil {
		if cacheScopeID := strings.TrimSpace(headers.Get(ClaudeCodeCacheSessionHeader)); cacheScopeID != "" {
			return cacheScopeID
		}
	}
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			if cacheScopeID := strings.TrimSpace(ginCtx.Request.Header.Get(ClaudeCodeCacheSessionHeader)); cacheScopeID != "" {
				return cacheScopeID
			}
		}
	}
	return ExtractClaudeCodeSessionID(ctx, payload, headers)
}

func extractClaudeCodeSessionIDFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	userID := gjson.GetBytes(payload, "metadata.user_id").String()
	if userID == "" {
		return ""
	}
	if matches := claudeCodeSessionSuffixPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return matches[1]
	}
	if len(userID) > 0 && userID[0] == '{' {
		return strings.TrimSpace(gjson.Get(userID, "session_id").String())
	}
	return ""
}

// ClaudeCodePromptCacheID maps a Claude Code cache scope to a stable upstream
// prompt_cache_key. The ID is derived directly from the model and launch scope
// so concurrent requests for the same scope cannot race and choose different
// cache keys.
func ClaudeCodePromptCacheID(ctx context.Context, modelName string, payload []byte, headers http.Header) (string, bool) {
	cacheScopeID := ExtractClaudeCodeCacheScopeID(ctx, payload, headers)
	if cacheScopeID == "" {
		return "", false
	}
	key := CodexPromptCacheKey(modelName, "claude:"+cacheScopeID)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String(), true
}

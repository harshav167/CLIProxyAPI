package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// principalSalt returns a stable, non-secret per-tenant salt derived from the
// authenticated downstream principal (the inbound proxy API key set as
// "userApiKey" by the auth middleware). It is mixed into every synthetic
// prompt-cache key and execution-session ID so that two tenants with DIFFERENT
// proxy API keys can never collide on client-controlled inputs
// (cursorConversationId, user, model, first-message anchor) and thereby share
// upstream cache slots or WebSocket connections.
//
// The principal is sha256-hashed (never used raw) so the salt is constant
// length and leaks nothing. Returns "" when no principal is present (e.g.
// auth-disabled / open deployments) — in that case behavior is identical to
// the pre-salt scheme, so single-tenant setups are unaffected.
func principalSalt(c *gin.Context) string {
	if c == nil {
		return ""
	}
	v, ok := c.Get("userApiKey")
	if !ok {
		return ""
	}
	principal, _ := v.(string)
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-principal\x00" + principal))
	return hex.EncodeToString(sum[:16])
}

func maybeInjectSyntheticPromptCacheKey(c *gin.Context, rawJSON []byte) []byte {
	if c == nil || c.Request == nil {
		return rawJSON
	}
	if existing := gjson.GetBytes(rawJSON, "prompt_cache_key").String(); existing != "" {
		return rawJSON
	}
	model := gjson.GetBytes(rawJSON, "model").String()
	if model == "" {
		return rawJSON
	}
	// Per-tenant salt so colliding client inputs never cross tenants.
	salt := principalSalt(c)

	// Cursor gives each subagent its own cursorConversationId. Keying those
	// explicit subagent requests on that ID cold-starts every subagent. Use a
	// shared-prefix cache key only when the request carries an explicit subagent
	// signal; normal multi-turn Cursor conversations stay keyed by
	// cursorConversationId below so evolving system context does not churn the
	// cache key across turns.
	if strings.HasPrefix(c.Request.UserAgent(), "Cursor/") && cursorHasExplicitSubagentSignal(c, rawJSON) {
		user := strings.TrimSpace(gjson.GetBytes(rawJSON, "user").String())
		if group := cursorSubagentCacheGroup(c, rawJSON); user != "" && group != "" {
			sum := sha256.Sum256([]byte("cli-proxy\x00cursor-subagent\x00" + salt + "\x00" + user + "\x00" + model + "\x00" + group))
			key := "cli-proxy-" + hex.EncodeToString(sum[:16])
			out, err := sjson.SetBytes(rawJSON, "prompt_cache_key", key)
			if err != nil {
				return rawJSON
			}
			return out
		}
	}

	// Fallback: derive pck from a client-supplied conversation identifier when
	// present. This keeps non-Cursor and prefix-less Cursor requests stable
	// across turns without changing explicit client-provided prompt_cache_key.
	if convID := strings.TrimSpace(gjson.GetBytes(rawJSON, "metadata.cursorConversationId").String()); convID != "" {
		sum := sha256.Sum256([]byte("cli-proxy\x00cursor-conv\x00" + salt + "\x00" + model + "\x00" + convID))
		key := "cli-proxy-" + hex.EncodeToString(sum[:16])
		out, err := sjson.SetBytes(rawJSON, "prompt_cache_key", key)
		if err != nil {
			return rawJSON
		}
		return out
	}

	// Fallback: derive pck from a stable text anchor (first user message,
	// truncated head). Used by clients without a conversation-ID metadata
	// field: generic openai-sdk consumers, Cursor in legacy modes, future
	// BYOK paths whose harnesses don't propagate a parent identifier.
	const anchorMax = 4096
	anchor := firstUserMessageAnchorAnyShape(rawJSON, anchorMax)
	if anchor == "" {
		return rawJSON
	}
	sum := sha256.Sum256([]byte("cli-proxy\x00" + salt + "\x00" + model + "\x00" + anchor))
	key := "cli-proxy-" + hex.EncodeToString(sum[:16])
	out, err := sjson.SetBytes(rawJSON, "prompt_cache_key", key)
	if err != nil {
		return rawJSON
	}
	return out
}

func cursorSubagentCacheGroup(c *gin.Context, rawJSON []byte) string {
	if parent := cursorParentCacheAnchor(c, rawJSON); parent != "" {
		return "parent\x00" + parent
	}
	if anchor := cursorSharedPrefixAnchor(rawJSON); anchor != "" {
		return "prefix\x00" + anchor
	}
	return ""
}

func cursorParentCacheAnchor(c *gin.Context, rawJSON []byte) string {
	if c != nil && c.Request != nil {
		for _, header := range []string{
			"x-codex-parent-thread-id",
			"x-cursor-parent-conversation-id",
			"x-cursor-parent-thread-id",
		} {
			if value := strings.TrimSpace(c.Request.Header.Get(header)); value != "" {
				return header + "\x00" + value
			}
		}
	}
	for _, path := range []string{
		"metadata.parentConversationId",
		"metadata.cursorParentConversationId",
		"metadata.parentThreadId",
		"metadata.cursorParentThreadId",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(rawJSON, path).String()); value != "" {
			return path + "\x00" + value
		}
	}
	return ""
}

func cursorHasExplicitSubagentSignal(c *gin.Context, rawJSON []byte) bool {
	if c != nil && c.Request != nil {
		for _, header := range []string{
			"x-openai-subagent",
			"x-codex-parent-thread-id",
			"x-cursor-parent-conversation-id",
			"x-cursor-subagent",
		} {
			if strings.TrimSpace(c.Request.Header.Get(header)) != "" {
				return true
			}
		}
	}
	for _, path := range []string{
		"metadata.parentConversationId",
		"metadata.cursorParentConversationId",
		"metadata.parentThreadId",
		"metadata.cursorParentThreadId",
	} {
		if strings.TrimSpace(gjson.GetBytes(rawJSON, path).String()) != "" {
			return true
		}
	}
	if value := gjson.GetBytes(rawJSON, "metadata.isSubagent"); value.Bool() || strings.EqualFold(strings.TrimSpace(value.String()), "true") {
		return true
	}
	if value := strings.TrimSpace(gjson.GetBytes(rawJSON, "metadata.subagent").String()); value != "" {
		return true
	}
	return false
}

func cursorSharedPrefixAnchor(rawJSON []byte) string {
	hasher := sha256.New()
	wrote := false
	writePart := func(label, raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		_, _ = hasher.Write([]byte(label))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(raw))
		_, _ = hasher.Write([]byte{0})
		wrote = true
	}
	appendJSON := func(label string, value gjson.Result) {
		if !value.Exists() {
			return
		}
		writePart(label, value.Raw)
	}

	appendJSON("instructions", gjson.GetBytes(rawJSON, "instructions"))
	appendJSON("system", gjson.GetBytes(rawJSON, "system"))
	appendJSON("tools", gjson.GetBytes(rawJSON, "tools"))

	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := strings.TrimSpace(msg.Get("role").String())
			if role != "system" && role != "developer" {
				return true
			}
			raw := strings.TrimSpace(msg.Raw)
			if raw != "" {
				writePart("message:"+role, raw)
			}
			return true
		})
	}

	input := gjson.GetBytes(rawJSON, "input")
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			if role != "system" && role != "developer" {
				return true
			}
			raw := strings.TrimSpace(item.Raw)
			if raw != "" {
				writePart("input:"+role, raw)
			}
			return true
		})
	}

	if !wrote {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func firstUserMessageAnchor(rawJSON []byte, maxLen int) string {
	msgs := gjson.GetBytes(rawJSON, "messages")
	if !msgs.IsArray() {
		return ""
	}
	var out string
	msgs.ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() != "user" {
			return true
		}
		c := m.Get("content")
		if !c.Exists() {
			return true
		}
		if c.IsArray() {
			c.ForEach(func(_, blk gjson.Result) bool {
				if t := blk.Get("text"); t.Exists() && t.Type == gjson.String {
					out = truncateGJSONString(t, maxLen)
					return out == ""
				}
				return true
			})
		} else if c.Type == gjson.String {
			out = truncateGJSONString(c, maxLen)
		}
		return false
	})
	return out
}

func truncateGJSONString(r gjson.Result, maxLen int) string {
	s := r.Str
	if s == "" {
		s = r.String()
	}
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

func firstUserMessageAnchorAnyShape(rawJSON []byte, maxLen int) string {
	if v := firstUserMessageAnchor(rawJSON, maxLen); v != "" {
		return v
	}
	arr := gjson.GetBytes(rawJSON, "input")
	if !arr.IsArray() {
		return ""
	}
	var out string
	arr.ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() != "user" {
			return true
		}
		c := m.Get("content")
		if c.IsArray() {
			c.ForEach(func(_, blk gjson.Result) bool {
				if t := blk.Get("text"); t.Exists() && t.Type == gjson.String {
					out = truncateGJSONString(t, maxLen)
					return false
				}
				return true
			})
		} else if c.Type == gjson.String {
			out = truncateGJSONString(c, maxLen)
		}
		return false
	})
	return out
}

// deriveCursorSessionID synthesizes a stable per-(user, conversation) execution
// session ID for chat-completions traffic that includes a `user` field.
// Designed for Cursor BYOK requests so wsExec reuses one upstream WebSocket
// across all turns of the same conversation — matching the behavior Droid gets
// via openai_responses_websocket.go's explicit WithExecutionSessionID.
//
// Returns empty when any anchor is missing — callers MUST skip
// WithExecutionSessionID in that case so we degrade gracefully to the F1
// per-request fresh-WS path instead of bucketing everything into one shared
// "empty session".
// deriveCursorSessionID synthesizes the execution-session ID. salt is the
// per-tenant principal salt (see principalSalt); it is mixed into every derived
// hash so two tenants with different proxy API keys cannot collide on
// client-controlled inputs and share an upstream WebSocket/connection. salt is
// "" for auth-disabled deployments (single-tenant; behavior unchanged).
//
// Note: when the client supplies its own explicit prompt_cache_key we use it as
// the local session anchor, but SALT+HASH it for the derived session ID so two
// tenants that happen to choose the same key (and share an upstream auth) cannot
// collide onto the same WebSocket/bridge chain. The raw prompt_cache_key is left
// untouched in the payload, so upstream cache behavior is unchanged — only the
// proxy-local session identity is namespaced per principal.
func deriveCursorSessionID(rawJSON []byte, salt string) string {
	if pck := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String()); pck != "" && !strings.HasPrefix(pck, "cli-proxy-") {
		sum := sha256.Sum256([]byte("cursor-pck\x00" + salt + "\x00" + pck))
		return "cursor-pck-" + hex.EncodeToString(sum[:12])
	}
	if convID := strings.TrimSpace(gjson.GetBytes(rawJSON, "metadata.cursorConversationId").String()); convID != "" {
		user := strings.TrimSpace(gjson.GetBytes(rawJSON, "user").String())
		model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if model == "" {
			return ""
		}
		sum := sha256.Sum256([]byte("cursor-conv-session\x00" + salt + "\x00" + user + "\x00" + model + "\x00" + convID))
		return "cursor-conv-" + hex.EncodeToString(sum[:12])
	}
	user := strings.TrimSpace(gjson.GetBytes(rawJSON, "user").String())
	if user == "" {
		return ""
	}
	model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if model == "" {
		return ""
	}
	anchor := firstUserMessageAnchorAnyShape(rawJSON, 4096)
	if anchor == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cursor:" + salt + ":" + user + ":" + model + ":" + anchor))
	return "cursor-" + hex.EncodeToString(sum[:12])
}

func withCursorExecutionSessionID(ctx context.Context, c *gin.Context, rawJSON []byte) context.Context {
	if c == nil || c.Request == nil || !strings.HasPrefix(c.Request.UserAgent(), "Cursor/") {
		return ctx
	}
	ctx = withCursorCacheIdentity(ctx, rawJSON)
	sessionID := deriveCursorSessionID(rawJSON, principalSalt(c))
	if sessionID == "" {
		return ctx
	}
	return handlers.WithExecutionSessionID(ctx, sessionID)
}

func withCursorCacheIdentity(ctx context.Context, rawJSON []byte) context.Context {
	conversationID := strings.TrimSpace(gjson.GetBytes(rawJSON, "metadata.cursorConversationId").String())
	promptCacheKey := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String())
	if conversationID == "" && promptCacheKey == "" {
		return ctx
	}
	return internallogging.WithCacheIdentity(ctx, conversationID, promptCacheKey)
}

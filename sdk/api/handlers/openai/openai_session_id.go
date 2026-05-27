package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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

	// PREFERRED — derive pck from a client-supplied conversation identifier
	// when present. Currently recognized:
	//
	//   metadata.cursorConversationId — UUID emitted by Cursor BYOK on every
	//     chat-completions turn; Cursor is the authority on what constitutes
	//     "one conversation" and (by extension) which subagent runs should
	//     share their parent's cache. Stable across all turns of a chat.
	//
	// Using a client-supplied identifier eliminates two failure modes that
	// content-hash anchors have:
	//   1. False collisions — two truly independent chats opened in the same
	//      workspace state (identical first 4KB of first user message) get
	//      DIFFERENT cursorConversationIds → different pck → no shared cache
	//      slot upstream → no mutual eviction.
	//   2. False splits — if Cursor ever ships subagents that share the parent
	//      conversation context, they'll naturally inherit cursorConversationId
	//      → same pck → subagent reuses parent's warm cache automatically.
	//
	// Future: add other clients' equivalents here as they surface (e.g. a
	// `metadata.parent_message_id` or `X-Conversation-Id` header).
	if convID := gjson.GetBytes(rawJSON, "metadata.cursorConversationId").String(); convID != "" {
		// Prefix per-source so different identifier semantics never collide
		// in the upstream cache namespace. sha256-hash so the public key
		// length stays constant and we don't leak the raw UUID upstream.
		sum := sha256.Sum256([]byte("cli-proxy\x00cursor-conv\x00" + model + "\x00" + convID))
		key := "cli-proxy-" + hex.EncodeToString(sum[:16])
		out, err := sjson.SetBytes(rawJSON, "prompt_cache_key", key)
		if err != nil {
			return rawJSON
		}
		return out
	}

	// FALLBACK — derive pck from a stable text anchor (first user message,
	// truncated head). Used by clients without a conversation-ID metadata
	// field: generic openai-sdk consumers, Cursor in legacy modes, future
	// BYOK paths whose harnesses don't propagate a parent identifier.
	const anchorMax = 4096
	anchor := firstUserMessageAnchorAnyShape(rawJSON, anchorMax)
	if anchor == "" {
		return rawJSON
	}
	sum := sha256.Sum256([]byte("cli-proxy\x00" + model + "\x00" + anchor))
	key := "cli-proxy-" + hex.EncodeToString(sum[:16])
	out, err := sjson.SetBytes(rawJSON, "prompt_cache_key", key)
	if err != nil {
		return rawJSON
	}
	return out
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
func deriveCursorSessionID(rawJSON []byte) string {
	if pck := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String()); pck != "" && !strings.HasPrefix(pck, "cli-proxy-") {
		return "cursor-pck-" + pck
	}
	if convID := strings.TrimSpace(gjson.GetBytes(rawJSON, "metadata.cursorConversationId").String()); convID != "" {
		user := strings.TrimSpace(gjson.GetBytes(rawJSON, "user").String())
		model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if model == "" {
			return ""
		}
		sum := sha256.Sum256([]byte("cursor-conv-session\x00" + user + "\x00" + model + "\x00" + convID))
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
	sum := sha256.Sum256([]byte("cursor:" + user + ":" + model + ":" + anchor))
	return "cursor-" + hex.EncodeToString(sum[:12])
}

func withCursorExecutionSessionID(ctx context.Context, rawJSON []byte) context.Context {
	sessionID := deriveCursorSessionID(rawJSON)
	if sessionID == "" {
		return ctx
	}
	return handlers.WithExecutionSessionID(ctx, sessionID)
}

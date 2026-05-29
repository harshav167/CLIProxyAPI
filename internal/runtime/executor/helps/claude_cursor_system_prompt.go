package helps

// Claude-Cursor system-prompt helpers. Note: separate from cursor_system_prompt.go,
// which patches GPT-5.4 / Codex prompts on the OpenAI handler path.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// LooksLikeCursorSubagent returns true when the request shape suggests a
// short-lived Cursor subagent rather than a parent agent. Used to pick a
// 5m ephemeral cache TTL over the 1h default, since subagents almost never
// re-read past the 5m mark and we waste the 2x write premium otherwise.
//
// Detection is purely STRUCTURAL — based on tool array size, not tool names.
// Tool names would couple this code to Cursor's current product strings
// ("Subagent", "CreatePlan", etc.) which can be renamed at any time.
//
// Empirical band (cliproxy logs docker/logs/cliproxy/cursor-1.0/, 2026-05-11,
// 9 distinct cursorConversationId values):
//
//	parent agents:    17-18 tools (full toolset including spawn/plan capabilities)
//	subagent agents:  14 tools    (trimmed; spawn/plan capabilities removed)
//	non-Cursor:       0-9 or > 20 tools
//
// We classify [10, 16] as subagent-shape. The lower bound rejects bare CLI
// probes and other non-Cursor clients. The upper bound is permissive on the
// parent side — only request shapes that *clearly* match the smaller subagent
// toolset get the 5m downgrade. False positives are cheap (5m on small parent
// = unchanged correctness, slightly more cache writes); false negatives are
// the status quo (1h on subagent = wasted money but still works).
//
// Caller is responsible for ensuring this function only runs on Cursor traffic
// (see the RewriteCursorSystemPromptBlocks early-return guard at the call site).
func LooksLikeCursorSubagent(payload []byte) bool {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	toolCount := int(tools.Get("#").Int())
	return toolCount >= 10 && toolCount <= 16
}

// RewriteCursorSystemPromptBlocks rewrites Cursor's identity-line sentinels and
// places two cache_control breakpoints (first + last system text block) at the
// supplied TTL.
//
// When useGlobalScopeOnLast is true, the LAST text block's cache_control adds
// scope:"global", mirroring the 2026-05-29 canonical Claude Code Opus 4.8 capture
// pattern (last system block is `{"type":"ephemeral","ttl":"1h","scope":"global"}`,
// earlier blocks are bare ttl). This is the position our 2026-05-11 attempt missed
// — that attempt put scope:"global" on the FIRST block, which violated Anthropic's
// prefix-chain validator. The LAST-block position is what claude-cli/2.1.156 ships
// and what Anthropic accepts.
//
// Flag-gated via cfg.ClaudeCursorGlobalCacheScope so prod can roll back fast.
func RewriteCursorSystemPromptBlocks(system gjson.Result, billingBlock string, ttl string, useGlobalScopeOnLast bool) (string, bool) {
	if !system.IsArray() {
		return "", false
	}

	// PATCH 2026-05-11 v5: allow empty billingBlock to skip the prepend.
	// Hypothesis being tested: Anthropic's prefix-chain validator treats a
	// system[0] text block whose content matches the `x-anthropic-billing-header:`
	// pattern as metadata rather than content, effectively making our scope:"global"
	// block (which we put on system[1]) act as their "system[0]" — which the
	// validator rejects when tools are present. Real Claude Code with
	// CLAUDE_CODE_ATTRIBUTION_HEADER=0 ships system[0] = identity text (not a
	// billing header) and gets 200 with scope:"global" + tools. Skipping the
	// billing prepend in the Cursor BYOK path mirrors that behavior.
	var systemBlocks []string
	if strings.TrimSpace(billingBlock) != "" {
		systemBlocks = []string{billingBlock}
	}
	matched := false
	system.ForEach(func(_, part gjson.Result) bool {
		partRaw := part.Raw
		if part.Get("type").String() == "text" {
			if rewritten, ok := RewriteCursorSystemPromptIdentityAndIntegrity(part.Get("text").String()); ok {
				if rewrittenPart, err := sjson.SetBytes([]byte(part.Raw), "text", rewritten); err == nil {
					partRaw = string(rewrittenPart)
					matched = true
				}
			}
		}
		systemBlocks = append(systemBlocks, partRaw)
		return true
	})
	if !matched {
		return "", false
	}

	// PATCH 2026-05-11 v2: dual cache_control breakpoint, mirroring real
	// Claude Code's pattern (claude-cli/2.1.126, Proxyman flow 940):
	//
	//   - FIRST text block:  cache_control { scope: "global", ttl }
	//   - LAST  text block:  cache_control { ttl }                       (no scope)
	//
	// Rationale: putting scope:"global" on the FIRST block (right after the
	// billingBlock) anchors the global-scoped cache at a position that does
	// NOT invalidate when later system content changes. The bare anchor on the
	// last block extends cache coverage and gives a second hit point inside
	// the 20-block lookback window if the conversation grows.
	//
	// Anthropic's prefix-chain rule: a block with scope:"global" must be the
	// FIRST cache_control breakpoint in the (tools → system → messages) chain.
	// Tools have no cache_control (neutral); the FIRST system text block we
	// tag carries scope:"global"; the LAST system block + the user-prompt
	// anchor downstream are narrower (no scope) — that ordering is valid.
	//
	// Earlier scope:global-on-LAST attempts triggered 400s when the rewriter
	// left bare-ephemeral cache_control on intermediate system blocks; we strip
	// all intermediate cache_controls below to keep the chain clean.
	//
	// TTL is the caller-supplied value: "1h" for parent agents, "5m" for
	// subagent-shaped requests.
	// Iterate over ALL system blocks (including index 0 when billingBlock was
	// skipped). When billingBlock is present, system[0] is the billing header
	// which never carries cache_control; when absent, system[0] is the first
	// Cursor text block.
	firstTextIdx, lastTextIdx := -1, -1
	for i := 0; i < len(systemBlocks); i++ {
		if gjson.Get(systemBlocks[i], "type").String() == "text" {
			if firstTextIdx == -1 {
				firstTextIdx = i
			}
			lastTextIdx = i
		}
	}
	if firstTextIdx >= 0 {
		// SINGLE-anchor layout, mirroring the canonical Claude Code 2.1.156
		// Opus 4.8 Proxyman capture (verified 2026-05-29):
		//   - ALL system blocks except the LAST: NO cache_control
		//   - LAST system block: cache_control { ttl [+ scope:"global"] }
		//
		// Anthropic's prefix-chain validator on Opus 4.8 (verified strict on 4.8,
		// lenient on 4.7) rejects requests where a scope:"global" anchor appears
		// AFTER any block carrying a narrower-scope (bare) cache_control. Putting
		// a bare-ttl anchor on the first text block and scope:"global" on the
		// last violated this rule:
		//   "A block with scope:'global' was found after content with a narrower
		//    cache scope. Note that tool definitions render before system blocks,
		//    so scope:'global' on system[0] is not a true prefix when tools are
		//    present."
		// Claude Code achieves this by anchoring ONLY at the last system block
		// — we now do the same. The trade is one breakpoint (vs two), but the
		// one anchor at scope:"global" actually shares the cache prefix across
		// main-agent ↔ subagent boundaries on the same account, which two bare
		// anchors never did.
		//
		// TTL is caller-supplied: "1h" for parent agents, "5m" for subagents.
		// useGlobalScopeOnLast (cfg.ClaudeCursorGlobalCacheScope) gates the
		// scope:"global" addition so we can roll back to bare-ttl without a
		// rebuild if a future Anthropic policy change breaks it.
		for i := firstTextIdx; i < lastTextIdx; i++ {
			if gjson.Get(systemBlocks[i], "cache_control").Exists() {
				if updated, err := sjson.DeleteBytes([]byte(systemBlocks[i]), "cache_control"); err == nil {
					systemBlocks[i] = string(updated)
				}
			}
		}
		var lastCC string
		if useGlobalScopeOnLast {
			lastCC = fmt.Sprintf(`{"type":"ephemeral","ttl":%q,"scope":"global"}`, ttl)
		} else {
			lastCC = fmt.Sprintf(`{"type":"ephemeral","ttl":%q}`, ttl)
		}
		if updated, err := sjson.SetRawBytes([]byte(systemBlocks[lastTextIdx]), "cache_control", []byte(lastCC)); err == nil {
			systemBlocks[lastTextIdx] = string(updated)
		}
	}

	return "[" + strings.Join(systemBlocks, ",") + "]", true
}

// SanitizeForwardedSystemPrompt reduces forwarded third-party system context to
// a tiny neutral reminder for Claude OAuth cloaking. The goal is to preserve
// only the minimum tool/task guidance while removing virtually all client-
// specific prompt structure that Anthropic may classify as third-party agent
// traffic.
func SanitizeForwardedSystemPrompt(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	base := `Use the available tools when needed to help with software engineering tasks.
Keep responses concise and focused on the user's request.
Prefer acting on the user's task over describing product-specific workflows.`

	// Whitelist-extract host's tool-format guidance from known tag wrappers.
	// We only forward instructions about HOW to invoke tools — never client
	// branding, file paths, project names, or workflow blocks that would
	// fingerprint the host (Cursor/Droid/Amp/etc.) to Anthropic.
	var extracted []string
	for _, m := range claudeToolGuidanceTagRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 3 {
			continue
		}
		body := strings.TrimSpace(m[2])
		if body == "" {
			continue
		}
		body = claudeIdentifierLeakageRe.ReplaceAllString(body, "[redacted]")
		body = claudePathLeakageRe.ReplaceAllString(body, "[path]")
		extracted = append(extracted, body)
	}
	if len(extracted) == 0 {
		return strings.TrimSpace(base)
	}
	return strings.TrimSpace(base + "\n\n" + strings.Join(extracted, "\n\n"))
}

// claudeToolGuidanceTagRe matches the inner content of host system-prompt
// sections that describe tool invocation rules. Whitelisted tag names only.
var claudeToolGuidanceTagRe = regexp.MustCompile(`(?is)<(tool_calling|tool_use|tool_calls|tools_usage)>(.*?)</(?:tool_calling|tool_use|tool_calls|tools_usage)>`)

// claudeIdentifierLeakageRe scrubs known third-party-host brand strings.
var claudeIdentifierLeakageRe = regexp.MustCompile(`(?i)\b(cursor|factory[- ]?cli|droid|ampcode|amp[- ]?cli|cline|aider|continue\.dev|windsurf|roocode|zed|copilot|github[- ]?copilot)\b`)

// claudePathLeakageRe scrubs absolute filesystem paths and home-relative paths.
var claudePathLeakageRe = regexp.MustCompile(`(?i)(/Users/[^\s"'\)]+|/home/[^\s"'\)]+|\$HOME[^\s"'\)]*|~/[^\s"'\)]+|C:\\[^\s"'\)]+)`)

// BuildClaudeTextBlock constructs a JSON text block object with proper
// escaping. Uses sjson.SetBytes to handle multi-line text, quotes, and control
// characters. cacheControl is optional; pass nil to omit cache_control.
// Supported keys: "scope" (e.g. "global"), "ttl" (e.g. "300").
func BuildClaudeTextBlock(text string, cacheControl map[string]string) string {
	block := []byte(`{"type":"text"}`)
	block, _ = sjson.SetBytes(block, "text", text)
	if cacheControl != nil && len(cacheControl) > 0 {
		// Build cache_control JSON manually to avoid sjson map marshaling issues.
		cc := `{"type":"ephemeral"`
		if scope, ok := cacheControl["scope"]; ok {
			cc += fmt.Sprintf(`,"scope":"%s"`, scope)
		}
		if t, ok := cacheControl["ttl"]; ok {
			cc += fmt.Sprintf(`,"ttl":"%s"`, t)
		}
		cc += "}"
		block, _ = sjson.SetRawBytes(block, "cache_control", []byte(cc))
	}
	return string(block)
}

// PrependClaudeToFirstUserMessage prepends text content to the first user
// message. This avoids putting non-Claude-Code system instructions in
// system[] which triggers Anthropic's extra usage billing for OAuth-proxied
// requests.
func PrependClaudeToFirstUserMessage(payload []byte, text string) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Find the first user message index
	firstUserIdx := -1
	messages.ForEach(func(idx, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			firstUserIdx = int(idx.Int())
			return false
		}
		return true
	})

	if firstUserIdx < 0 {
		return payload
	}

	prefixBlock := fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)

	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		newBlock := fmt.Sprintf(`{"type":"text","text":%q}`, prefixBlock)
		var newArray string
		if content.Raw == "[]" || content.Raw == "" {
			newArray = "[" + newBlock + "]"
		} else {
			newArray = "[" + newBlock + "," + content.Raw[1:]
		}
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte(newArray))
	} else if content.Type == gjson.String {
		newText := prefixBlock + content.String()
		payload, _ = sjson.SetBytes(payload, contentPath, newText)
	}

	return payload
}

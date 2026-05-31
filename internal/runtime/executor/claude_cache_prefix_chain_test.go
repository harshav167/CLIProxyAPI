package executor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// runFullCachePipeline reproduces the exact sequence prepareMessagesRequest
// applies after translation, so the harness proves the SAME bytes that reach
// Anthropic — not just the system-rewrite step in isolation. Mirrors
// claude_executor.go prepareMessagesRequest lines 154-171.
func runFullCachePipeline(body []byte, useGlobalScopeOnLast, isCursor bool) []byte {
	// applyCloaking → checkSystemInstructionsWithSigningMode is the system step.
	body = checkSystemInstructionsWithSigningMode(body, false, false, false, "2.1.63", "", "", useGlobalScopeOnLast)
	// Then the message/auto anchors and limit/normalize passes, in order.
	if helps.CountClaudeCacheControls(body) == 0 {
		body = helps.EnsureClaudeCacheControl(body)
	}
	body = helps.EnsureClaudeUserPromptCacheAnchor(body)
	// Mirror claude_executor.go: skip the top-level automatic cache_control when
	// global scope is on (Claude Code never sends it; it breaks the prefix chain).
	if isCursor && !useGlobalScopeOnLast {
		body = helps.EnsureCursorClaudeAutomaticPromptCacheControl(body)
	}
	body = helps.EnforceClaudeCacheControlLimit(body, 4)
	body = helps.NormalizeClaudeCacheControlTTL(body)
	// Mirror claude_executor.go prepareMessagesRequest: on the global-scope path
	// strip every cache_control rendering BEFORE the system global anchor
	// (top-level + tool-level) so the anchor leads the prefix chain. This step
	// is what the original harness omitted; without it an inbound top-level/tool
	// cache_control survives and 400s.
	if useGlobalScopeOnLast {
		body = helps.StripClaudeCacheControlsBeforeGlobalAnchor(body)
	}
	return body
}

// This harness encodes the prefix-chain invariant byte-verified against the
// canonical Claude Code 2.1.156 Opus 4.8 captures (gist 3a0a2db78a42c00b0029d8f2cc1c070b,
// fetched 2026-05-29). The verified layout across all 4 Opus 4.8 requests was:
//
//	render order: tools -> system -> messages
//	tools[*]                       NO cache_control
//	system[0] billing header       NO cache_control
//	system[1] "You are Claude Code" NO cache_control
//	system[2] core agent prompt    cache_control {ttl:1h, scope:"global"}   <- FIRST anchor, GLOBAL
//	system[3] later system block   cache_control {ttl:1h}                   <- bare, AFTER global
//	messages[last]                 cache_control {ttl:1h}                   <- bare, AFTER global
//
// The single hard rule Anthropic enforces (and that broke prod when violated):
// the FIRST cache_control block in render order must be the scope:"global" one;
// nothing carrying cache_control may render before it. Bare anchors AFTER the
// global one are fine.
//
// assertPrefixChainValid scans a built upstream body in render order and fails
// if any cache_control block appears before the first scope:"global" block.
// When there is no global block at all (flag off), the chain is trivially valid.
func assertPrefixChainValid(t *testing.T, body []byte) {
	t.Helper()

	type anchor struct {
		loc   string
		scope string
	}
	var chain []anchor

	root := gjson.ParseBytes(body)
	// Top-level request cache_control renders BEFORE tools/system/messages in
	// Anthropic's prefix chain (it is a request-level breakpoint). If it carries
	// a narrower scope than a later scope:"global" block, the request 400s. This
	// is the exact field Claude Code never sends (all 4 gist captures have
	// top-level cache_control = null) and the one our first harness ignored.
	if cc := root.Get("cache_control"); cc.Exists() {
		chain = append(chain, anchor{loc: "<top-level>", scope: cc.Get("scope").String()})
	}
	root.Get("tools").ForEach(func(i, tool gjson.Result) bool {
		if cc := tool.Get("cache_control"); cc.Exists() {
			chain = append(chain, anchor{loc: "tools[" + i.String() + "]", scope: cc.Get("scope").String()})
		}
		return true
	})
	root.Get("system").ForEach(func(i, blk gjson.Result) bool {
		if cc := blk.Get("cache_control"); cc.Exists() {
			chain = append(chain, anchor{loc: "system[" + i.String() + "]", scope: cc.Get("scope").String()})
		}
		return true
	})
	root.Get("messages").ForEach(func(mi, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(ci, blk gjson.Result) bool {
			if cc := blk.Get("cache_control"); cc.Exists() {
				chain = append(chain, anchor{loc: "messages[" + mi.String() + "].content[" + ci.String() + "]", scope: cc.Get("scope").String()})
			}
			return true
		})
		return true
	})

	firstGlobal := -1
	for i, a := range chain {
		if a.scope == "global" {
			firstGlobal = i
			break
		}
	}
	if firstGlobal == -1 {
		// No global anchor: chain is trivially valid (this is the flag-off path).
		return
	}
	// Every anchor before the first global one is a violation.
	for i := 0; i < firstGlobal; i++ {
		t.Errorf("prefix-chain violation: %s carries cache_control (scope=%q) BEFORE the first scope:global anchor at %s; Anthropic 400s this",
			chain[i].loc, chain[i].scope, chain[firstGlobal].loc)
	}
	// Anthropic also caps total breakpoints at 4.
	if len(chain) > 4 {
		t.Errorf("too many cache_control anchors: %d (Anthropic max 4)", len(chain))
	}
}

// representativeCursorClaudeBody builds the downstream shape Cursor BYOK Claude
// actually sends, verified against docker/logs/cliproxy/cursor-1.0 fixtures:
// ONE consolidated system block carrying Cursor identity sentinels + its own
// bare ephemeral cache_control, plus tools and a couple of user/assistant turns.
func representativeCursorClaudeBody() []byte {
	sysText := strings.Join([]string{
		"You are an AI coding assistant, powered by Claude.",
		"",
		"You operate in Cursor.",
		"",
		"You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.",
		"",
		"<tool_calling>",
		"Use tools to accomplish the task.",
		"</tool_calling>",
	}, "\n")

	body := `{
		"model": "claude-opus-4-8",
		"max_tokens": 64000,
		"stream": true,
		"thinking": {"type":"adaptive"},
		"output_config": {"effort":"high"},
		"system": [
			{"type":"text","text":` + jsonQuote(sysText) + `,"cache_control":{"type":"ephemeral"}}
		],
		"tools": [
			{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{}}},
			{"name":"edit_file","description":"Edit a file","input_schema":{"type":"object","properties":{}}}
		],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"first turn question"}]},
			{"role":"assistant","content":[{"type":"text","text":"first answer"}]},
			{"role":"user","content":[{"type":"text","text":"second turn question"}]}
		]
	}`
	return []byte(body)
}

// representativeCursorClaudeBodyWithPreAnchorCacheControls builds the worst-case
// inbound shape: Cursor sends its own bare top-level cache_control AND a bare
// tool-level cache_control, both of which render BEFORE the system blocks in
// Anthropic's prefix chain. With the global flag on, the system rewrite emits a
// scope:"global" anchor; if these pre-anchor bare controls survive, the request
// 400s ("a block with scope:global was found after content with a narrower
// cache scope"). This is the exact scenario the original harness never covered.
func representativeCursorClaudeBodyWithPreAnchorCacheControls() []byte {
	sysText := strings.Join([]string{
		"You are an AI coding assistant, powered by Claude.",
		"",
		"You operate in Cursor.",
		"",
		"You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.",
		"",
		"<tool_calling>",
		"Use tools to accomplish the task.",
		"</tool_calling>",
	}, "\n")

	body := `{
		"model": "claude-opus-4-8",
		"max_tokens": 64000,
		"stream": true,
		"thinking": {"type":"adaptive"},
		"output_config": {"effort":"high"},
		"cache_control": {"type":"ephemeral"},
		"system": [
			{"type":"text","text":` + jsonQuote(sysText) + `,"cache_control":{"type":"ephemeral"}}
		],
		"tools": [
			{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{}}},
			{"name":"edit_file","description":"Edit a file","input_schema":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}}
		],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"first turn question"}]},
			{"role":"assistant","content":[{"type":"text","text":"first answer"}]},
			{"role":"user","content":[{"type":"text","text":"second turn question"}]}
		]
	}`
	return []byte(body)
}

func jsonQuote(s string) string {
	// minimal JSON string quoting for the fixture builder
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// TestCursorClaudeCacheChainFlagOff proves the deployed (flag-off) layout never
// emits a scope:"global" block, so the prefix-chain rule cannot be violated.
func TestCursorClaudeCacheChainFlagOff(t *testing.T) {
	body := representativeCursorClaudeBody()

	// flag off → checkSystemInstructionsWithSigningMode(..., useGlobalScopeOnLast=false)
	out := checkSystemInstructionsWithSigningMode(body, false, false, false, "2.1.63", "", "", false)

	if gjson.GetBytes(out, "system").IsArray() {
		var globals int
		gjson.GetBytes(out, "system").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("cache_control.scope").String() == "global" {
				globals++
			}
			return true
		})
		if globals != 0 {
			t.Errorf("flag-off path must NOT emit scope:global; found %d", globals)
		}
	}
	assertPrefixChainValid(t, out)
}

// TestCursorClaudeCacheChainFlagOn proves the flag-on layout satisfies the
// byte-verified prefix-chain invariant: the global anchor is the FIRST
// cache_control in render order, with nothing carrying cache_control before it.
func TestCursorClaudeCacheChainFlagOn(t *testing.T) {
	body := representativeCursorClaudeBody()

	out := checkSystemInstructionsWithSigningMode(body, false, false, false, "2.1.63", "", "", true)

	// Must have produced at least one global anchor (otherwise the flag did nothing).
	var globals int
	gjson.GetBytes(out, "system").ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("cache_control.scope").String() == "global" {
			globals++
		}
		return true
	})
	if globals == 0 {
		t.Fatalf("flag-on path must emit a scope:global anchor; found none. system=%s", gjson.GetBytes(out, "system").Raw)
	}

	assertPrefixChainValid(t, out)
}

// TestCursorClaudeFullPipelineFlagOff reproduces the EXACT bytes prepareMessagesRequest
// emits (system rewrite + all message/auto anchor injectors + limit + normalize)
// for the deployed flag-off config. This is the real prod path; it must stay valid.
func TestCursorClaudeFullPipelineFlagOff(t *testing.T) {
	out := runFullCachePipeline(representativeCursorClaudeBody(), false, true)
	assertPrefixChainValid(t, out)
}

// TestCursorClaudeFullPipelineFlagOn is the gate for re-enabling global scope.
// It runs the full injector chain (the part my first harness skipped and that
// caused the live 400s) and asserts the global anchor is first with nothing
// before it. If this fails, do NOT flip the flag in prod.
func TestCursorClaudeFullPipelineFlagOn(t *testing.T) {
	out := runFullCachePipeline(representativeCursorClaudeBody(), true, true)
	assertPrefixChainValid(t, out)

	// And confirm a global anchor actually survived the full chain.
	var globals int
	root := gjson.ParseBytes(out)
	root.Get("system").ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("cache_control.scope").String() == "global" {
			globals++
		}
		return true
	})
	if globals == 0 {
		t.Fatalf("flag-on full pipeline produced no surviving scope:global anchor; system=%s", root.Get("system").Raw)
	}
}

// TestCursorClaudeFullPipelineFlagOnStripsInboundPreAnchorCacheControls is the
// regression test for the prefix-chain 400 the deployed strip fixes. It feeds an
// inbound body carrying BOTH a bare top-level cache_control and a bare tool-level
// cache_control (which render before system in Anthropic's chain), runs the full
// flag-on pipeline, and asserts:
//   - the outgoing payload has NO top-level cache_control,
//   - NO tool carries cache_control,
//   - a scope:"global" anchor exists and is the FIRST breakpoint in render order.
func TestCursorClaudeFullPipelineFlagOnStripsInboundPreAnchorCacheControls(t *testing.T) {
	in := representativeCursorClaudeBodyWithPreAnchorCacheControls()

	// Sanity: the inbound body really does carry the pre-anchor controls we want
	// to prove get stripped (otherwise the test would pass vacuously).
	if !gjson.GetBytes(in, "cache_control").Exists() {
		t.Fatal("fixture missing inbound top-level cache_control")
	}
	var inboundToolCC bool
	gjson.GetBytes(in, "tools").ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			inboundToolCC = true
			return false
		}
		return true
	})
	if !inboundToolCC {
		t.Fatal("fixture missing inbound tool-level cache_control")
	}

	out := runFullCachePipeline(in, true, true)

	// The global anchor must lead the chain with nothing carrying cache_control
	// before it (this is the assertion that 400'd in prod before the strip).
	assertPrefixChainValid(t, out)

	root := gjson.ParseBytes(out)
	if root.Get("cache_control").Exists() {
		t.Errorf("top-level cache_control survived the strip; body=%s", string(out))
	}
	root.Get("tools").ForEach(func(i, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			t.Errorf("tools[%s] cache_control survived the strip; body=%s", i.String(), string(out))
		}
		return true
	})

	var globals int
	root.Get("system").ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("cache_control.scope").String() == "global" {
			globals++
		}
		return true
	})
	if globals == 0 {
		t.Fatalf("no surviving scope:global anchor after strip; system=%s", root.Get("system").Raw)
	}
}

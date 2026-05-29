package helps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func cursorIdentityBlock(t *testing.T, label string) string {
	t.Helper()
	text := strings.Join([]string{
		"You are an AI coding assistant, powered by Opus 4.7.",
		"",
		"You operate in Cursor.",
		"",
		"You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.",
		"",
		label,
	}, "\n")
	b, err := json.Marshal(map[string]any{"type": "text", "text": text})
	if err != nil {
		t.Fatalf("marshal identity block: %v", err)
	}
	return string(b)
}

// TestRewriteCursorSystemPromptBlocks_GlobalScopeOnLastWhenEnabled is the
// regression test for the 2026-05-29 scope:"global" restoration. With the
// flag ON, the LAST system text block must carry scope:"global", and the
// FIRST text block must remain bare ttl-only. This mirrors the canonical
// Claude Code Opus 4.8 capture pattern (last system block has scope:global,
// earlier blocks are bare ttl).
func TestRewriteCursorSystemPromptBlocks_GlobalScopeOnLastWhenEnabled(t *testing.T) {
	// Build a minimal system array with 2 Cursor-identity text blocks so the
	// rewriter matches and produces two text blocks with cache_control.
	firstRaw := cursorIdentityBlock(t, "Project metadata lives under .cursor/projects/example.")
	lastRaw := cursorIdentityBlock(t, "Cursor UI references should remain because they describe the host surface.")
	system := gjson.Parse("[" + firstRaw + "," + lastRaw + "]")

	out, ok := RewriteCursorSystemPromptBlocks(system, "", "1h", true)
	if !ok {
		t.Fatalf("RewriteCursorSystemPromptBlocks did not match Cursor identity sentinels; output=%q", out)
	}

	parsed := gjson.Parse(out)
	if !parsed.IsArray() {
		t.Fatalf("rewritten system is not an array: %s", out)
	}
	blocks := parsed.Array()
	if len(blocks) < 2 {
		t.Fatalf("expected >=2 system blocks, got %d: %s", len(blocks), out)
	}

	firstBlock := blocks[0]
	lastBlock := blocks[len(blocks)-1]

	// Single-anchor layout (matches canonical Claude Code 2.1.156 Opus 4.8
	// capture): cache_control ONLY on the last system block. Earlier blocks
	// must remain cache_control-free so the scope:"global" anchor is the
	// FIRST cache_control in the prefix chain. Anthropic's prefix-chain
	// validator rejects bare-cache_control before scope:"global".
	if firstBlock.Get("cache_control").Exists() {
		t.Errorf("first block must NOT have cache_control (single-anchor layout); got %s", firstBlock.Get("cache_control").Raw)
	}

	if got := lastBlock.Get("cache_control.ttl").String(); got != "1h" {
		t.Errorf("last block cache_control.ttl = %q, want %q", got, "1h")
	}
	if got := lastBlock.Get("cache_control.scope").String(); got != "global" {
		t.Errorf("last block cache_control.scope = %q, want %q; block=%s", got, "global", lastBlock.Raw)
	}
}

// TestRewriteCursorSystemPromptBlocks_NoGlobalScopeWhenDisabled confirms the
// flag-off path remains the historical safe behavior from PATCH 2026-05-11 v6:
// both first and last system text blocks carry bare ttl-only cache_control,
// no scope.
func TestRewriteCursorSystemPromptBlocks_NoGlobalScopeWhenDisabled(t *testing.T) {
	firstRaw := cursorIdentityBlock(t, "Project metadata lives under .cursor/projects/example.")
	lastRaw := cursorIdentityBlock(t, "Cursor UI references should remain because they describe the host surface.")
	system := gjson.Parse("[" + firstRaw + "," + lastRaw + "]")

	out, ok := RewriteCursorSystemPromptBlocks(system, "", "1h", false)
	if !ok {
		t.Fatalf("rewriter did not match; out=%q", out)
	}
	if strings.Contains(out, `"scope":"global"`) {
		t.Errorf("flag-off output should not contain scope:global; got %s", out)
	}
}

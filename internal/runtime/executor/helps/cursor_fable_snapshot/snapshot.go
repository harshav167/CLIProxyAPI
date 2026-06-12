// Package cursor_fable_snapshot bakes a frozen Cursor→Anthropic Claude request
// shape (system blocks + native tools array) and exposes a single helper that
// swaps an incoming request's system+tools with that snapshot.
//
// Why this exists: Cursor's BYOK routing layer refuses to route `claude-fable-5`
// requests to non-first-party Anthropic providers ("routing_unsupported …
// requires non-ZDR provider policy"). To work around that, we expose
// non-claude-prefixed aliases (f5-low, …, f5-max) that Cursor classifies as
// generic custom models, so BYOK forwards them to our proxy with the standard
// custom-model body. But Cursor sends a DIFFERENT system prompt + DIFFERENT
// tool set to custom models (no native Anthropic-shape `Read`, `Edit`,
// `Shell`, etc., no Cursor Claude system blocks). To still hit fable-5 with
// the same identity Cursor uses for its real Claude requests, we swap those
// two fields with the captured snapshot before forwarding.
//
// Source of the snapshot: production log
// v1-chat-completions-2026-06-12T162540-fab1d524.log on the production VM, an
// Opus 4.7 thinking-max request, post our existing Cursor rewrite pipeline
// (integrity contract + identity rewrite already applied). The only edit is
// the "powered by Claude Opus 4.7." → "powered by Claude Fable 5." swap so
// the assistant's self-identity matches the model we route to.
package cursor_fable_snapshot

import (
	_ "embed"
)

//go:embed system.json
var systemJSON []byte

//go:embed tools.json
var toolsJSON []byte

// SystemBlocks returns the raw JSON array of Anthropic-style system text
// blocks captured from the Opus 4.7 prod request (model identity already
// swapped to Fable 5).
func SystemBlocks() []byte {
	out := make([]byte, len(systemJSON))
	copy(out, systemJSON)
	return out
}

// Tools returns the raw JSON array of native Cursor tool definitions captured
// from the same prod request (Shell, Read, Grep, Glob, Task, TodoWrite, …).
func Tools() []byte {
	out := make([]byte, len(toolsJSON))
	copy(out, toolsJSON)
	return out
}

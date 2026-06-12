package helps

import (
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor_fable_snapshot"
)

// fableAliasModelPrefix is the prefix Cursor sees in its model picker for the
// non-claude-prefixed Fable 5 aliases (f5-low / f5-medium / f5-high /
// f5-xhigh / f5-max). The prefix is what bypasses Cursor's
// "claude-fable-5 requires non-ZDR provider policy" routing gate: Cursor only
// applies that gate to model IDs that start with "claude-".
const fableAliasModelPrefix = "f5-"

// IsCursorFableAliasModel reports whether the supplied model id is one of the
// non-claude-prefixed Fable 5 aliases that should be reshaped via the captured
// snapshot.
func IsCursorFableAliasModel(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), fableAliasModelPrefix)
}

// ApplyCursorFableAliasSnapshot replaces the request's "system" and "tools"
// fields with the captured Cursor → Anthropic Claude snapshot when the model
// id is one of the f5-* aliases. Other fields (messages, max_tokens, thinking,
// model itself, etc.) are left untouched — the caller still routes to the
// claude-fable-5 backend via the existing oauth-model-alias config, and the
// downstream Claude executor still runs its own system-prompt rewrite
// pipeline, which is idempotent on the already-rewritten snapshot content.
//
// Returns the payload unchanged when the alias does not match.
//
// Both sjson writes are applied transactionally: if either fails (which can
// only happen if the inbound payload is not a JSON object, in which case the
// request would already have errored upstream of here), the original payload
// is returned with no partial mutation, so the system + tools fields can never
// drift out of sync. The two errors are logged at error level so any future
// change that breaks this invariant is surfaced in observability.
func ApplyCursorFableAliasSnapshot(payload []byte, model string) []byte {
	if !IsCursorFableAliasModel(model) {
		return payload
	}
	withSystem, err := sjson.SetRawBytes(payload, "system", cursor_fable_snapshot.SystemBlocks())
	if err != nil {
		log.WithError(err).WithField("model", model).Error("cursor fable alias: failed to inject system snapshot; leaving payload untouched")
		return payload
	}
	withTools, err := sjson.SetRawBytes(withSystem, "tools", cursor_fable_snapshot.Tools())
	if err != nil {
		log.WithError(err).WithField("model", model).Error("cursor fable alias: failed to inject tools snapshot after system swap; rolling back to original payload")
		return payload
	}
	return withTools
}

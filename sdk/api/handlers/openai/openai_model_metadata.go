package openai

func normalizeOpenAIModelMetadata(out map[string]any) {
	id, ok := out["id"].(string)
	if !ok {
		return
	}
	version, vok := out["version"].(string)
	if !vok || version == "" || version == id {
		return
	}

	// codex-spud must stay selectable/displayed as codex-spud, but Cursor
	// should classify it through its GPT-5.5/OpenAI prompt stack.
	if id == "codex-spud" {
		out["display_name"] = id
		out["owned_by"] = "openai"
		return
	}

	// For aliased entries (where id != version, e.g. id=codex-5.5-high
	// but version=gpt-5.5), rewrite `version` and `display_name` to the
	// alias id so clients (notably Cursor) can't preset-match on the
	// underlying model name and override our reported context_length.
	out["version"] = id
	out["display_name"] = id

	// Break Cursor's owned_by="openai" family recognition only for aliases
	// whose real context_window is smaller than what Cursor's preset would
	// assume. For gpt-5.5 aliases (272K real, Cursor assumes 1M) we spoof to
	// prevent Cursor from disabling compaction. For gpt-5.4 aliases (1M real,
	// Cursor assumes 1M) we keep owned_by="openai" so Cursor displays the
	// correct 1M context and handles images through its native path.
	if shouldSpoofOwnerForAlias(out) {
		out["owned_by"] = "cpapi-plus"
	}
}

func shouldSpoofOwnerForAlias(out map[string]any) bool {
	cwVal, cwOk := out["context_window"]
	if !cwOk {
		return false
	}
	switch cw := cwVal.(type) {
	case float64:
		return cw < 500000
	case int:
		return cw < 500000
	case int64:
		return cw < 500000
	default:
		return false
	}
}

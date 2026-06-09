package registry

import "testing"

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}

// TestWithXAIBuiltinsOverridesComposerWindow guards the grok-composer-2.5-fast
// context/output window against the stale 128k/32k values served by the remote
// models.json feed. The built-in must replace any matching ID with the
// advertised 256k window.
func TestWithXAIBuiltinsOverridesComposerWindow(t *testing.T) {
	// Seed a stale remote-style entry the built-in must override.
	stale := []*ModelInfo{{
		ID:                  xaiBuiltinComposerModelID,
		Type:                "xai",
		ContextLength:       131072,
		MaxCompletionTokens: 32768,
	}}

	models := WithXAIBuiltins(stale)

	var composer *ModelInfo
	count := 0
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinComposerModelID {
			composer = model
			count++
		}
	}
	if composer == nil {
		t.Fatalf("expected builtin %s in output", xaiBuiltinComposerModelID)
	}
	if count != 1 {
		t.Fatalf("expected exactly one %s entry, got %d (stale entry not replaced)", xaiBuiltinComposerModelID, count)
	}
	if composer.ContextLength != 256000 {
		t.Fatalf("context_length = %d, want 256000", composer.ContextLength)
	}
	if composer.MaxCompletionTokens != 256000 {
		t.Fatalf("max_completion_tokens = %d, want 256000", composer.MaxCompletionTokens)
	}
}

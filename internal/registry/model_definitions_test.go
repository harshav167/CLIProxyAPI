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

func TestWithXAIBuiltinsIncludesGrok45(t *testing.T) {
	stale := []*ModelInfo{{
		ID:                  xaiBuiltinGrok45ModelID,
		Type:                "xai",
		ContextLength:       128000,
		MaxCompletionTokens: 32768,
		Thinking: &ThinkingSupport{
			ZeroAllowed: true,
			Levels:      []string{"none", "low", "medium", "high"},
		},
	}}
	models := WithXAIBuiltins(stale)

	var grok45 *ModelInfo
	count := 0
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinGrok45ModelID {
			grok45 = model
			count++
		}
	}
	if grok45 == nil {
		t.Fatalf("expected builtin %s in output", xaiBuiltinGrok45ModelID)
	}
	if count != 1 {
		t.Fatalf("expected exactly one %s entry, got %d", xaiBuiltinGrok45ModelID, count)
	}
	if grok45.ContextLength != 500000 {
		t.Fatalf("context_length = %d, want 500000", grok45.ContextLength)
	}
	if grok45.Thinking == nil || grok45.Thinking.ZeroAllowed {
		t.Fatalf("grok-4.5 must not allow zero/none effort, got %+v", grok45.Thinking)
	}
	if len(grok45.Thinking.Levels) != 3 {
		t.Fatalf("grok-4.5 levels = %v, want low/medium/high", grok45.Thinking.Levels)
	}
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}

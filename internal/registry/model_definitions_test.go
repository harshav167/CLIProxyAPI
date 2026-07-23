package registry

import (
	"strings"
	"testing"
)

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

func TestGetKimiModelsIncludesLiveCodingCatalog(t *testing.T) {
	want := map[string]struct {
		contextLength int
		maxTokens     int
		thinking      func(*ThinkingSupport) bool
	}{
		"k3": {
			contextLength: 1_048_576,
			maxTokens:     1_048_576,
			thinking: func(support *ThinkingSupport) bool {
				return support != nil &&
					!support.ZeroAllowed &&
					support.DynamicAllowed &&
					len(support.Levels) == 3 &&
					support.Levels[0] == "low" &&
					support.Levels[1] == "high" &&
					support.Levels[2] == "max"
			},
		},
		"kimi-for-coding": {
			contextLength: 262_144,
			maxTokens:     32_768,
			thinking: func(support *ThinkingSupport) bool {
				return support != nil &&
					!support.ZeroAllowed &&
					support.DynamicAllowed &&
					len(support.Levels) == 0
			},
		},
		"kimi-for-coding-highspeed": {
			contextLength: 262_144,
			maxTokens:     32_768,
			thinking: func(support *ThinkingSupport) bool {
				return support != nil &&
					!support.ZeroAllowed &&
					support.DynamicAllowed &&
					len(support.Levels) == 0
			},
		},
	}

	models := GetKimiModels()
	if len(models) != len(want) {
		t.Fatalf("Kimi builtins = %d, want exactly live catalog %d: %+v", len(models), len(want), models)
	}
	for _, model := range models {
		expected, ok := want[model.ID]
		if !ok {
			t.Fatalf("stale or unknown Kimi builtin %q", model.ID)
		}
		if model.ContextLength != expected.contextLength {
			t.Fatalf("%s context_length = %d, want %d", model.ID, model.ContextLength, expected.contextLength)
		}
		if model.MaxCompletionTokens != expected.maxTokens {
			t.Fatalf("%s max_completion_tokens = %d, want %d", model.ID, model.MaxCompletionTokens, expected.maxTokens)
		}
		if len(model.SupportedInputModalities) != 3 {
			t.Fatalf("%s supported input modalities = %v, want TEXT/IMAGE/VIDEO", model.ID, model.SupportedInputModalities)
		}
		if !expected.thinking(model.Thinking) {
			t.Fatalf("%s thinking metadata = %+v, want live wire capability", model.ID, model.Thinking)
		}
		delete(want, model.ID)
	}

	if len(want) != 0 {
		t.Fatalf("missing live Kimi Coding models: %v", want)
	}
}

func TestGetAntigravityModelsIncludesCurrentGeminiMultimodalCatalog(t *testing.T) {
	want := map[string]struct {
		contextLength int
		maxTokens     int
		thinkingMin   int
		thinkingMax   int
		dynamic       bool
	}{
		"gemini-3-flash":             {contextLength: 1_048_576, maxTokens: 65_536, thinkingMin: 32, thinkingMax: 65_536, dynamic: true},
		"gemini-3-flash-agent":       {contextLength: 1_048_576, maxTokens: 65_536, thinkingMin: 32, thinkingMax: 10_000},
		"gemini-pro-agent":           {contextLength: 1_048_576, maxTokens: 65_535, thinkingMin: 128, thinkingMax: 10_001},
		"gemini-3.1-pro-low":         {contextLength: 1_048_576, maxTokens: 65_535, thinkingMin: 128, thinkingMax: 1_001},
		"gemini-3.5-flash-low":       {contextLength: 1_048_576, maxTokens: 65_536, thinkingMin: 32, thinkingMax: 4_000},
		"gemini-3.5-flash-extra-low": {contextLength: 1_048_576, maxTokens: 65_536, thinkingMin: 32, thinkingMax: 1_000},
		"gemini-3.6-flash-tiered":    {contextLength: 1_048_576, maxTokens: 65_536, thinkingMin: 32, thinkingMax: 65_536, dynamic: true},
	}

	for _, model := range GetAntigravityModels() {
		expected, ok := want[model.ID]
		if !ok {
			continue
		}
		if model.ContextLength != expected.contextLength || model.MaxCompletionTokens != expected.maxTokens {
			t.Fatalf("%s token limits = %d/%d, want %d/%d", model.ID, model.ContextLength, model.MaxCompletionTokens, expected.contextLength, expected.maxTokens)
		}
		if got := strings.Join(model.SupportedInputModalities, ","); got != "TEXT,IMAGE,VIDEO" {
			t.Fatalf("%s input modalities = %q, want TEXT,IMAGE,VIDEO", model.ID, got)
		}
		if got := strings.Join(model.SupportedOutputModalities, ","); got != "TEXT" {
			t.Fatalf("%s output modalities = %q, want TEXT", model.ID, got)
		}
		if model.Thinking == nil ||
			model.Thinking.Min != expected.thinkingMin ||
			model.Thinking.Max != expected.thinkingMax ||
			model.Thinking.DynamicAllowed != expected.dynamic ||
			len(model.Thinking.Levels) != 0 {
			t.Fatalf("%s thinking metadata = %+v, want min=%d max=%d dynamic=%v without synthetic levels", model.ID, model.Thinking, expected.thinkingMin, expected.thinkingMax, expected.dynamic)
		}
		delete(want, model.ID)
	}

	if len(want) != 0 {
		t.Fatalf("missing current Antigravity Gemini models: %v", want)
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

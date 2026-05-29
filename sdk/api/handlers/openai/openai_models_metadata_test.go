package openai

import "testing"

func TestNormalizeOpenAIModelMetadataKeepsCodexSpudOnGPTPromptStack(t *testing.T) {
	model := map[string]any{
		"id":             "codex-spud",
		"version":        "gpt-5.5",
		"display_name":   "gpt-5.5",
		"owned_by":       "cpapi-plus",
		"context_window": 272000,
	}

	normalizeOpenAIModelMetadata(model)

	if got := model["id"]; got != "codex-spud" {
		t.Fatalf("id changed: got %v", got)
	}
	if got := model["version"]; got != "gpt-5.5" {
		t.Fatalf("codex-spud should keep upstream GPT version, got %v", got)
	}
	if got := model["display_name"]; got != "codex-spud" {
		t.Fatalf("codex-spud should stay visible as codex-spud, got %v", got)
	}
	if got := model["owned_by"]; got != "openai" {
		t.Fatalf("codex-spud should be classified as OpenAI for Cursor GPT prompt stack, got %v", got)
	}
}

func TestNormalizeOpenAIModelMetadataSpoofsSmallGPT55Aliases(t *testing.T) {
	model := map[string]any{
		"id":             "codex-5.5-high",
		"version":        "gpt-5.5",
		"display_name":   "gpt-5.5",
		"owned_by":       "openai",
		"context_window": 272000,
	}

	normalizeOpenAIModelMetadata(model)

	if got := model["version"]; got != "codex-5.5-high" {
		t.Fatalf("small alias version should be rewritten to alias id, got %v", got)
	}
	if got := model["display_name"]; got != "codex-5.5-high" {
		t.Fatalf("small alias display_name should be rewritten to alias id, got %v", got)
	}
	if got := model["owned_by"]; got != "cpapi-plus" {
		t.Fatalf("small alias owner should be spoofed to preserve Cursor compaction, got %v", got)
	}
}

func TestNormalizeOpenAIModelMetadataKeepsLargeAliasesInOpenAIFamily(t *testing.T) {
	model := map[string]any{
		"id":             "gpt-5.4-1m-high",
		"version":        "gpt-5.4",
		"display_name":   "gpt-5.4",
		"owned_by":       "openai",
		"context_window": 1000000,
	}

	normalizeOpenAIModelMetadata(model)

	if got := model["version"]; got != "gpt-5.4-1m-high" {
		t.Fatalf("large alias version should still be rewritten to alias id, got %v", got)
	}
	if got := model["owned_by"]; got != "openai" {
		t.Fatalf("large aliases should stay in OpenAI family, got %v", got)
	}
}

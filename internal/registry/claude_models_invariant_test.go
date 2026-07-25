package registry

import (
	"regexp"
	"testing"
	"time"
)

// claudeModelIDPattern matches the Claude model IDs we ship in models.json:
// a "claude-" prefix followed by family/version segments, optionally a trailing
// YYYYMMDD date snapshot. Examples: claude-opus-4-8, claude-sonnet-4-5-20250929,
// claude-haiku-4-5-20251001. This is intentionally strict so a typo'd or
// foreign ID accidentally placed in the Claude section fails loudly.
var claudeModelIDPattern = regexp.MustCompile(`^claude-[a-z0-9]+(-[a-z0-9]+)*$`)

// allowedClaudeThinkingLevels is the closed set of discrete reasoning effort
// levels the Claude pipeline understands. A level outside this set means a
// suffix alias would route to an effort the executor can't map.
var allowedClaudeThinkingLevels = map[string]struct{}{
	"low":    {},
	"medium": {},
	"high":   {},
	"xhigh":  {},
	"max":    {},
}

// TestClaudeModelsJSONInvariants validates the hand-authored Claude entries in
// the embedded models.json. These rows are edited by hand (e.g. when a new Opus
// ships), and a single bad field — an ID typo, max_completion_tokens above the
// context window, an unknown thinking level, a duplicate ID, or a nonsensical
// created timestamp — would silently become runtime routing behavior. This test
// is the guard the model registry otherwise lacks.
func TestClaudeModelsJSONInvariants(t *testing.T) {
	models := GetClaudeModels()
	if len(models) == 0 {
		t.Fatal("GetClaudeModels() returned no models; embedded models.json claude section missing or unparsed")
	}

	// Reasonable absolute bounds for the created timestamp (unix seconds):
	// not before 2023-01-01 (Claude models postdate this) and not absurdly far
	// in the future. Hand-authored future entries (e.g. unreleased Opus) are
	// allowed, but a created value past this ceiling signals a units/typo bug
	// (e.g. milliseconds pasted into a seconds field).
	minCreated := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	maxCreated := time.Now().AddDate(5, 0, 0).Unix()

	seen := make(map[string]int, len(models))
	for i, model := range models {
		if model == nil {
			t.Fatalf("claude[%d] is nil", i)
		}
		id := model.ID
		if id == "" {
			t.Fatalf("claude[%d] has empty id", i)
		}

		// ID pattern.
		if !claudeModelIDPattern.MatchString(id) {
			t.Errorf("claude[%d] id %q does not match Claude ID pattern %s", i, id, claudeModelIDPattern.String())
		}

		// Duplicate / overlap detection.
		if prev, exists := seen[id]; exists {
			t.Errorf("duplicate Claude model id %q at indices %d and %d", id, prev, i)
		}
		seen[id] = i

		// Type tag must identify the section correctly.
		if model.Type != "" && model.Type != "claude" {
			t.Errorf("claude[%d] %q has type=%q, want \"claude\"", i, id, model.Type)
		}

		// Limit sanity: max completion must not exceed the context window.
		if model.ContextLength > 0 && model.MaxCompletionTokens > 0 &&
			model.MaxCompletionTokens > model.ContextLength {
			t.Errorf("claude[%d] %q has max_completion_tokens=%d > context_length=%d",
				i, id, model.MaxCompletionTokens, model.ContextLength)
		}

		// Timestamp sanity.
		if model.Created != 0 && (model.Created < minCreated || model.Created > maxCreated) {
			t.Errorf("claude[%d] %q has created=%d outside sane range [%d, %d]",
				i, id, model.Created, minCreated, maxCreated)
		}

		// Thinking levels: every declared level must be one the pipeline maps.
		if model.Thinking != nil {
			for _, level := range model.Thinking.Levels {
				if _, ok := allowedClaudeThinkingLevels[level]; !ok {
					t.Errorf("claude[%d] %q declares unknown thinking level %q", i, id, level)
				}
			}
			// Budget bounds, when used, must be coherent.
			if model.Thinking.Min < 0 {
				t.Errorf("claude[%d] %q thinking.min=%d is negative", i, id, model.Thinking.Min)
			}
			if model.Thinking.Max > 0 && model.Thinking.Min > model.Thinking.Max {
				t.Errorf("claude[%d] %q thinking.min=%d > thinking.max=%d",
					i, id, model.Thinking.Min, model.Thinking.Max)
			}
			if model.ContextLength > 0 && model.Thinking.Max > model.ContextLength {
				t.Errorf("claude[%d] %q thinking.max=%d > context_length=%d",
					i, id, model.Thinking.Max, model.ContextLength)
			}
		}
	}
}

func TestClaudeOpus5CatalogContract(t *testing.T) {
	var opus5 *ModelInfo
	for _, model := range GetClaudeModels() {
		if model != nil && model.ID == "claude-opus-5" {
			opus5 = model
			break
		}
	}
	if opus5 == nil {
		t.Fatal("claude-opus-5 missing from embedded Claude model catalog")
	}
	if opus5.ContextLength != 1_000_000 {
		t.Fatalf("claude-opus-5 context_length = %d, want 1000000", opus5.ContextLength)
	}
	if opus5.MaxCompletionTokens != 64_000 {
		t.Fatalf("claude-opus-5 max_completion_tokens = %d, want captured 64000", opus5.MaxCompletionTokens)
	}
	if opus5.Thinking == nil {
		t.Fatal("claude-opus-5 thinking metadata is nil")
	}
	if opus5.Thinking.Min != 1_024 || opus5.Thinking.Max != 64_000 || !opus5.Thinking.ZeroAllowed {
		t.Fatalf("claude-opus-5 thinking metadata = %+v, want min=1024 max=64000 zero_allowed=true", opus5.Thinking)
	}
	wantLevels := []string{"low", "medium", "high", "xhigh", "max"}
	if len(opus5.Thinking.Levels) != len(wantLevels) {
		t.Fatalf("claude-opus-5 thinking levels = %v, want %v", opus5.Thinking.Levels, wantLevels)
	}
	for i, want := range wantLevels {
		if opus5.Thinking.Levels[i] != want {
			t.Fatalf("claude-opus-5 thinking level[%d] = %q, want %q", i, opus5.Thinking.Levels[i], want)
		}
	}
}

func TestIsAdaptiveClaudeOpusModel(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4-7",
		"claude-opus-4-7-thinking-low",
		"claude-opus-4-8",
		"claude-opus-4-8-thinking-max",
		"claude-opus-5",
		"claude-opus-5-thinking-high",
	} {
		if !IsAdaptiveClaudeOpusModel(model) {
			t.Errorf("IsAdaptiveClaudeOpusModel(%q) = false, want true", model)
		}
	}
	for _, model := range []string{
		"claude-opus-50",
		"claude-opus-5-preview",
		"claude-sonnet-5",
		"mythos",
	} {
		if IsAdaptiveClaudeOpusModel(model) {
			t.Errorf("IsAdaptiveClaudeOpusModel(%q) = true, want false", model)
		}
	}
}

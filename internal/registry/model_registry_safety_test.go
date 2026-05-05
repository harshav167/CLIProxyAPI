package registry

import (
	"testing"
	"time"
)

func TestGetModelInfoReturnsClone(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Min: 1, Max: 2, Levels: []string{"low", "high"}},
	}})

	first := r.GetModelInfo("m1", "gemini")
	if first == nil {
		t.Fatal("expected model info")
	}
	first.DisplayName = "mutated"
	first.Thinking.Levels[0] = "mutated"

	second := r.GetModelInfo("m1", "gemini")
	if second.DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second.DisplayName)
	}
	if second.Thinking == nil || len(second.Thinking.Levels) == 0 || second.Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second.Thinking)
	}
}

func TestGetModelsForClientReturnsClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := r.GetModelsForClient("client-1")
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("expected one model, got %+v", first)
	}
	first[0].DisplayName = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := r.GetModelsForClient("client-1")
	if len(second) != 1 || second[0] == nil {
		t.Fatalf("expected one model on second fetch, got %+v", second)
	}
	if second[0].DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second[0].DisplayName)
	}
	if second[0].Thinking == nil || len(second[0].Thinking.Levels) == 0 || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second[0].Thinking)
	}
}

func TestGetAvailableModelsByProviderReturnsClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := r.GetAvailableModelsByProvider("gemini")
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("expected one model, got %+v", first)
	}
	first[0].DisplayName = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := r.GetAvailableModelsByProvider("gemini")
	if len(second) != 1 || second[0] == nil {
		t.Fatalf("expected one model on second fetch, got %+v", second)
	}
	if second[0].DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second[0].DisplayName)
	}
	if second[0].Thinking == nil || len(second[0].Thinking.Levels) == 0 || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second[0].Thinking)
	}
}

func TestCleanupExpiredQuotasInvalidatesAvailableModelsCache(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{ID: "m1", Created: 1}})
	r.SetModelQuotaExceeded("client-1", "m1")
	if models := r.GetAvailableModels("openai"); len(models) != 1 {
		t.Fatalf("expected cooldown model to remain listed before cleanup, got %d", len(models))
	}

	r.mutex.Lock()
	quotaTime := time.Now().Add(-6 * time.Minute)
	r.models["m1"].QuotaExceededClients["client-1"] = &quotaTime
	r.mutex.Unlock()

	r.CleanupExpiredQuotas()

	if count := r.GetModelCount("m1"); count != 1 {
		t.Fatalf("expected model count 1 after cleanup, got %d", count)
	}
	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to stay available after cleanup, got %d", len(models))
	}
	if got := models[0]["id"]; got != "m1" {
		t.Fatalf("expected model id m1, got %v", got)
	}
}

func TestGetAvailableModelsReturnsClonedSupportedParameters(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{
		ID:                  "m1",
		DisplayName:         "Model One",
		SupportedParameters: []string{"temperature", "top_p"},
	}})

	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatalf("expected one model, got %d", len(first))
	}
	params, ok := first[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 {
		t.Fatalf("expected supported_parameters slice, got %#v", first[0]["supported_parameters"])
	}
	params[0] = "mutated"

	second := r.GetAvailableModels("openai")
	params, ok = second[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 || params[0] != "temperature" {
		t.Fatalf("expected cloned supported_parameters, got %#v", second[0]["supported_parameters"])
	}
}

func TestGetAvailableModelsIncludesThinkingMetadataForOpenAIAndClaudeCompatibleHandlers(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-openai", "openai", []*ModelInfo{{
		ID:                  "gpt-test",
		DisplayName:         "GPT Test",
		ContextLength:       200000,
		MaxCompletionTokens: 32000,
		Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
	}})
	r.RegisterClient("client-antigravity", "antigravity", []*ModelInfo{{
		ID:                  "ag-test",
		DisplayName:         "AG Test",
		ContextLength:       128000,
		MaxCompletionTokens: 16000,
		Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
	}})

	openAIModels := r.GetAvailableModels("openai")
	if len(openAIModels) != 2 {
		t.Fatalf("expected two openai-visible models, got %d", len(openAIModels))
	}

	var openAIModel map[string]any
	for _, model := range openAIModels {
		if id, _ := model["id"].(string); id == "gpt-test" {
			openAIModel = model
			break
		}
	}
	if openAIModel == nil {
		t.Fatal("expected openai model in registry output")
	}
	if levels, ok := openAIModel["reasoning_effort_levels"].([]string); !ok || len(levels) != 3 || levels[0] != "low" {
		t.Fatalf("expected reasoning_effort_levels in openai model, got %#v", openAIModel["reasoning_effort_levels"])
	}
	thinkingMap, ok := openAIModel["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking map in openai model, got %#v", openAIModel["thinking"])
	}
	thinkingLevels, ok := thinkingMap["levels"].([]string)
	if !ok || len(thinkingLevels) != 3 || thinkingLevels[2] != "high" {
		t.Fatalf("expected thinking levels in openai model, got %#v", thinkingMap["levels"])
	}

	claudeModels := r.GetAvailableModels("antigravity")
	if len(claudeModels) != 2 {
		t.Fatalf("expected two claude-compatible models, got %d", len(claudeModels))
	}

	var antigravityModel map[string]any
	for _, model := range claudeModels {
		if id, _ := model["id"].(string); id == "ag-test" {
			antigravityModel = model
			break
		}
	}
	if antigravityModel == nil {
		t.Fatal("expected antigravity model in registry output")
	}
	if thinking, ok := antigravityModel["thinking"].(bool); !ok || !thinking {
		t.Fatalf("expected thinking=true in claude-compatible model, got %#v", antigravityModel["thinking"])
	}
	extended, ok := antigravityModel["extended_thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected extended_thinking map, got %#v", antigravityModel["extended_thinking"])
	}
	if got, ok := extended["supported"].(bool); !ok || !got {
		t.Fatalf("expected extended_thinking.supported=true, got %#v", extended["supported"])
	}
	if got, ok := antigravityModel["context_length"].(int); !ok || got != 128000 {
		t.Fatalf("expected context_length=128000, got %#v", antigravityModel["context_length"])
	}
	if got, ok := antigravityModel["max_completion_tokens"].(int); !ok || got != 16000 {
		t.Fatalf("expected max_completion_tokens=16000, got %#v", antigravityModel["max_completion_tokens"])
	}
}

func TestLookupModelInfoReturnsCloneForStaticDefinitions(t *testing.T) {
	first := LookupModelInfo("claude-sonnet-4-6")
	if first == nil || first.Thinking == nil || len(first.Thinking.Levels) == 0 {
		t.Fatalf("expected static model with thinking levels, got %+v", first)
	}
	first.Thinking.Levels[0] = "mutated"

	second := LookupModelInfo("claude-sonnet-4-6")
	if second == nil || second.Thinking == nil || len(second.Thinking.Levels) == 0 || second.Thinking.Levels[0] == "mutated" {
		t.Fatalf("expected static lookup clone, got %+v", second)
	}
}

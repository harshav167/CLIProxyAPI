// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

const (
	codexBuiltinImageModelID      = "gpt-image-2"
	xaiBuiltinImageModelID        = "grok-imagine-image"
	xaiBuiltinImageQualityModelID = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID        = "grok-imagine-video"
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	GeminiCLI   []*ModelInfo `json:"gemini-cli"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
	XAI         []*ModelInfo `json:"xai"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetGeminiModels returns the standard Gemini model definitions.
func GetGeminiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Gemini)
}

// GetGeminiVertexModels returns Gemini model definitions for Vertex AI.
func GetGeminiVertexModels() []*ModelInfo {
	return cloneModelInfos(getModels().Vertex)
}

// GetGeminiCLIModels returns Gemini model definitions for the Gemini CLI.
func GetGeminiCLIModels() []*ModelInfo {
	return cloneModelInfos(getModels().GeminiCLI)
}

// GetAIStudioModels returns model definitions for AI Studio.
func GetAIStudioModels() []*ModelInfo {
	return cloneModelInfos(getModels().AIStudio)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexFree))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexTeam))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPlus))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPro))
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetAntigravityModels returns the standard Antigravity model definitions.
func GetAntigravityModels() []*ModelInfo {
	return cloneModelInfos(getModels().Antigravity)
}

// GetXAIModels returns the standard xAI Grok model definitions.
func GetXAIModels() []*ModelInfo {
	return WithXAIBuiltins(cloneModelInfos(getModels().XAI))
}

// WithCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates. Built-ins replace any matching IDs
// already present in the provided slice.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, codexBuiltinImageModelInfo())
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo())
}

func codexBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImageModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2",
		Version:     codexBuiltinImageModelID,
	}
}

func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetCodeBuddyModels returns nil; the codebuddy executor maintains its own
// model list. Stub exists to satisfy registry channel and lookup call sites
// from the f88a5eae migration commit.
func GetCodeBuddyModels() []*ModelInfo { return nil }

// GetGitHubCopilotModels returns nil; same reason. The github_copilot
// executor at internal/runtime/executor/github_copilot_executor.go owns
// the real model list.
func GetGitHubCopilotModels() []*ModelInfo { return nil }

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - gemini-cli
//   - aistudio
//   - codex
//   - kimi
//   - antigravity
//   - codebuddy
//   - github-copilot
//   - cursor
//   - xai
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
		return GetGeminiModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "gemini-cli":
		return GetGeminiCLIModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return GetCodexProModels()
	case "kimi":
		return GetKimiModels()
	case "antigravity":
		return GetAntigravityModels()
	case "codebuddy":
		return GetCodeBuddyModels()
	case "cursor":
		return GetCursorModels()
	case "github-copilot":
		return GetGitHubCopilotModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	default:
		return nil
	}
}

// GetCursorModels returns the fallback Cursor model definitions.
func GetCursorModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "composer-2", Object: "model", OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2", ContextLength: 200000, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Max: 50000, DynamicAllowed: true}},
		{ID: "claude-4-sonnet", Object: "model", OwnedBy: "cursor", Type: "cursor", DisplayName: "Claude 4 Sonnet", ContextLength: 200000, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Max: 50000, DynamicAllowed: true}},
		{ID: "claude-3.5-sonnet", Object: "model", OwnedBy: "cursor", Type: "cursor", DisplayName: "Claude 3.5 Sonnet", ContextLength: 200000, MaxCompletionTokens: 8192},
		{ID: "gpt-4o", Object: "model", OwnedBy: "cursor", Type: "cursor", DisplayName: "GPT-4o", ContextLength: 128000, MaxCompletionTokens: 16384},
		{ID: "cursor-small", Object: "model", OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Small", ContextLength: 200000, MaxCompletionTokens: 64000},
		{ID: "gemini-2.5-pro", Object: "model", OwnedBy: "cursor", Type: "cursor", DisplayName: "Gemini 2.5 Pro", ContextLength: 1000000, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Max: 50000, DynamicAllowed: true}},
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	data := getModels()
	allModels := [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.GeminiCLI,
		data.AIStudio,
		data.CodexPro,
		data.Kimi,
		data.Antigravity,
		data.XAI,
		GetCodeBuddyModels(),
		GetCursorModels(),
		GetGitHubCopilotModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	return nil
}

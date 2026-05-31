package openai

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

// shouldTreatAsResponsesFormat detects OpenAI Responses-style payloads that are
// accidentally sent to the Chat Completions endpoint.
func shouldTreatAsResponsesFormat(rawJSON []byte) bool {
	if gjson.GetBytes(rawJSON, "messages").Exists() {
		return false
	}
	if gjson.GetBytes(rawJSON, "input").Exists() {
		return true
	}
	if gjson.GetBytes(rawJSON, "instructions").Exists() {
		return true
	}
	return false
}

// shouldRouteResponsesBodyViaCodexResponses detects clients that reached the
// Chat Completions route but are already speaking the Responses request dialect
// for a Codex/OpenAI Responses-backed model. Keeping this branch in the chat
// handler preserves the existing prompt-cache injection and route guardrails,
// while avoiding a lossy Responses -> Chat -> Responses request conversion.
func shouldRouteResponsesBodyViaCodexResponses(modelName string, rawJSON []byte) bool {
	return shouldTreatAsResponsesFormat(rawJSON) && hasModelProvider(modelName, "codex")
}

// hasModelProvider reports whether modelName routes to any of the given
// provider names (the variadic replacement for the older isCodexBoundModel
// and isPromptCacheBoundModel predicates).
func hasModelProvider(modelName string, providers ...string) bool {
	if modelName == "" || len(providers) == 0 {
		return false
	}
	// Strip any thinking suffix (e.g. "gpt-5-codex:thinking-high") before the
	// registry lookup. GetProviderName matches the registered base ID exactly;
	// a suffixed name would not match, provider detection would fail, and the
	// caller would bypass Codex Responses routing + prompt-cache handling.
	// Every other call site already routes through thinking.ParseSuffix.
	baseModel := thinking.ParseSuffix(modelName).ModelName
	if baseModel == "" {
		baseModel = modelName
	}
	for _, p := range util.GetProviderName(baseModel) {
		for _, want := range providers {
			if p == want {
				return true
			}
		}
	}
	return false
}

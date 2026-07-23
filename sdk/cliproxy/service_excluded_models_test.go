package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestRegisterModelsForAuth_UsesPreMergedExcludedModelsAttribute(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"gemini": {"gemini-2.5-pro"},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-gemini",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "gemini-2.5-flash",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetAvailableModelsByProvider("gemini")
	if len(models) == 0 {
		t.Fatal("expected gemini models to be registered")
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if strings.EqualFold(modelID, "gemini-2.5-flash") {
			t.Fatalf("expected model %q to be excluded by auth attribute", modelID)
		}
	}

	seenGlobalExcluded := false
	for _, model := range models {
		if model == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.ID), "gemini-2.5-pro") {
			seenGlobalExcluded = true
			break
		}
	}
	if !seenGlobalExcluded {
		t.Fatal("expected global excluded model to be present when attribute override is set")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityImageModelType(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "images",
					BaseURL: "https://example.com/v1",
					Models: []config.OpenAICompatibilityModel{
						{Name: "upstream-image", Alias: "compat-image", Image: true},
						{Name: "upstream-chat", Alias: "compat-chat"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-image",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"compat_name":  "images",
			"provider_key": "images",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	var imageModel *internalregistry.ModelInfo
	var chatModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "compat-image":
			imageModel = model
		case "compat-chat":
			chatModel = model
		}
	}
	if imageModel == nil {
		t.Fatal("expected compat-image to be registered")
	}
	if imageModel.Type != internalregistry.OpenAIImageModelType {
		t.Fatalf("image model type = %q, want %q", imageModel.Type, internalregistry.OpenAIImageModelType)
	}
	if imageModel.Thinking != nil {
		t.Fatalf("image model thinking = %+v, want nil", imageModel.Thinking)
	}
	if chatModel == nil {
		t.Fatal("expected compat-chat to be registered")
	}
	if chatModel.Type != "openai-compatibility" {
		t.Fatalf("chat model type = %q, want openai-compatibility", chatModel.Type)
	}
	if chatModel.Thinking == nil {
		t.Fatal("expected chat model to keep default thinking support")
	}
}

func TestRegisterModelsForAuth_AntigravityFetchesWebSearchCapability(t *testing.T) {
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityModelsPath {
			t.Fatalf("path = %q, want %s", r.URL.Path, antigravityModelsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		requestBody, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		if got := gjson.GetBytes(requestBody, "project").String(); got != "test-project" {
			t.Fatalf("project = %q, want test-project", got)
		}
		sawFetch = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
				"models": {
					"gemini-3.1-flash-lite": {
						"displayName": "Gemini 3.1 Flash Lite",
						"maxTokens": 1,
						"maxOutputTokens": 2
					},
					"fetched-only-search-model": {
						"displayName": "Fetched Only Search Model"
					},
					"gemini-3.6-flash-tiered": {
						"displayName": "Gemini 3.6 Flash",
						"supportsImages": true,
						"supportsThinking": true,
						"thinkingBudget": -1,
						"minThinkingBudget": 32,
						"maxTokens": 1048576,
						"maxOutputTokens": 65536,
						"supportsVideo": true,
						"supportedMimeTypes": {
							"image/png": true,
							"video/mp4": true,
							"audio/webm;codecs=opus": true
						}
					}
				},
				"webSearchModelIds": ["gemini-3.1-flash-lite", "fetched-only-search-model"]
			}`))
	}))
	defer server.Close()

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "auth-antigravity-fetch-models",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "test-project",
		},
	}

	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)
	if !sawFetch {
		t.Fatal("expected fetchAvailableModels request")
	}

	models := registry.GetModelsForClient(auth.ID)
	staticModels := internalregistry.GetAntigravityModels()
	staticByID := make(map[string]*internalregistry.ModelInfo, len(staticModels))
	for _, model := range staticModels {
		if model != nil {
			staticByID[model.ID] = model
		}
	}

	var webSearchModel, agentModel, staticOnlyModel, tieredModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "gemini-3.1-flash-lite":
			webSearchModel = model
		case "gemini-3-flash-agent":
			agentModel = model
		case "gpt-oss-120b-medium":
			staticOnlyModel = model
		case "gemini-3.6-flash-tiered":
			tieredModel = model
		}
	}
	if webSearchModel == nil {
		t.Fatal("expected gemini-3.1-flash-lite to be registered")
	}
	if !webSearchModel.SupportsWebSearch {
		t.Fatal("expected gemini-3.1-flash-lite to support web search")
	}
	staticWebSearchModel := staticByID["gemini-3.1-flash-lite"]
	if staticWebSearchModel == nil {
		t.Fatal("expected static gemini-3.1-flash-lite definition")
	}
	if webSearchModel.ContextLength != staticWebSearchModel.ContextLength || webSearchModel.MaxCompletionTokens != staticWebSearchModel.MaxCompletionTokens {
		t.Fatalf("static token limits should be preserved, got=%#v static=%#v", webSearchModel, staticWebSearchModel)
	}
	if agentModel == nil {
		t.Fatal("expected gemini-3-flash-agent to be registered")
	}
	if agentModel.SupportsWebSearch {
		t.Fatal("gemini-3-flash-agent should not support web search")
	}
	if staticOnlyModel == nil {
		t.Fatal("expected static-only Antigravity model to remain registered")
	}
	for _, model := range models {
		if model != nil && model.ID == "fetched-only-search-model" {
			t.Fatalf("fetched-only internal model should not be registered: %#v", model)
		}
	}
	if tieredModel == nil {
		t.Fatal("expected gemini-3.6-flash-tiered to be registered")
	}
	if tieredModel.DisplayName != "Gemini 3.6 Flash" {
		t.Fatalf("tiered model display name = %q, want Gemini 3.6 Flash", tieredModel.DisplayName)
	}
	if tieredModel.ContextLength != 1_048_576 || tieredModel.MaxCompletionTokens != 65_536 {
		t.Fatalf("tiered token limits = %d/%d, want 1048576/65536", tieredModel.ContextLength, tieredModel.MaxCompletionTokens)
	}
	if tieredModel.Thinking == nil || tieredModel.Thinking.Min != 32 || !tieredModel.Thinking.DynamicAllowed {
		t.Fatalf("tiered thinking metadata = %+v, want min 32 and dynamic", tieredModel.Thinking)
	}
	if got := strings.Join(tieredModel.SupportedInputModalities, ","); got != "TEXT,IMAGE,VIDEO" {
		t.Fatalf("tiered input modalities = %q, want TEXT,IMAGE,VIDEO", got)
	}
	if got := strings.Join(tieredModel.SupportedOutputModalities, ","); got != "TEXT" {
		t.Fatalf("tiered output modalities = %q, want TEXT", got)
	}
}

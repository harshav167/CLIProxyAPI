package cliproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityModelBaseURLDaily = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelBaseURLProd  = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath        = "/v1internal:fetchAvailableModels"
)

type antigravityFetchAvailableModelsResponse struct {
	WebSearchModelIDs []string                           `json:"webSearchModelIds"`
	Models            map[string]antigravityFetchedModel `json:"models"`
}

type antigravityFetchedModel struct {
	DisplayName        string          `json:"displayName"`
	SupportsImages     bool            `json:"supportsImages"`
	SupportsThinking   bool            `json:"supportsThinking"`
	ThinkingBudget     int             `json:"thinkingBudget"`
	MinThinkingBudget  int             `json:"minThinkingBudget"`
	MaxTokens          int             `json:"maxTokens"`
	MaxOutputTokens    int             `json:"maxOutputTokens"`
	SupportsVideo      bool            `json:"supportsVideo"`
	SupportedMIMETypes map[string]bool `json:"supportedMimeTypes"`
}

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
	Models            map[string]*ModelInfo
}

func (s *Service) fetchAntigravityModelCapabilityHintsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityModelCapabilityHints {
	if auth == nil || auth.Metadata == nil {
		return antigravityModelCapabilityHints{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityModelCapabilityHints{}
	}

	client := &http.Client{}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth)); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	requestBody := antigravityModelsRequestBody(auth)
	for _, baseURL := range antigravityModelBaseURLs(auth) {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(requestBody))
		if errReq != nil {
			continue
		}
		req.Close = true
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", misc.AntigravityUserAgent())

		resp, errDo := client.Do(req)
		if errDo != nil {
			continue
		}
		body, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
		if errRead != nil {
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			continue
		}
		hints := parseAntigravityModelCapabilityHints(body)
		if len(hints.WebSearchModelIDs) > 0 || len(hints.Models) > 0 {
			return hints
		}
	}
	return antigravityModelCapabilityHints{}
}

func antigravityModelsRequestBody(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return `{}`
	}
	projectID, _ := auth.Metadata["project_id"].(string)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return `{}`
	}
	body, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return `{}`
	}
	return string(body)
}

func (s *Service) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if s != nil && s.cfg != nil {
		return strings.TrimSpace(s.cfg.ProxyURL)
	}
	return ""
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily, antigravityModelBaseURLProd}
}

func resolveAntigravityModelBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["base_url"].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return ""
}

func parseAntigravityModelCapabilityHints(body []byte) antigravityModelCapabilityHints {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}
	models := make(map[string]*ModelInfo, len(parsed.Models))
	for modelID, fetched := range parsed.Models {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID == "" {
			continue
		}
		models[modelID] = antigravityFetchedModelInfo(modelID, fetched)
	}
	return antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels, Models: models}
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		if _, ok := hints.WebSearchModelIDs[modelID]; ok {
			model.SupportsWebSearch = true
		}
		if fetched := hints.Models[modelID]; fetched != nil {
			mergeAntigravityFetchedModel(model, fetched)
		}
	}
	return models
}

func antigravityFetchedModelInfo(modelID string, fetched antigravityFetchedModel) *ModelInfo {
	model := &ModelInfo{
		ID:                        modelID,
		Object:                    "model",
		OwnedBy:                   "antigravity",
		Type:                      "antigravity",
		DisplayName:               strings.TrimSpace(fetched.DisplayName),
		Name:                      modelID,
		Description:               strings.TrimSpace(fetched.DisplayName),
		ContextLength:             fetched.MaxTokens,
		MaxCompletionTokens:       fetched.MaxOutputTokens,
		SupportedInputModalities:  []string{"TEXT"},
		SupportedOutputModalities: []string{"TEXT"},
	}
	if fetched.SupportsImages || antigravitySupportsMIMEPrefix(fetched.SupportedMIMETypes, "image/") {
		model.SupportedInputModalities = append(model.SupportedInputModalities, "IMAGE")
	}
	if fetched.SupportsVideo || antigravitySupportsMIMEPrefix(fetched.SupportedMIMETypes, "video/") {
		model.SupportedInputModalities = append(model.SupportedInputModalities, "VIDEO")
	}
	if fetched.SupportsThinking {
		model.Thinking = &registry.ThinkingSupport{
			Min:            fetched.MinThinkingBudget,
			Max:            fetched.ThinkingBudget,
			DynamicAllowed: fetched.ThinkingBudget == -1,
		}
		if model.Thinking.DynamicAllowed {
			model.Thinking.Max = fetched.MaxOutputTokens
		}
	}
	return model
}

func mergeAntigravityFetchedModel(model, fetched *ModelInfo) {
	if fetched.DisplayName != "" {
		model.DisplayName = fetched.DisplayName
		model.Description = fetched.Description
	}
	if model.ContextLength == 0 && fetched.ContextLength > 0 {
		model.ContextLength = fetched.ContextLength
	}
	if model.MaxCompletionTokens == 0 && fetched.MaxCompletionTokens > 0 {
		model.MaxCompletionTokens = fetched.MaxCompletionTokens
	}
	if len(fetched.SupportedInputModalities) > 1 {
		model.SupportedInputModalities = append([]string(nil), fetched.SupportedInputModalities...)
	}
	if len(fetched.SupportedOutputModalities) > 0 {
		model.SupportedOutputModalities = append([]string(nil), fetched.SupportedOutputModalities...)
	}
	if fetched.Thinking != nil && (model.Thinking == nil || len(model.Thinking.Levels) == 0) {
		thinking := *fetched.Thinking
		thinking.Levels = append([]string(nil), fetched.Thinking.Levels...)
		model.Thinking = &thinking
	}
}

func antigravitySupportsMIMEPrefix(mimeTypes map[string]bool, prefix string) bool {
	for mimeType, supported := range mimeTypes {
		if supported && strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), prefix) {
			return true
		}
	}
	return false
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

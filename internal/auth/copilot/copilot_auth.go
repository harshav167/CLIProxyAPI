package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	copilotAPITokenURL = "https://api.github.com/copilot_internal/v2/token"
	copilotAPIEndpoint = "https://api.githubcopilot.com"

	copilotUserAgent     = "GithubCopilot/1.0"
	copilotEditorVersion = "vscode/1.100.0"
	copilotPluginVersion = "copilot/1.300.0"
	copilotIntegrationID = "vscode-chat"
	copilotOpenAIIntent  = "conversation-panel"
)

// CopilotAPIToken represents the Copilot API token response.
type CopilotAPIToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Endpoints struct {
		API           string `json:"api"`
		Proxy         string `json:"proxy"`
		OriginTracker string `json:"origin-tracker"`
		Telemetry     string `json:"telemetry"`
	} `json:"endpoints,omitempty"`
	ErrorDetails *struct {
		URL              string `json:"url"`
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	} `json:"error_details,omitempty"`
}

// CopilotAuth handles GitHub Copilot authentication flow.
type CopilotAuth struct {
	httpClient   *http.Client
	deviceClient *DeviceFlowClient
	cfg          *config.Config
}

func NewCopilotAuth(cfg *config.Config) *CopilotAuth {
	return &CopilotAuth{
		httpClient:   util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second}),
		deviceClient: NewDeviceFlowClient(cfg),
		cfg:          cfg,
	}
}

func (c *CopilotAuth) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	return c.deviceClient.RequestDeviceCode(ctx)
}

func (c *CopilotAuth) WaitForAuthorization(ctx context.Context, deviceCode *DeviceCodeResponse) (*CopilotAuthBundle, error) {
	tokenData, err := c.deviceClient.PollForToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	userInfo, err := c.deviceClient.FetchUserInfo(ctx, tokenData.AccessToken)
	if err != nil {
		log.Warnf("copilot: failed to fetch user info: %v", err)
	}

	username := userInfo.Login
	if username == "" {
		username = "github-user"
	}

	return &CopilotAuthBundle{
		TokenData: tokenData,
		Username:  username,
		Email:     userInfo.Email,
		Name:      userInfo.Name,
	}, nil
}

func (c *CopilotAuth) GetCopilotAPIToken(ctx context.Context, githubAccessToken string) (*CopilotAPIToken, error) {
	if githubAccessToken == "" {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed, fmt.Errorf("github access token is empty"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotAPITokenURL, nil)
	if err != nil {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed, err)
	}

	req.Header.Set("Authorization", "token "+githubAccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed, err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("copilot api token: close body error: %v", errClose)
		}
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed, err)
	}

	if !isHTTPSuccess(resp.StatusCode) {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed,
			fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes)))
	}

	var apiToken CopilotAPIToken
	if err = json.Unmarshal(bodyBytes, &apiToken); err != nil {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed, err)
	}

	if apiToken.Token == "" {
		return nil, NewAuthenticationError(ErrTokenExchangeFailed, fmt.Errorf("empty copilot api token"))
	}

	return &apiToken, nil
}

func (c *CopilotAuth) ValidateToken(ctx context.Context, accessToken string) (bool, string, error) {
	if accessToken == "" {
		return false, "", nil
	}

	userInfo, err := c.deviceClient.FetchUserInfo(ctx, accessToken)
	if err != nil {
		return false, "", err
	}

	return true, userInfo.Login, nil
}

func (c *CopilotAuth) CreateTokenStorage(bundle *CopilotAuthBundle) *CopilotTokenStorage {
	return &CopilotTokenStorage{
		AccessToken: bundle.TokenData.AccessToken,
		TokenType:   bundle.TokenData.TokenType,
		Scope:       bundle.TokenData.Scope,
		Username:    bundle.Username,
		Email:       bundle.Email,
		Name:        bundle.Name,
		Type:        "github-copilot",
	}
}

func (c *CopilotAuth) LoadAndValidateToken(ctx context.Context, storage *CopilotTokenStorage) (bool, error) {
	if storage == nil || storage.AccessToken == "" {
		return false, fmt.Errorf("no token available")
	}

	apiToken, err := c.GetCopilotAPIToken(ctx, storage.AccessToken)
	if err != nil {
		return false, err
	}

	if apiToken.ExpiresAt > 0 && time.Now().Unix() >= apiToken.ExpiresAt {
		return false, fmt.Errorf("copilot api token expired")
	}

	return true, nil
}

func (c *CopilotAuth) GetAPIEndpoint() string {
	return copilotAPIEndpoint
}

func (c *CopilotAuth) MakeAuthenticatedRequest(ctx context.Context, method, url string, body io.Reader, apiToken *CopilotAPIToken) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiToken.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("Openai-Intent", copilotOpenAIIntent)
	req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)

	return req, nil
}

// CopilotModelEntry represents a single model entry returned by the Copilot /models API.
type CopilotModelEntry struct {
	ID           string         `json:"id"`
	Object       string         `json:"object"`
	Created      int64          `json:"created"`
	OwnedBy      string         `json:"owned_by"`
	Name         string         `json:"name,omitempty"`
	Version      string         `json:"version,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

// CopilotModelLimits holds the token limits returned by the Copilot /models API.
type CopilotModelLimits struct {
	MaxContextWindowTokens int
	MaxPromptTokens        int
	MaxOutputTokens        int
}

func (e *CopilotModelEntry) Limits() *CopilotModelLimits {
	if e.Capabilities == nil {
		return nil
	}
	limitsRaw, ok := e.Capabilities["limits"]
	if !ok {
		return nil
	}
	limitsMap, ok := limitsRaw.(map[string]any)
	if !ok {
		return nil
	}

	result := &CopilotModelLimits{
		MaxContextWindowTokens: anyToInt(limitsMap["max_context_window_tokens"]),
		MaxPromptTokens:        anyToInt(limitsMap["max_prompt_tokens"]),
		MaxOutputTokens:        anyToInt(limitsMap["max_output_tokens"]),
	}

	if result.MaxContextWindowTokens == 0 && result.MaxPromptTokens == 0 && result.MaxOutputTokens == 0 {
		return nil
	}
	return result
}

func anyToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

type CopilotModelsResponse struct {
	Data   []CopilotModelEntry `json:"data"`
	Object string              `json:"object"`
}

const maxModelsResponseSize = 2 * 1024 * 1024

var allowedCopilotAPIHosts = map[string]bool{
	"api.githubcopilot.com":               true,
	"api.individual.githubcopilot.com":    true,
	"api.business.githubcopilot.com":      true,
	"copilot-proxy.githubusercontent.com": true,
}

func (c *CopilotAuth) ListModels(ctx context.Context, apiToken *CopilotAPIToken) ([]CopilotModelEntry, error) {
	if apiToken == nil || apiToken.Token == "" {
		return nil, fmt.Errorf("copilot: api token is required for listing models")
	}

	modelsURL := copilotAPIEndpoint + "/models"
	if ep := strings.TrimRight(apiToken.Endpoints.API, "/"); ep != "" {
		parsed, err := url.Parse(ep)
		if err == nil && parsed.Scheme == "https" && allowedCopilotAPIHosts[parsed.Host] {
			modelsURL = ep + "/models"
		} else {
			log.Warnf("copilot: ignoring untrusted API endpoint %q, using default", ep)
		}
	}

	req, err := c.MakeAuthenticatedRequest(ctx, http.MethodGet, modelsURL, nil, apiToken)
	if err != nil {
		return nil, fmt.Errorf("copilot: failed to create models request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot: models request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("copilot list models: close body error: %v", errClose)
		}
	}()

	limitedReader := io.LimitReader(resp.Body, maxModelsResponseSize)
	bodyBytes, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("copilot: failed to read models response: %w", err)
	}

	if !isHTTPSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("copilot: list models failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var modelsResp CopilotModelsResponse
	if err = json.Unmarshal(bodyBytes, &modelsResp); err != nil {
		return nil, fmt.Errorf("copilot: failed to parse models response: %w", err)
	}

	return modelsResp.Data, nil
}

func (c *CopilotAuth) ListModelsWithGitHubToken(ctx context.Context, githubAccessToken string) ([]CopilotModelEntry, error) {
	apiToken, err := c.GetCopilotAPIToken(ctx, githubAccessToken)
	if err != nil {
		return nil, fmt.Errorf("copilot: failed to get API token for model listing: %w", err)
	}

	return c.ListModels(ctx, apiToken)
}

func buildChatCompletionURL() string {
	return copilotAPIEndpoint + "/chat/completions"
}

package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	copilotauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	githubCopilotBaseURL       = "https://api.githubcopilot.com"
	githubCopilotChatPath      = "/chat/completions"
	githubCopilotResponsesPath = "/responses"
	githubCopilotAuthType      = "github-copilot"
	githubCopilotTokenCacheTTL = 25 * time.Minute
	tokenExpiryBuffer          = 5 * time.Minute

	copilotUserAgent     = "GitHubCopilotChat/0.35.0"
	copilotEditorVersion = "vscode/1.107.0"
	copilotPluginVersion = "copilot-chat/0.35.0"
	copilotIntegrationID = "vscode-chat"
	copilotOpenAIIntent  = "conversation-edits"
	copilotGitHubAPIVer  = "2025-04-01"
)

type GitHubCopilotExecutor struct {
	cfg   *config.Config
	mu    sync.RWMutex
	cache map[string]*cachedAPIToken
}

type cachedAPIToken struct {
	token       string
	apiEndpoint string
	expiresAt   time.Time
}

func NewGitHubCopilotExecutor(cfg *config.Config) *GitHubCopilotExecutor {
	return &GitHubCopilotExecutor{
		cfg:   cfg,
		cache: make(map[string]*cachedAPIToken),
	}
}

func (e *GitHubCopilotExecutor) Identifier() string { return githubCopilotAuthType }

func (e *GitHubCopilotExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	apiToken, _, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return err
	}
	e.applyHeaders(req, apiToken, nil)
	return nil
}

func (e *GitHubCopilotExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("github-copilot executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *GitHubCopilotExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if githubCopilotUsesAnthropicGateway(req.Model) {
		return e.executeViaClaude(ctx, auth, req, opts)
	}
	if useGitHubCopilotResponsesEndpoint(opts.SourceFormat, req.Model) {
		return e.executeResponses(ctx, auth, req, opts)
	}
	return e.executeChat(ctx, auth, req, opts)
}

func (e *GitHubCopilotExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if githubCopilotUsesAnthropicGateway(req.Model) {
		return e.executeStreamViaClaude(ctx, auth, req, opts)
	}
	if useGitHubCopilotResponsesEndpoint(opts.SourceFormat, req.Model) {
		return e.executeResponsesStream(ctx, auth, req, opts)
	}
	return e.executeChatStream(ctx, auth, req, opts)
}

func (e *GitHubCopilotExecutor) executeViaClaude(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	nativeAuth := buildCopilotAnthropicGatewayAuth(auth, apiToken, baseURL, req.Payload)
	if nativeAuth == nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "failed to build copilot claude auth"}
	}
	nativeReq := normalizeGitHubCopilotRequestModel(req)
	return NewClaudeExecutor(e.cfg).Execute(ctx, nativeAuth, nativeReq, opts)
}

func (e *GitHubCopilotExecutor) executeStreamViaClaude(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	nativeAuth := buildCopilotAnthropicGatewayAuth(auth, apiToken, baseURL, req.Payload)
	if nativeAuth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "failed to build copilot claude auth"}
	}
	nativeReq := normalizeGitHubCopilotRequestModel(req)
	return NewClaudeExecutor(e.cfg).ExecuteStream(ctx, nativeAuth, nativeReq, opts)
}

func (e *GitHubCopilotExecutor) executeChat(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	nativeAuth := buildCopilotOpenAIGatewayAuth(auth, apiToken, baseURL, req.Payload)
	if nativeAuth == nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "failed to build copilot openai auth"}
	}
	nativeReq := normalizeGitHubCopilotRequestModel(req)
	return NewOpenAICompatExecutor(e.Identifier(), e.cfg).Execute(ctx, nativeAuth, nativeReq, opts)
}

func (e *GitHubCopilotExecutor) executeChatStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	nativeAuth := buildCopilotOpenAIGatewayAuth(auth, apiToken, baseURL, req.Payload)
	if nativeAuth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "failed to build copilot openai auth"}
	}
	nativeReq := normalizeGitHubCopilotRequestModel(req)
	return NewOpenAICompatExecutor(e.Identifier(), e.cfg).ExecuteStream(ctx, nativeAuth, nativeReq, opts)
}

func (e *GitHubCopilotExecutor) executeResponses(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return resp, err
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, req.Model, originalPayloadSource, false)
	body := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), false)
	body = normalizeGitHubCopilotResponseModel(req.Model, body)
	body = stripUnsupportedBetas(body)
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "codex", e.Identifier())
	if err != nil {
		return resp, err
	}
	body = normalizeGitHubCopilotResponsesInput(body)
	body = normalizeGitHubCopilotResponsesTools(body)
	body = applyGitHubCopilotResponsesDefaults(body)
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, req.Model, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "stream", false)

	url := strings.TrimRight(baseURL, "/") + githubCopilotResponsesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	e.applyHeaders(httpReq, apiToken, req.Payload)
	if detectVisionContent(req.Payload) {
		httpReq.Header.Set("Copilot-Vision-Request", "true")
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("github-copilot executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, data)
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return resp, err
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	data = normalizeGitHubCopilotReasoningField(data)
	detail := parseOpenAIResponsesUsage(data)
	if detail.TotalTokens > 0 {
		reporter.publish(ctx, detail)
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), body, data, &param)
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *GitHubCopilotExecutor) executeResponsesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, req.Model, originalPayloadSource, true)
	body := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), true)
	body = normalizeGitHubCopilotResponseModel(req.Model, body)
	body = stripUnsupportedBetas(body)
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "codex", e.Identifier())
	if err != nil {
		return nil, err
	}
	body = normalizeGitHubCopilotResponsesInput(body)
	body = normalizeGitHubCopilotResponsesTools(body)
	body = applyGitHubCopilotResponsesDefaults(body)
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, req.Model, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "stream", true)

	url := strings.TrimRight(baseURL, "/") + githubCopilotResponsesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	e.applyHeaders(httpReq, apiToken, req.Payload)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	if detectVisionContent(req.Payload) {
		httpReq.Header.Set("Copilot-Vision-Request", "true")
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("github-copilot executor: close response body error: %v", errClose)
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		return nil, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("github-copilot executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 20_971_520)
		var param any
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			if bytes.HasPrefix(line, dataTag) {
				sseData := bytes.TrimSpace(line[len(dataTag):])
				if !bytes.Equal(sseData, []byte("[DONE]")) && gjson.ValidBytes(sseData) {
					sseData = normalizeGitHubCopilotReasoningField(sseData)
					line = append(append([]byte(nil), dataTag...), sseData...)
				}
				if detail, ok := parseOpenAIResponsesStreamUsage(line); ok {
					reporter.publish(ctx, detail)
				}
			}

			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), body, line, &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: bytes.Clone(chunks[i])}
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else {
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), body, []byte("data: [DONE]"), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: bytes.Clone(chunks[i])}
			}
			reporter.ensurePublished(ctx)
		}
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: httpResp.Header.Clone(),
		Chunks:  out,
	}, nil
}

func (e *GitHubCopilotExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if githubCopilotUsesAnthropicGateway(req.Model) {
		apiToken, baseURL, err := e.ensureAPIToken(ctx, auth)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		nativeAuth := buildCopilotAnthropicGatewayAuth(auth, apiToken, baseURL, req.Payload)
		if nativeAuth == nil {
			return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "failed to build copilot claude auth"}
		}
		nativeReq := normalizeGitHubCopilotRequestModel(req)
		return NewClaudeExecutor(e.cfg).CountTokens(ctx, nativeAuth, nativeReq, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	enc, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("github copilot executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("github copilot executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

func (e *GitHubCopilotExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		return auth, nil
	}
	copilotAuth := copilotauth.NewCopilotAuth(e.cfg)
	_, err := copilotAuth.GetCopilotAPIToken(ctx, accessToken)
	if err != nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: fmt.Sprintf("github-copilot token validation failed: %v", err)}
	}
	return auth, nil
}

func (e *GitHubCopilotExecutor) ensureAPIToken(ctx context.Context, auth *cliproxyauth.Auth) (string, string, error) {
	if auth == nil {
		return "", "", statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		return "", "", statusErr{code: http.StatusUnauthorized, msg: "missing github access token"}
	}

	e.mu.RLock()
	if cached, ok := e.cache[accessToken]; ok && cached.expiresAt.After(time.Now().Add(tokenExpiryBuffer)) {
		e.mu.RUnlock()
		return cached.token, cached.apiEndpoint, nil
	}
	e.mu.RUnlock()

	copilotAuth := copilotauth.NewCopilotAuth(e.cfg)
	apiToken, err := copilotAuth.GetCopilotAPIToken(ctx, accessToken)
	if err != nil {
		return "", "", statusErr{code: http.StatusUnauthorized, msg: fmt.Sprintf("failed to get copilot api token: %v", err)}
	}

	apiEndpoint := githubCopilotBaseURL
	if apiToken.Endpoints.API != "" {
		apiEndpoint = strings.TrimRight(apiToken.Endpoints.API, "/")
	}

	expiresAt := time.Now().Add(githubCopilotTokenCacheTTL)
	if apiToken.ExpiresAt > 0 {
		expiresAt = time.Unix(apiToken.ExpiresAt, 0)
	}
	e.mu.Lock()
	e.cache[accessToken] = &cachedAPIToken{
		token:       apiToken.Token,
		apiEndpoint: apiEndpoint,
		expiresAt:   expiresAt,
	}
	e.mu.Unlock()

	return apiToken.Token, apiEndpoint, nil
}

func (e *GitHubCopilotExecutor) applyHeaders(r *http.Request, apiToken string, body []byte) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiToken)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", copilotUserAgent)
	r.Header.Set("Editor-Version", copilotEditorVersion)
	r.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	r.Header.Set("Openai-Intent", copilotOpenAIIntent)
	r.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	r.Header.Set("X-Github-Api-Version", copilotGitHubAPIVer)
	r.Header.Set("X-Request-Id", uuid.NewString())

	initiator := "user"
	if isAgentInitiated(body) {
		initiator = "agent"
	}
	r.Header.Set("X-Initiator", initiator)
}

func buildCopilotAnthropicGatewayAuth(auth *cliproxyauth.Auth, apiToken, baseURL string, body []byte) *cliproxyauth.Auth {
	apiToken = strings.TrimSpace(apiToken)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if apiToken == "" || baseURL == "" {
		return nil
	}

	nativeAuth := auth.Clone()
	if nativeAuth == nil {
		nativeAuth = &cliproxyauth.Auth{}
	}
	nativeAuth.Provider = "claude"
	if nativeAuth.Attributes == nil {
		nativeAuth.Attributes = make(map[string]string)
	}
	nativeAuth.Attributes["api_key"] = apiToken
	nativeAuth.Attributes["base_url"] = baseURL
	setCopilotHeaderAttrs(nativeAuth.Attributes, body)
	return nativeAuth
}

func buildCopilotOpenAIGatewayAuth(auth *cliproxyauth.Auth, apiToken, baseURL string, body []byte) *cliproxyauth.Auth {
	apiToken = strings.TrimSpace(apiToken)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if apiToken == "" || baseURL == "" {
		return nil
	}

	nativeAuth := auth.Clone()
	if nativeAuth == nil {
		nativeAuth = &cliproxyauth.Auth{}
	}
	nativeAuth.Provider = githubCopilotAuthType
	if nativeAuth.Attributes == nil {
		nativeAuth.Attributes = make(map[string]string)
	}
	nativeAuth.Attributes["api_key"] = apiToken
	nativeAuth.Attributes["base_url"] = baseURL
	setCopilotHeaderAttrs(nativeAuth.Attributes, body)
	return nativeAuth
}

func setCopilotHeaderAttrs(attrs map[string]string, body []byte) {
	if attrs == nil {
		return
	}
	attrs["header:User-Agent"] = copilotUserAgent
	attrs["header:Editor-Version"] = copilotEditorVersion
	attrs["header:Editor-Plugin-Version"] = copilotPluginVersion
	attrs["header:Openai-Intent"] = copilotOpenAIIntent
	attrs["header:Copilot-Integration-Id"] = copilotIntegrationID
	attrs["header:X-Github-Api-Version"] = copilotGitHubAPIVer
	attrs["header:X-Request-Id"] = uuid.NewString()
	if isAgentInitiated(body) {
		attrs["header:X-Initiator"] = "agent"
	} else {
		attrs["header:X-Initiator"] = "user"
	}
	if detectVisionContent(body) {
		attrs["header:Copilot-Vision-Request"] = "true"
	}
}

func normalizeGitHubCopilotRequestModel(req cliproxyexecutor.Request) cliproxyexecutor.Request {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if baseModel == "" || baseModel == req.Model {
		return req
	}
	req.Model = baseModel
	req.Payload = normalizeGitHubCopilotResponseModel(baseModel, req.Payload)
	return req
}

func normalizeGitHubCopilotResponseModel(model string, body []byte) []byte {
	if len(body) == 0 || strings.TrimSpace(model) == "" || !gjson.ValidBytes(body) {
		return body
	}
	updated, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return updated
}

func githubCopilotUsesAnthropicGateway(model string) bool {
	baseModel := strings.ToLower(thinking.ParseSuffix(model).ModelName)
	return strings.HasPrefix(baseModel, "claude-")
}

func useGitHubCopilotResponsesEndpoint(sourceFormat sdktranslator.Format, model string) bool {
	if sourceFormat.String() == "openai-response" {
		return true
	}
	baseModel := strings.ToLower(thinking.ParseSuffix(model).ModelName)
	if info := registry.GetGlobalRegistry().GetModelInfo(baseModel, githubCopilotAuthType); info != nil {
		return len(info.SupportedEndpoints) > 0 && !containsEndpoint(info.SupportedEndpoints, githubCopilotChatPath) && containsEndpoint(info.SupportedEndpoints, githubCopilotResponsesPath)
	}
	if info := lookupGitHubCopilotStaticModelInfo(baseModel); info != nil {
		return len(info.SupportedEndpoints) > 0 && !containsEndpoint(info.SupportedEndpoints, githubCopilotChatPath) && containsEndpoint(info.SupportedEndpoints, githubCopilotResponsesPath)
	}
	return strings.Contains(baseModel, "codex")
}

func lookupGitHubCopilotStaticModelInfo(model string) *registry.ModelInfo {
	for _, info := range registry.GetStaticModelDefinitionsByChannel(githubCopilotAuthType) {
		if info != nil && strings.EqualFold(info.ID, model) {
			return info
		}
	}
	return nil
}

func containsEndpoint(endpoints []string, endpoint string) bool {
	for _, item := range endpoints {
		if item == endpoint {
			return true
		}
	}
	return false
}

func detectVisionContent(body []byte) bool {
	messagesResult := gjson.GetBytes(body, "messages")
	if !messagesResult.Exists() || !messagesResult.IsArray() {
		return false
	}
	for _, message := range messagesResult.Array() {
		content := message.Get("content")
		if content.IsArray() {
			for _, block := range content.Array() {
				blockType := block.Get("type").String()
				if blockType == "image_url" || blockType == "image" {
					return true
				}
			}
		}
	}
	return false
}

func stripUnsupportedBetas(body []byte) []byte {
	betaPaths := []string{"betas", "metadata.betas"}
	for _, path := range betaPaths {
		arr := gjson.GetBytes(body, path)
		if !arr.Exists() || !arr.IsArray() {
			continue
		}
		var filtered []string
		changed := false
		for _, item := range arr.Array() {
			beta := item.String()
			if beta == "context-1m-2025-08-07" {
				changed = true
				continue
			}
			filtered = append(filtered, beta)
		}
		if !changed {
			continue
		}
		if len(filtered) == 0 {
			body, _ = sjson.DeleteBytes(body, path)
		} else {
			body, _ = sjson.SetBytes(body, path, filtered)
		}
	}
	return body
}

func normalizeGitHubCopilotReasoningField(data []byte) []byte {
	choices := gjson.GetBytes(data, "choices")
	if !choices.Exists() || !choices.IsArray() {
		return data
	}
	for i := range choices.Array() {
		msgRT := fmt.Sprintf("choices.%d.message.reasoning_text", i)
		msgRC := fmt.Sprintf("choices.%d.message.reasoning_content", i)
		if rt := gjson.GetBytes(data, msgRT); rt.Exists() && rt.String() != "" {
			if rc := gjson.GetBytes(data, msgRC); !rc.Exists() || rc.Type == gjson.Null || rc.String() == "" {
				data, _ = sjson.SetBytes(data, msgRC, rt.String())
			}
		}
		deltaRT := fmt.Sprintf("choices.%d.delta.reasoning_text", i)
		deltaRC := fmt.Sprintf("choices.%d.delta.reasoning_content", i)
		if rt := gjson.GetBytes(data, deltaRT); rt.Exists() && rt.String() != "" {
			if rc := gjson.GetBytes(data, deltaRC); !rc.Exists() || rc.Type == gjson.Null || rc.String() == "" {
				data, _ = sjson.SetBytes(data, deltaRC, rt.String())
			}
		}
	}
	return data
}

func isAgentInitiated(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		arr := messages.Array()
		if len(arr) == 0 {
			return false
		}
		lastRole := ""
		for i := len(arr) - 1; i >= 0; i-- {
			if r := arr[i].Get("role").String(); r != "" {
				lastRole = r
				break
			}
		}
		if lastRole == "assistant" || lastRole == "tool" {
			return true
		}
		if lastRole == "user" {
			lastContent := arr[len(arr)-1].Get("content")
			if lastContent.Exists() && lastContent.IsArray() {
				for _, part := range lastContent.Array() {
					if part.Get("type").String() == "tool_result" {
						return true
					}
				}
			}
			if len(arr) >= 2 {
				prev := arr[len(arr)-2]
				if prev.Get("role").String() == "assistant" {
					prevContent := prev.Get("content")
					if prevContent.Exists() && prevContent.IsArray() {
						for _, part := range prevContent.Array() {
							if part.Get("type").String() == "tool_use" {
								return true
							}
						}
					}
				}
			}
		}
		return false
	}
	if inputs := gjson.GetBytes(body, "input"); inputs.Exists() && inputs.IsArray() {
		arr := inputs.Array()
		if len(arr) == 0 {
			return false
		}
		last := arr[len(arr)-1]
		if role := last.Get("role").String(); role == "assistant" {
			return true
		}
		switch last.Get("type").String() {
		case "function_call", "function_call_arguments", "computer_call", "function_call_output", "function_call_response", "tool_result", "computer_call_output":
			return true
		}
		for _, item := range arr {
			if role := item.Get("role").String(); role == "assistant" {
				return true
			}
			switch item.Get("type").String() {
			case "function_call", "function_call_output", "function_call_response", "function_call_arguments", "computer_call", "computer_call_output":
				return true
			}
		}
	}
	return false
}

func normalizeGitHubCopilotResponsesInput(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "service_tier")
	input := gjson.GetBytes(body, "input")
	if input.Exists() {
		if input.Type == gjson.String || input.IsArray() {
			return body
		}
		body, _ = sjson.SetBytes(body, "input", input.Raw)
		return body
	}
	return body
}

func normalizeGitHubCopilotResponsesTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() {
		filtered := "[]"
		if tools.IsArray() {
			for _, tool := range tools.Array() {
				toolType := tool.Get("type").String()
				if toolType != "" && toolType != "function" {
					continue
				}
				name := tool.Get("name").String()
				if name == "" {
					name = tool.Get("function.name").String()
				}
				if name == "" {
					continue
				}
				normalized := `{"type":"function","name":""}`
				normalized, _ = sjson.Set(normalized, "name", name)
				if params := tool.Get("parameters"); params.Exists() {
					normalized, _ = sjson.SetRaw(normalized, "parameters", params.Raw)
				} else if params = tool.Get("function.parameters"); params.Exists() {
					normalized, _ = sjson.SetRaw(normalized, "parameters", params.Raw)
				} else if params = tool.Get("input_schema"); params.Exists() {
					normalized, _ = sjson.SetRaw(normalized, "parameters", params.Raw)
				}
				filtered, _ = sjson.SetRaw(filtered, "-1", normalized)
			}
		}
		body, _ = sjson.SetRawBytes(body, "tools", []byte(filtered))
	}
	return body
}

func applyGitHubCopilotResponsesDefaults(body []byte) []byte {
	if !gjson.GetBytes(body, "store").Exists() {
		body, _ = sjson.SetBytes(body, "store", false)
	}
	if !gjson.GetBytes(body, "include").Exists() {
		body, _ = sjson.SetRawBytes(body, "include", []byte(`["reasoning.encrypted_content"]`))
	}
	if gjson.GetBytes(body, "reasoning.effort").Exists() && !gjson.GetBytes(body, "reasoning.summary").Exists() {
		body, _ = sjson.SetBytes(body, "reasoning.summary", "auto")
	}
	return body
}

const (
	defaultCopilotContextLength        = 128000
	defaultCopilotMaxCompletionTokens  = 16384
)

func FetchGitHubCopilotModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	if auth == nil {
		log.Debug("github-copilot: auth is nil, using static models")
		return registry.GetGitHubCopilotModels()
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		log.Debug("github-copilot: no access_token in auth metadata, using static models")
		return registry.GetGitHubCopilotModels()
	}

	copilotAuth := copilotauth.NewCopilotAuth(cfg)
	entries, err := copilotAuth.ListModelsWithGitHubToken(ctx, accessToken)
	if err != nil {
		log.Warnf("github-copilot: failed to fetch dynamic models: %v, using static models", err)
		return registry.GetGitHubCopilotModels()
	}
	if len(entries) == 0 {
		log.Debug("github-copilot: API returned no models, using static models")
		return registry.GetGitHubCopilotModels()
	}

	staticMap := make(map[string]*registry.ModelInfo)
	for _, m := range registry.GetGitHubCopilotModels() {
		staticMap[m.ID] = m
	}

	now := time.Now().Unix()
	models := make([]*registry.ModelInfo, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.ID == "" {
			continue
		}
		if _, dup := seen[entry.ID]; dup {
			continue
		}
		seen[entry.ID] = struct{}{}

		m := &registry.ModelInfo{
			ID:      entry.ID,
			Object:  "model",
			Created: now,
			OwnedBy: "github-copilot",
			Type:    "github-copilot",
		}
		if entry.Created > 0 {
			m.Created = entry.Created
		}
		if entry.Name != "" {
			m.DisplayName = entry.Name
		} else {
			m.DisplayName = entry.ID
		}
		if static, ok := staticMap[entry.ID]; ok {
			if m.DisplayName == entry.ID && static.DisplayName != "" {
				m.DisplayName = static.DisplayName
			}
			m.Description = static.Description
			m.ContextLength = static.ContextLength
			m.MaxCompletionTokens = static.MaxCompletionTokens
			m.SupportedEndpoints = static.SupportedEndpoints
			m.Thinking = static.Thinking
		} else {
			m.Description = entry.ID + " via GitHub Copilot"
			m.ContextLength = defaultCopilotContextLength
			m.MaxCompletionTokens = defaultCopilotMaxCompletionTokens
		}
		if limits := entry.Limits(); limits != nil {
			if limits.MaxPromptTokens > 0 {
				m.ContextLength = limits.MaxPromptTokens
			}
			if limits.MaxOutputTokens > 0 {
				m.MaxCompletionTokens = limits.MaxOutputTokens
			}
		}
		models = append(models, m)
	}

	log.Infof("github-copilot: fetched %d models from API", len(models))
	return models
}

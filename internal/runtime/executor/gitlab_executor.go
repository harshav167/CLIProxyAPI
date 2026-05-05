package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gitlab"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	gitLabProviderKey             = "gitlab"
	gitLabAuthMethodOAuth         = "oauth"
	gitLabAuthMethodPAT           = "pat"
	gitLabChatEndpoint            = "/api/v4/chat/completions"
	gitLabCodeSuggestionsEndpoint = "/api/v4/code_suggestions/completions"
	gitLabSSEStreamingHeader      = "X-Supports-Sse-Streaming"
	gitLabContext1MBeta           = "context-1m-2025-08-07"
	gitLabNativeUserAgent         = "CLIProxyAPIPlus/GitLab-Duo"
)

type GitLabExecutor struct {
	cfg *config.Config
}

type gitLabCatalogModel struct {
	ID          string
	DisplayName string
	Provider    string
}

type gitLabPrompt struct {
	Instruction           string
	FileName              string
	ContentAboveCursor    string
	ChatContext           []map[string]any
	CodeSuggestionContext []map[string]any
}

type gitLabOpenAIStreamState struct {
	ID           string
	Model        string
	Created      int64
	LastFullText string
	Started      bool
	Finished     bool
}

var gitLabAgenticCatalog = []gitLabCatalogModel{
	{ID: "duo-chat-gpt-5-1", DisplayName: "GitLab Duo (GPT-5.1)", Provider: "openai"},
	{ID: "duo-chat-opus-4-6", DisplayName: "GitLab Duo (Claude Opus 4.6)", Provider: "anthropic"},
	{ID: "duo-chat-opus-4-5", DisplayName: "GitLab Duo (Claude Opus 4.5)", Provider: "anthropic"},
	{ID: "duo-chat-sonnet-4-6", DisplayName: "GitLab Duo (Claude Sonnet 4.6)", Provider: "anthropic"},
	{ID: "duo-chat-sonnet-4-5", DisplayName: "GitLab Duo (Claude Sonnet 4.5)", Provider: "anthropic"},
	{ID: "duo-chat-gpt-5-mini", DisplayName: "GitLab Duo (GPT-5 Mini)", Provider: "openai"},
	{ID: "duo-chat-gpt-5-2", DisplayName: "GitLab Duo (GPT-5.2)", Provider: "openai"},
	{ID: "duo-chat-gpt-5-2-codex", DisplayName: "GitLab Duo (GPT-5.2 Codex)", Provider: "openai"},
	{ID: "duo-chat-gpt-5-codex", DisplayName: "GitLab Duo (GPT-5 Codex)", Provider: "openai"},
	{ID: "duo-chat-haiku-4-5", DisplayName: "GitLab Duo (Claude Haiku 4.5)", Provider: "anthropic"},
}

var gitLabModelAliases = map[string]string{
	"duo-chat-haiku-4-6": "duo-chat-haiku-4-5",
}

func NewGitLabExecutor(cfg *config.Config) *GitLabExecutor {
	return &GitLabExecutor{cfg: cfg}
}

func (e *GitLabExecutor) Identifier() string { return gitLabProviderKey }

func (e *GitLabExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if nativeExec, nativeAuth, nativeReq, ok := e.nativeGateway(auth, req); ok {
		return nativeExec.Execute(ctx, nativeAuth, nativeReq, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	translated, err := e.translateToOpenAI(req, opts)
	if err != nil {
		return resp, err
	}
	prompt := buildGitLabPrompt(translated)
	if strings.TrimSpace(prompt.Instruction) == "" && strings.TrimSpace(prompt.ContentAboveCursor) == "" {
		err = statusErr{code: http.StatusBadRequest, msg: "gitlab duo executor: request has no usable text content"}
		return resp, err
	}

	text, err := e.invokeText(ctx, auth, prompt)
	if err != nil {
		return resp, err
	}

	responseModel := gitLabResolvedModel(auth, req.Model)
	openAIResponse := buildGitLabOpenAIResponse(responseModel, text, translated)
	reporter.publish(ctx, parseOpenAIUsage(openAIResponse))
	reporter.ensurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		sdktranslator.FromString("openai"),
		opts.SourceFormat,
		req.Model,
		opts.OriginalRequest,
		translated,
		openAIResponse,
		&param,
	)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: make(http.Header)}, nil
}

func (e *GitLabExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if nativeExec, nativeAuth, nativeReq, ok := e.nativeGateway(auth, req); ok {
		return nativeExec.ExecuteStream(ctx, nativeAuth, nativeReq, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	translated, err := e.translateToOpenAI(req, opts)
	if err != nil {
		return nil, err
	}
	prompt := buildGitLabPrompt(translated)
	if strings.TrimSpace(prompt.Instruction) == "" && strings.TrimSpace(prompt.ContentAboveCursor) == "" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "gitlab duo executor: request has no usable text content"}
	}

	if result, streamErr := e.requestCodeSuggestionsStream(ctx, auth, prompt, translated, req, opts, reporter); streamErr == nil {
		return result, nil
	} else if !shouldFallbackToCodeSuggestions(streamErr) {
		return nil, streamErr
	}

	text, err := e.invokeText(ctx, auth, prompt)
	if err != nil {
		return nil, err
	}
	responseModel := gitLabResolvedModel(auth, req.Model)
	openAIResponse := buildGitLabOpenAIResponse(responseModel, text, translated)
	reporter.publish(ctx, parseOpenAIUsage(openAIResponse))
	reporter.ensurePublished(ctx)

	out := make(chan cliproxyexecutor.StreamChunk, 8)
	go func() {
		defer close(out)
		var param any
		lines := buildGitLabOpenAIStream(responseModel, text)
		for _, line := range lines {
			chunks := sdktranslator.TranslateStream(
				ctx,
				sdktranslator.FromString("openai"),
				opts.SourceFormat,
				req.Model,
				opts.OriginalRequest,
				translated,
				[]byte(line),
				&param,
			)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: make(http.Header), Chunks: out}, nil
}

func (e *GitLabExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("gitlab duo executor: auth is nil")
	}
	baseURL := gitLabBaseURL(auth)
	token := gitLabPrimaryToken(auth)
	if baseURL == "" || token == "" {
		return nil, fmt.Errorf("gitlab duo executor: missing base URL or token")
	}

	client := gitlab.NewAuthClient(e.cfg)
	method := strings.ToLower(strings.TrimSpace(gitLabMetadataString(auth.Metadata, "auth_method", "auth_kind")))
	if method == "" {
		method = gitLabAuthMethodOAuth
	}

	if method == gitLabAuthMethodOAuth {
		if refreshed, refreshErr := e.refreshOAuthToken(ctx, client, auth, baseURL); refreshErr == nil && refreshed != nil {
			token = refreshed.AccessToken
			applyGitLabTokenMetadata(auth.Metadata, refreshed)
		}
	}

	direct, err := client.FetchDirectAccess(ctx, baseURL, token)
	if err != nil && method == gitLabAuthMethodOAuth {
		if refreshed, refreshErr := e.refreshOAuthToken(ctx, client, auth, baseURL); refreshErr == nil && refreshed != nil {
			token = refreshed.AccessToken
			applyGitLabTokenMetadata(auth.Metadata, refreshed)
			direct, err = client.FetchDirectAccess(ctx, baseURL, token)
		}
	}
	if err != nil {
		return nil, err
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = gitLabProviderKey
	auth.Metadata["auth_method"] = method
	auth.Metadata["auth_kind"] = gitLabAuthKind(method)
	auth.Metadata["base_url"] = gitlab.NormalizeBaseURL(baseURL)
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	mergeGitLabDirectAccessMetadata(auth.Metadata, direct)
	return auth, nil
}

func (e *GitLabExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if nativeExec, nativeAuth, nativeReq, ok := e.nativeGateway(auth, req); ok {
		return nativeExec.CountTokens(ctx, nativeAuth, nativeReq, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	translated := sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FromString("openai"), baseModel, req.Payload, false)
	enc, err := tokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("gitlab duo executor: tokenizer init failed: %w", err)
	}
	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: buildOpenAIUsageJSON(count), Headers: make(http.Header)}, nil
}

func (e *GitLabExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("gitlab duo executor: request is nil")
	}
	if nativeExec, nativeAuth := e.nativeGatewayHTTP(auth); nativeExec != nil {
		return nativeExec.HttpRequest(ctx, nativeAuth, req)
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if token := gitLabPrimaryToken(auth); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	return newProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
}

func (e *GitLabExecutor) translateToOpenAI(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	return sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FromString("openai"), baseModel, req.Payload, opts.Stream), nil
}

func (e *GitLabExecutor) nativeGateway(
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
) (cliproxyauth.ProviderExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, bool) {
	if nativeAuth, ok := buildGitLabAnthropicGatewayAuth(auth, req.Model); ok {
		nativeReq := req
		nativeReq.Model = gitLabResolvedModel(auth, req.Model)
		return NewClaudeExecutor(e.cfg), nativeAuth, nativeReq, true
	}
	if nativeAuth, ok := buildGitLabOpenAIGatewayAuth(auth, req.Model); ok {
		nativeReq := req
		nativeReq.Model = gitLabResolvedModel(auth, req.Model)
		return NewCodexExecutor(e.cfg), nativeAuth, nativeReq, true
	}
	return nil, nil, req, false
}

func (e *GitLabExecutor) nativeGatewayHTTP(auth *cliproxyauth.Auth) (cliproxyauth.ProviderExecutor, *cliproxyauth.Auth) {
	if nativeAuth, ok := buildGitLabAnthropicGatewayAuth(auth, ""); ok {
		return NewClaudeExecutor(e.cfg), nativeAuth
	}
	if nativeAuth, ok := buildGitLabOpenAIGatewayAuth(auth, ""); ok {
		return NewCodexExecutor(e.cfg), nativeAuth
	}
	return nil, nil
}

func (e *GitLabExecutor) invokeText(ctx context.Context, auth *cliproxyauth.Auth, prompt gitLabPrompt) (string, error) {
	if text, err := e.requestChat(ctx, auth, prompt); err == nil {
		return text, nil
	} else if !shouldFallbackToCodeSuggestions(err) {
		return "", err
	}
	return e.requestCodeSuggestions(ctx, auth, prompt)
}

func (e *GitLabExecutor) requestChat(ctx context.Context, auth *cliproxyauth.Auth, prompt gitLabPrompt) (string, error) {
	body := map[string]any{
		"content":            prompt.Instruction,
		"with_clean_history": true,
	}
	if len(prompt.ChatContext) > 0 {
		body["additional_context"] = prompt.ChatContext
	}
	return e.doJSONTextRequest(ctx, auth, gitLabChatEndpoint, body)
}

func (e *GitLabExecutor) requestCodeSuggestions(ctx context.Context, auth *cliproxyauth.Auth, prompt gitLabPrompt) (string, error) {
	contentAbove := strings.TrimSpace(prompt.ContentAboveCursor)
	if contentAbove == "" {
		contentAbove = prompt.Instruction
	}
	body := map[string]any{
		"current_file": map[string]any{
			"file_name":            prompt.FileName,
			"content_above_cursor": contentAbove,
			"content_below_cursor": "",
		},
		"intent":           "generation",
		"generation_type":  "small_file",
		"user_instruction": prompt.Instruction,
		"stream":           false,
	}
	if len(prompt.CodeSuggestionContext) > 0 {
		body["context"] = prompt.CodeSuggestionContext
	}
	return e.doJSONTextRequest(ctx, auth, gitLabCodeSuggestionsEndpoint, body)
}

func (e *GitLabExecutor) requestCodeSuggestionsStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	prompt gitLabPrompt,
	translated []byte,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	reporter *usageReporter,
) (*cliproxyexecutor.StreamResult, error) {
	contentAbove := strings.TrimSpace(prompt.ContentAboveCursor)
	if contentAbove == "" {
		contentAbove = prompt.Instruction
	}
	body := map[string]any{
		"current_file": map[string]any{
			"file_name":            prompt.FileName,
			"content_above_cursor": contentAbove,
			"content_below_cursor": "",
		},
		"intent":           "generation",
		"generation_type":  "small_file",
		"user_instruction": prompt.Instruction,
		"stream":           true,
	}
	if len(prompt.CodeSuggestionContext) > 0 {
		body["context"] = prompt.CodeSuggestionContext
	}

	httpResp, bodyRaw, err := e.doJSONRequest(ctx, auth, gitLabCodeSuggestionsEndpoint, body, "text/event-stream")
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		defer func() { _ = httpResp.Body.Close() }()
		respBody, readErr := io.ReadAll(httpResp.Body)
		if readErr != nil {
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		appendAPIResponseChunk(ctx, e.cfg, respBody)
		return nil, statusErr{code: httpResp.StatusCode, msg: strings.TrimSpace(string(respBody))}
	}

	responseModel := gitLabResolvedModel(auth, req.Model)
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		defer func() { _ = httpResp.Body.Close() }()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)

		var (
			param     any
			eventName string
			state     gitLabOpenAIStreamState
		)
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			if bytes.HasPrefix(trimmed, []byte("event:")) {
				eventName = strings.TrimSpace(string(trimmed[len("event:"):]))
				continue
			}
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			normalized := normalizeGitLabStreamChunk(eventName, payload, responseModel, &state)
			eventName = ""
			for _, item := range normalized {
				if detail, ok := parseOpenAIStreamUsage(item); ok {
					reporter.publish(ctx, detail)
				}
				chunks := sdktranslator.TranslateStream(
					ctx,
					sdktranslator.FromString("openai"),
					opts.SourceFormat,
					req.Model,
					opts.OriginalRequest,
					translated,
					item,
					&param,
				)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if !state.Finished {
			for _, item := range finalizeGitLabStream(responseModel, &state) {
				chunks := sdktranslator.TranslateStream(
					ctx,
					sdktranslator.FromString("openai"),
					opts.SourceFormat,
					req.Model,
					opts.OriginalRequest,
					translated,
					item,
					&param,
				)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
		}
		reporter.ensurePublished(ctx)
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: cloneGitLabStreamHeaders(httpResp.Header, bodyRaw),
		Chunks:  out,
	}, nil
}

func (e *GitLabExecutor) doJSONTextRequest(ctx context.Context, auth *cliproxyauth.Auth, endpoint string, payload map[string]any) (string, error) {
	resp, _, err := e.doJSONRequest(ctx, auth, endpoint, payload, "application/json")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return "", err
	}
	appendAPIResponseChunk(ctx, e.cfg, respBody)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", statusErr{code: resp.StatusCode, msg: strings.TrimSpace(string(respBody))}
	}

	text, err := parseGitLabTextResponse(endpoint, respBody)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func (e *GitLabExecutor) doJSONRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	endpoint string,
	payload map[string]any,
	accept string,
) (*http.Response, []byte, error) {
	token := gitLabPrimaryToken(auth)
	baseURL := gitLabBaseURL(auth)
	if token == "" || baseURL == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "gitlab duo executor: missing credentials"}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlab duo executor: marshal request failed: %w", err)
	}

	url := strings.TrimRight(baseURL, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "CLIProxyAPI/GitLab-Duo")
	applyGitLabRequestHeaders(req, auth)
	if strings.EqualFold(accept, "text/event-stream") {
		req.Header.Set(gitLabSSEStreamingHeader, "true")
	}

	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header.Clone(),
		Body:    bytes.Clone(body),
	})

	client := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := client.Do(req)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, body, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	return resp, body, nil
}

func parseGitLabTextResponse(endpoint string, body []byte) (string, error) {
	result := gjson.ParseBytes(body)
	switch endpoint {
	case gitLabChatEndpoint:
		if result.Type == gjson.String {
			return result.String(), nil
		}
		return strings.TrimSpace(result.String()), nil
	case gitLabCodeSuggestionsEndpoint:
		if text := strings.TrimSpace(result.Get("choices.0.text").String()); text != "" {
			return text, nil
		}
		if text := strings.TrimSpace(result.Get("choices.0.content").String()); text != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("gitlab duo executor: response did not contain text")
}

func buildGitLabPrompt(translated []byte) gitLabPrompt {
	result := gjson.ParseBytes(translated)
	prompt := gitLabPrompt{
		FileName: "main.go",
	}

	var instructionParts []string
	messages := result.Get("messages")
	if messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			role := strings.TrimSpace(message.Get("role").String())
			content := extractGitLabMessageContent(message.Get("content"))
			if content == "" {
				return true
			}
			switch role {
			case "system":
				instructionParts = append(instructionParts, content)
			case "user":
				if prompt.ContentAboveCursor == "" {
					prompt.ContentAboveCursor = content
				}
				instructionParts = append(instructionParts, content)
				prompt.ChatContext = append(prompt.ChatContext, map[string]any{
					"role":    "user",
					"content": content,
				})
				prompt.CodeSuggestionContext = append(prompt.CodeSuggestionContext, map[string]any{
					"file_name":            prompt.FileName,
					"content_above_cursor": content,
					"content_below_cursor": "",
				})
			default:
				prompt.ChatContext = append(prompt.ChatContext, map[string]any{
					"role":    role,
					"content": content,
				})
			}
			return true
		})
	}

	prompt.Instruction = strings.TrimSpace(strings.Join(instructionParts, "\n\n"))
	if prompt.Instruction == "" {
		prompt.Instruction = strings.TrimSpace(prompt.ContentAboveCursor)
	}
	if fileName := strings.TrimSpace(result.Get("metadata.file_name").String()); fileName != "" {
		prompt.FileName = fileName
	}
	return prompt
}

func extractGitLabMessageContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if !content.IsArray() {
		return strings.TrimSpace(content.String())
	}
	var parts []string
	content.ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "text", "input_text", "output_text":
			if text := strings.TrimSpace(item.Get("text").String()); text != "" {
				parts = append(parts, text)
			}
		default:
			if text := strings.TrimSpace(item.String()); text != "" {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func gitLabBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if baseURL := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "duo_gateway_base_url")); baseURL != "" {
		return strings.TrimRight(baseURL, "/")
	}
	if baseURL := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "base_url")); baseURL != "" {
		return gitlab.NormalizeBaseURL(baseURL)
	}
	return ""
}

func gitLabPrimaryToken(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"duo_gateway_token", "access_token", "personal_access_token"} {
		if token := strings.TrimSpace(gitLabMetadataString(auth.Metadata, key)); token != "" {
			return token
		}
	}
	return ""
}

func gitLabMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if metadata == nil {
			break
		}
		value, ok := metadata[key]
		if !ok || value == nil {
			continue
		}
		switch v := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		case fmt.Stringer:
			if trimmed := strings.TrimSpace(v.String()); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func gitLabResolvedModel(auth *cliproxyauth.Auth, requested string) string {
	requested = strings.TrimSpace(thinking.ParseSuffix(requested).ModelName)
	if requested != "" && !strings.EqualFold(requested, "gitlab-duo") {
		if mapped, ok := gitLabModelAliases[strings.ToLower(requested)]; ok && strings.TrimSpace(mapped) != "" {
			return mapped
		}
		return requested
	}
	if auth == nil || auth.Metadata == nil {
		return "gitlab-duo"
	}
	if model := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "model_name")); model != "" {
		if mapped, ok := gitLabModelAliases[strings.ToLower(model)]; ok && strings.TrimSpace(mapped) != "" {
			return mapped
		}
		return model
	}
	return "gitlab-duo"
}

func applyGitLabRequestHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	if req == nil || auth == nil || auth.Metadata == nil {
		return
	}
	headersRaw, ok := auth.Metadata["duo_gateway_headers"]
	if !ok || headersRaw == nil {
		return
	}
	switch headers := headersRaw.(type) {
	case map[string]string:
		for key, value := range headers {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			req.Header.Set(key, value)
		}
	case map[string]any:
		for key, value := range headers {
			key = strings.TrimSpace(key)
			if key == "" || value == nil {
				continue
			}
			req.Header.Set(key, strings.TrimSpace(fmt.Sprint(value)))
		}
	}
}

func (e *GitLabExecutor) refreshOAuthToken(ctx context.Context, client *gitlab.AuthClient, auth *cliproxyauth.Auth, baseURL string) (*gitlab.TokenResponse, error) {
	if client == nil || auth == nil || auth.Metadata == nil {
		return nil, fmt.Errorf("gitlab duo executor: missing refresh prerequisites")
	}
	refreshToken := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "refresh_token"))
	if refreshToken == "" {
		return nil, fmt.Errorf("gitlab duo executor: refresh token missing")
	}
	clientID := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "oauth_client_id"))
	clientSecret := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "oauth_client_secret"))
	return client.RefreshTokens(ctx, baseURL, clientID, clientSecret, refreshToken)
}

func applyGitLabTokenMetadata(metadata map[string]any, token *gitlab.TokenResponse) {
	if metadata == nil || token == nil {
		return
	}
	metadata["access_token"] = strings.TrimSpace(token.AccessToken)
	if refreshToken := strings.TrimSpace(token.RefreshToken); refreshToken != "" {
		metadata["refresh_token"] = refreshToken
	}
	if tokenType := strings.TrimSpace(token.TokenType); tokenType != "" {
		metadata["token_type"] = tokenType
	}
	if scope := strings.TrimSpace(token.Scope); scope != "" {
		metadata["scope"] = scope
	}
	if expiry := gitlab.TokenExpiry(time.Now(), token); !expiry.IsZero() {
		metadata["oauth_expires_at"] = expiry.Format(time.RFC3339)
	}
}

func gitLabAuthKind(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case gitLabAuthMethodPAT:
		return "personal_access_token"
	default:
		return "oauth"
	}
}

func mergeGitLabDirectAccessMetadata(metadata map[string]any, direct *gitlab.DirectAccessResponse) {
	if metadata == nil || direct == nil {
		return
	}
	if base := strings.TrimSpace(direct.BaseURL); base != "" {
		metadata["duo_gateway_base_url"] = base
	}
	if token := strings.TrimSpace(direct.Token); token != "" {
		metadata["duo_gateway_token"] = token
	}
	if direct.ExpiresAt > 0 {
		expiry := time.Unix(direct.ExpiresAt, 0).UTC()
		metadata["duo_gateway_expires_at"] = expiry.Format(time.RFC3339)
		now := time.Now().UTC()
		if ttl := expiry.Sub(now); ttl > 0 {
			interval := int(ttl.Seconds()) / 2
			switch {
			case interval < 60:
				interval = 60
			case interval > 240:
				interval = 240
			}
			metadata["refresh_interval_seconds"] = interval
		}
	}
	if len(direct.Headers) > 0 {
		headers := make(map[string]string, len(direct.Headers))
		for key, value := range direct.Headers {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			headers[key] = value
		}
		if len(headers) > 0 {
			metadata["duo_gateway_headers"] = headers
		}
	}
	if direct.ModelDetails != nil {
		if provider := strings.TrimSpace(direct.ModelDetails.ModelProvider); provider != "" {
			metadata["model_provider"] = provider
		}
		if model := strings.TrimSpace(direct.ModelDetails.ModelName); model != "" {
			metadata["model_name"] = model
		}
	}
}

func buildGitLabAnthropicGatewayAuth(auth *cliproxyauth.Auth, requestedModel string) (*cliproxyauth.Auth, bool) {
	if auth == nil || auth.Metadata == nil {
		return nil, false
	}
	provider := strings.ToLower(strings.TrimSpace(gitLabMetadataString(auth.Metadata, "model_provider")))
	if provider != "anthropic" {
		return nil, false
	}
	baseURL := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "duo_gateway_base_url"))
	token := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "duo_gateway_token"))
	if baseURL == "" || token == "" {
		return nil, false
	}
	native := auth.Clone()
	native.Provider = "claude"
	native.ProxyURL = ""
	if native.Metadata == nil {
		native.Metadata = make(map[string]any)
	}
	native.Metadata["api_key"] = token
	native.Metadata["base_url"] = strings.TrimRight(baseURL, "/") + "/v1/proxy/anthropic"
	if model := gitLabResolvedModel(auth, requestedModel); model != "" {
		native.Metadata["model_name"] = model
	}
	if auth.Metadata != nil {
		if headers, ok := auth.Metadata["duo_gateway_headers"]; ok {
			native.Metadata["duo_gateway_headers"] = headers
		}
	}
	if native.Attributes == nil {
		native.Attributes = make(map[string]string)
	}
	native.Attributes["api_key"] = token
	return native, true
}

func buildGitLabOpenAIGatewayAuth(auth *cliproxyauth.Auth, requestedModel string) (*cliproxyauth.Auth, bool) {
	if auth == nil || auth.Metadata == nil {
		return nil, false
	}
	provider := strings.ToLower(strings.TrimSpace(gitLabMetadataString(auth.Metadata, "model_provider")))
	if provider != "openai" {
		return nil, false
	}
	baseURL := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "duo_gateway_base_url"))
	token := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "duo_gateway_token"))
	if baseURL == "" || token == "" {
		return nil, false
	}
	native := auth.Clone()
	native.Provider = "codex"
	native.ProxyURL = ""
	if native.Metadata == nil {
		native.Metadata = make(map[string]any)
	}
	native.Metadata["api_key"] = token
	native.Metadata["base_url"] = strings.TrimRight(baseURL, "/") + "/v1/proxy/openai"
	if model := gitLabResolvedModel(auth, requestedModel); model != "" {
		native.Metadata["model_name"] = model
	}
	if auth.Metadata != nil {
		if headers, ok := auth.Metadata["duo_gateway_headers"]; ok {
			native.Metadata["duo_gateway_headers"] = headers
		}
	}
	if native.Attributes == nil {
		native.Attributes = make(map[string]string)
	}
	native.Attributes["api_key"] = token
	return native, true
}

func cloneGitLabStreamHeaders(headers http.Header, bodyRaw []byte) http.Header {
	cloned := make(http.Header)
	for key, values := range headers {
		for _, value := range values {
			cloned.Add(key, value)
		}
	}
	if len(bodyRaw) > 0 {
		cloned.Set("X-Original-Request-Bytes", fmt.Sprintf("%d", len(bodyRaw)))
	}
	return cloned
}

func normalizeGitLabStreamChunk(eventName string, payload []byte, responseModel string, state *gitLabOpenAIStreamState) [][]byte {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return finalizeGitLabStream(responseModel, state)
	}

	line := bytes.TrimSpace(payload)
	if bytes.HasPrefix(line, []byte("{")) && gjson.ValidBytes(line) {
		resultType := strings.TrimSpace(gjson.GetBytes(line, "type").String())
		switch resultType {
		case "response.created", "response.output_text.delta", "response.completed":
			return [][]byte{bytes.Clone(line)}
		}
	}

	text := string(payload)
	if strings.TrimSpace(eventName) == "completion" || strings.TrimSpace(eventName) == "" {
		return buildGitLabDeltaEvents(responseModel, text, state)
	}
	return nil
}

func buildGitLabDeltaEvents(responseModel, text string, state *gitLabOpenAIStreamState) [][]byte {
	now := time.Now().Unix()
	if state != nil {
		if state.ID == "" {
			state.ID = fmt.Sprintf("gitlab-stream-%d", now)
			state.Model = responseModel
			state.Created = now
		}
		state.Started = true
		state.LastFullText += text
	}
	created := now
	id := fmt.Sprintf("gitlab-stream-%d", now)
	if state != nil {
		created = state.Created
		id = state.ID
	}
	return [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"%s","created_at":%d,"model":%q}}`, id, created, responseModel)),
		[]byte(fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, text)),
	}
}

func finalizeGitLabStream(responseModel string, state *gitLabOpenAIStreamState) [][]byte {
	if state != nil && state.Finished {
		return nil
	}
	now := time.Now().Unix()
	id := fmt.Sprintf("gitlab-stream-%d", now)
	fullText := ""
	created := now
	if state != nil {
		if state.ID != "" {
			id = state.ID
		}
		if state.Created > 0 {
			created = state.Created
		}
		fullText = state.LastFullText
		state.Finished = true
	}
	return [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"%s","created_at":%d,"model":%q,"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":%q}]}]}}`, id, created, responseModel, fullText)),
	}
}

func buildGitLabOpenAIResponse(model, text string, translated []byte) []byte {
	created := time.Now().Unix()
	response := map[string]any{
		"id":      fmt.Sprintf("gitlab-%d", created),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	if len(translated) > 0 && gjson.ValidBytes(translated) {
		if requested := strings.TrimSpace(gjson.GetBytes(translated, "model").String()); requested != "" {
			response["requested_model"] = requested
		}
	}
	encoded, _ := json.Marshal(response)
	return encoded
}

func buildGitLabOpenAIStream(model, text string) []string {
	created := time.Now().Unix()
	id := fmt.Sprintf("gitlab-%d", created)
	return []string{
		fmt.Sprintf(`{"id":"%s","object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, id, created, model, text),
		fmt.Sprintf(`{"id":"%s","object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, id, created, model),
	}
}

func shouldFallbackToCodeSuggestions(err error) bool {
	if err == nil {
		return false
	}
	var status statusErr
	if ok := AsStatusErr(err, &status); ok {
		return status.code == http.StatusForbidden || status.code == http.StatusNotFound
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "feature unavailable")
}

func AsStatusErr(err error, target *statusErr) bool {
	if err == nil || target == nil {
		return false
	}
	se, ok := err.(statusErr)
	if ok {
		*target = se
		return true
	}
	return false
}

func GitLabModelsFromAuth(auth *cliproxyauth.Auth) []*registry.ModelInfo {
	models := make([]*registry.ModelInfo, 0, len(gitLabAgenticCatalog)+4)
	seen := make(map[string]struct{}, len(gitLabAgenticCatalog)+4)
	addModel := func(id, displayName, provider string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		models = append(models, &registry.ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     time.Now().Unix(),
			OwnedBy:     gitLabProviderKey,
			Type:        gitLabProviderKey,
			DisplayName: displayName,
		})
	}

	for _, model := range gitLabAgenticCatalog {
		addModel(model.ID, model.DisplayName, model.Provider)
	}

	if auth != nil && auth.Metadata != nil {
		if modelName := strings.TrimSpace(gitLabMetadataString(auth.Metadata, "model_name")); modelName != "" {
			addModel(modelName, "GitLab Duo ("+modelName+")", strings.TrimSpace(gitLabMetadataString(auth.Metadata, "model_provider")))
		}
	}

	return models
}

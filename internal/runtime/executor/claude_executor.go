package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg *config.Config
}

type preparedClaudeRequest struct {
	baseModel                string
	apiKey                   string
	baseURL                  string
	from                     sdktranslator.Format
	to                       sdktranslator.Format
	bodyForTranslation       []byte
	bodyForUpstream          []byte
	extraBetas               []string
	oauthToolNamesReverseMap map[string]string
}

// claudeToolPrefix is empty to match real Claude Code behavior (no tool name prefix).
// Previously "proxy_" was used but this is a detectable fingerprint difference.
const claudeToolPrefix = helps.ClaudeToolPrefix

// oauthToolRenameMap aliases the reserved rename map in helps for OAuth remapping.
var oauthToolRenameMap = helps.ClaudeOAuthToolRenameMap

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return sigcompat.SignatureProviderFromModelName(baseModel) == sigcompat.SignatureProviderClaude
}

func sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx context.Context, body []byte, baseModel string) []byte {
	sanitized := body
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) {
		var report sigcompat.SignatureSanitizeReport
		sanitized, report = sigcompat.SanitizeClaudeMessagesForClaudeUpstream(body, baseModel)
		logClaudeSignatureSanitizeReport(ctx, baseModel, report)
	}
	return sanitizeClaudeWebSearchDomains(sanitized)
}

// sanitizeClaudeWebSearchDomains removes empty allowed_domains/blocked_domains
// arrays from built-in web_search tools. Some clients (e.g. litellm) emit an
// empty array instead of omitting the field, and Anthropic rejects it with
// "Empty list of domains is ambiguous. Provide at least one domain or null.".
// Deleting the key is equivalent to leaving it unset.
func sanitizeClaudeWebSearchDomains(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	tools.ForEach(func(index, tool gjson.Result) bool {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			return true
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if value.Exists() && value.IsArray() && len(value.Array()) == 0 {
				path := fmt.Sprintf("tools.%d.%s", index.Int(), field)
				if updated, errDelete := sjson.DeleteBytes(body, path); errDelete == nil {
					body = updated
				}
			}
		}
		return true
	})
	return body
}

func logClaudeSignatureSanitizeReport(ctx context.Context, baseModel string, report sigcompat.SignatureSanitizeReport) {
	if report.DroppedBlocks == 0 && report.DroppedSignatures == 0 && report.ReplacedSignatures == 0 {
		return
	}

	fields := log.Fields{
		"component":           "signature_sanitizer",
		"executor":            "claude",
		"action":              "sanitize_claude_messages",
		"target_provider":     string(report.TargetProvider),
		"target_model":        baseModel,
		"preserved":           report.Preserved,
		"dropped_blocks":      report.DroppedBlocks,
		"dropped_signatures":  report.DroppedSignatures,
		"replaced_signatures": report.ReplacedSignatures,
	}
	if len(report.Decisions) > 0 {
		decision := report.Decisions[0]
		fields["first_block_kind"] = string(decision.BlockKind)
		fields["first_detected_provider"] = string(decision.DetectedProvider)
		fields["first_reason"] = decision.Reason
	}

	helps.LogWithRequestID(ctx).WithFields(fields).Debug("claude executor: sanitized signature history before upstream")
}

// The reverse map is now computed per-request in remapOAuthToolNames so that
// only names the client actually caused us to rewrite are restored on the
// response. A global reverse map — as used previously — corrupted responses
// for clients that sent mixed casing (e.g. `Bash` TitleCase alongside `glob`
// lowercase; the request flagged renames via `glob` -> `Glob`, then the global
// reverse map incorrectly rewrote every `Bash` in the response to `bash`).

// oauthToolsToRemove lists tool names that must be stripped from OAuth requests
// even after remapping. Currently empty — all tools are mapped instead of removed.
var oauthToolsToRemove = map[string]bool{}

// Anthropic-compatible upstreams may reject or even crash when Claude models
// omit max_tokens. Prefer registered model metadata before using a fallback.
const defaultModelMaxTokens = 1024

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") && strings.EqualFold(req.URL.Host, "api.anthropic.com")
	if isAnthropicBase && useAPIKey {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *ClaudeExecutor) prepareMessagesRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (preparedClaudeRequest, error) {
	prepared := preparedClaudeRequest{
		baseModel: thinking.ParseSuffix(req.Model).ModelName,
		to:        sdktranslator.FromString("claude"),
	}
	prepared.apiKey, prepared.baseURL = claudeCreds(auth)
	if prepared.baseURL == "" {
		prepared.baseURL = "https://api.anthropic.com"
	}

	prepared.from = opts.SourceFormat
	translationStream := stream
	if !stream {
		// Use streaming translation to preserve function calling, except for claude.
		translationStream = prepared.from != prepared.to
	}

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(prepared.from, prepared.to, prepared.baseModel, originalPayloadSource, translationStream)
	body := sdktranslator.TranslateRequest(prepared.from, prepared.to, prepared.baseModel, req.Payload, translationStream)
	body, _ = sjson.SetBytes(body, "model", prepared.baseModel)

	var err error
	body, err = thinking.ApplyThinking(body, req.Model, prepared.from.String(), prepared.to.String(), e.Identifier())
	if err != nil {
		return prepared, err
	}

	// Cursor Fable 5 alias path: when Cursor sends a non-claude-prefixed alias
	// (f5-*), it skips its ZDR routing gate but also sends a generic custom-model
	// system prompt + tool set instead of the rich Cursor→Anthropic shape it
	// uses for real claude-* models. Swap both with a captured snapshot of a
	// real prod Cursor→Anthropic claude-* request so the upstream Fable 5 call
	// still sees Cursor's native tools and Cursor's Claude-flavour system
	// blocks. The model field has already been resolved to the backing
	// claude-fable-5 model upstream of here, so we key off the inbound req.Model.
	body = helps.ApplyCursorFableAliasSnapshot(body, req.Model)

	body = applyCloaking(ctx, e.cfg, auth, body, prepared.baseModel, prepared.apiKey)

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, prepared.baseModel, prepared.to.String(), prepared.from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = ensureModelMaxTokens(body, prepared.baseModel)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeTemperatureForThinking(body)

	if helps.CountClaudeCacheControls(body) == 0 {
		body = helps.EnsureClaudeCacheControl(body)
	}
	body = helps.EnsureClaudeUserPromptCacheAnchor(body)
	// The top-level automatic cache_control is a request-level breakpoint that
	// renders BEFORE tools/system/messages in Anthropic's prefix chain. When
	// global cache scope is enabled, a scoped system block exists later in the
	// chain, and a non-global top-level breakpoint before it triggers a 400
	// ("a block with scope:global was found after content with a narrower cache
	// scope"). Canonical Claude Code never sends a top-level cache_control (all
	// 2026-05-29 Opus 4.8 gist captures have it null), so skip it in the
	// global-scope path to match Claude Code exactly.
	if IsCursorClient(ctx) && !(e.cfg != nil && e.cfg.ClaudeCursorGlobalCacheScope) {
		body = helps.EnsureCursorClaudeAutomaticPromptCacheControl(body)
	}
	body = helps.EnforceClaudeCacheControlLimit(body, 4)
	body = helps.NormalizeClaudeCacheControlTTL(body)
	// Global-scope path: the system rewrite anchors scope:"global" on a system
	// block, which must be the FIRST cache_control breakpoint in the
	// tools→system→messages chain. Inbound payloads can carry their own bare
	// top-level / tool-level cache_control that survives the pipeline and
	// renders before the system anchor, tripping Anthropic's prefix-chain 400.
	// Strip those pre-anchor breakpoints so the global anchor leads the chain.
	//
	// Gated on the SAME condition that produces the global anchor
	// (cfg.ClaudeCursorGlobalCacheScope), NOT on IsCursorClient — the anchor is
	// emitted for any cloaked Claude request when the flag is on (see
	// applyCloaking → checkSystemInstructionsWithSigningMode), so a non-Cursor
	// cloaked request would otherwise keep its pre-anchor cache_control and 400.
	// The helper itself no-ops when the outgoing payload has no global anchor,
	// so this is safe for requests cloaking didn't touch.
	if e.cfg != nil && e.cfg.ClaudeCursorGlobalCacheScope {
		body = helps.StripClaudeCacheControlsBeforeGlobalAnchor(body)
	}
	body = stripUnsupportedAnthropicFields(body)

	prepared.extraBetas, body = extractAndRemoveBetas(body)
	prepared.bodyForTranslation = body
	prepared.bodyForUpstream = body
	oauthToken := isClaudeOAuthToken(prepared.apiKey)
	if oauthToken {
		prepared.bodyForUpstream, prepared.oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(prepared.bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	// Upstream signature sanitize (merged from upstream/main): drop/replace
	// incompatible signature history before forwarding to the Claude upstream.
	// Runs after OAuth tool-name remapping and before cch signing so the body
	// signed is the exact body sent.
	prepared.bodyForUpstream = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, prepared.bodyForUpstream, prepared.baseModel)
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) {
		prepared.bodyForUpstream = signAnthropicMessagesBody(prepared.bodyForUpstream)
	}
	return prepared, nil
}

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	// Adopt upstream's executor-aware usage reporter (executor-type tracking).
	// Credentials come from prepared.* below in our flow, so we do not call
	// claudeCreds here (that path is upstream-only and would be unused).
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	prepared, err := e.prepareMessagesRequest(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
<<<<<<< HEAD
	upstreamStream := prepared.from != prepared.to

	// SetTranslatedReasoningEffort is upstream 11f0f906: log the reasoning
	// effort as it appears in the final translated payload sent upstream.
	// We call it once here (and once in ExecuteStream) instead of inside
	// prepareMessagesRequest because reporter is owned by the caller and
	// prepared.bodyForUpstream is the right input.
	reporter.SetTranslatedReasoningEffort(prepared.bodyForUpstream, prepared.to.String())

	url := fmt.Sprintf("%s/v1/messages?beta=true", prepared.baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.bodyForUpstream))
	if err != nil {
		return resp, err
	}
	applyClaudeHeaders(httpReq, auth, prepared.apiKey, false, prepared.extraBetas, e.cfg)
=======
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	body, err = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey)
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = ensureModelMaxTokens(body, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeTemperatureForThinking(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	if countCacheControls(body) == 0 {
		body = ensureCacheControl(body)
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	// Cloaking and ensureCacheControl may push the total over 4 when the client
	// already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	oauthToken := isClaudeOAuthToken(apiKey)
	var oauthToolNamesReverseMap map[string]string
	if oauthToken {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	bodyForUpstream = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, bodyForUpstream, baseModel)
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	// Claude Code always computes cch; missing or invalid cch is a detectable fingerprint.
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) {
		bodyForUpstream = signAnthropicMessagesBody(bodyForUpstream)
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, e.cfg); errHeaders != nil {
		return resp, errHeaders
	}
>>>>>>> upstream/main
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      prepared.bodyForUpstream,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	reporter.RecordQuota(ctx, httpResp.Header)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return resp, statusErr{code: httpResp.StatusCode, msg: msg}
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), retryAfter: parseClaudeRetryAfter(httpResp.StatusCode, httpResp.Header, b, time.Now())}
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if upstreamStream {
		if errValidate := validateClaudeStreamingResponse(data); errValidate != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errValidate)
			return resp, errValidate
		}
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
				logClaudeUsageFromLine(prepared.baseModel, line)
			}
		}
	} else {
		reporter.Publish(ctx, helps.ParseClaudeUsage(data))
		if usageNode := gjson.ParseBytes(data).Get("usage"); usageNode.Exists() {
			helps.LogClaudeCacheUsage(prepared.baseModel, usageNode)
		}
	}
	data = restoreClaudeOAuthToolNamesFromResponse(data, claudeToolPrefix, auth.ToolPrefixDisabled(), prepared.oauthToolNamesReverseMap)
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		prepared.to,
		prepared.from,
		req.Model,
		opts.OriginalRequest,
		prepared.bodyForTranslation,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	// Adopt upstream's executor-aware usage reporter; creds come from prepared.*
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	prepared, err := e.prepareMessagesRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

<<<<<<< HEAD
	// Upstream 11f0f906: log reasoning effort as sent upstream.
	reporter.SetTranslatedReasoningEffort(prepared.bodyForUpstream, prepared.to.String())
=======
	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	body, err = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey)
	if err != nil {
		return nil, err
	}
>>>>>>> upstream/main

	url := fmt.Sprintf("%s/v1/messages?beta=true", prepared.baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.bodyForUpstream))
	if err != nil {
		return nil, err
	}
<<<<<<< HEAD
	applyClaudeHeaders(httpReq, auth, prepared.apiKey, true, prepared.extraBetas, e.cfg)
=======
	if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, e.cfg); errHeaders != nil {
		return nil, errHeaders
	}
>>>>>>> upstream/main
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      prepared.bodyForUpstream,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	reporter.RecordQuota(ctx, httpResp.Header)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return nil, statusErr{code: httpResp.StatusCode, msg: msg}
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b), retryAfter: parseClaudeRetryAfter(httpResp.StatusCode, httpResp.Header, b, time.Now())}
		return nil, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()

		// If from == to (Claude -> Claude), directly forward the SSE stream without translation.
		if prepared.from == prepared.to {
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			for scanner.Scan() {
				line := scanner.Bytes()
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
					logClaudeUsageFromLine(prepared.baseModel, line)
				}
				if streamErr := helps.DetectClaudeStreamError(line); streamErr != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
					case <-ctx.Done():
					}
					return
				}
				line = restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), prepared.oauthToolNamesReverseMap)
				// Forward the line as-is to preserve SSE format
				cloned := make([]byte, len(line)+1)
				copy(cloned, line)
				cloned[len(line)] = '\n'
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
				case <-ctx.Done():
					return
				}
			}
			if errScan := scanner.Err(); errScan != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
				case <-ctx.Done():
				}
			}
			return
		}

		// For other formats, use translation
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
				logClaudeUsageFromLine(prepared.baseModel, line)
			}
			if streamErr := helps.DetectClaudeStreamError(line); streamErr != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
				reporter.PublishFailure(ctx, streamErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return
			}
			line = restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), prepared.oauthToolNamesReverseMap)
			chunks := sdktranslator.TranslateStream(
				ctx,
				prepared.to,
				prepared.from,
				req.Model,
				opts.OriginalRequest,
				prepared.bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func validateClaudeStreamingResponse(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 52_428_800)

	hasData := false
	hasMessageStart := false
	hasMessageDelta := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		hasData = true
		if !gjson.ValidBytes(payload) {
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
		}

		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "error":
			message := strings.TrimSpace(root.Get("error.message").String())
			if message == "" {
				message = strings.TrimSpace(root.Get("error.type").String())
			}
			if message == "" {
				message = "unknown upstream error"
			}
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned error event: " + message}
		case "message_start":
			message := root.Get("message")
			if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
				return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream message_start is missing id or model"}
			}
			hasMessageStart = true
		case "message_delta":
			hasMessageDelta = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if !hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !hasMessageStart {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start"}
	}
	if !hasMessageDelta {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response ended before message completion"}
	}
	return nil
}

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	if !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
		body = checkSystemInstructions(body)
	}

	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = helps.EnforceClaudeCacheControlLimit(body, 4)
	body = helps.NormalizeClaudeCacheControlTTL(body)

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	if isClaudeOAuthToken(apiKey) {
		body, _ = prepareClaudeOAuthToolNamesForUpstream(body, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	body = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, body, baseModel)

	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, e.cfg); errHeaders != nil {
		return cliproxyexecutor.Response{}, errHeaders
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
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

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: msg}
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(b)}
	}
	decodedBody, err := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// stripUnsupportedAnthropicFields removes top-level fields that the Anthropic
// Messages API explicitly rejects with `invalid_request_error: Extra inputs
// are not permitted` when the body is forwarded verbatim. These fields come
// from clients that originally framed their request as OpenAI Chat Completions
// (notably Cursor BYOK with a custom `claude-*` model entry, which sets
// stream_options=...) and reach this executor via the openai→claude
// translator.
//
// Verified failures stripped here:
//   - stream_options: OpenAI-only field; Anthropic returns `invalid_request_error`
//     "stream_options: Extra inputs are not permitted" (Cursor BYOK 2026-05-04).
//
// Add additional fields here as new clients surface new rejections — keep the
// strip list narrow and document each entry with the exact upstream error.
func stripUnsupportedAnthropicFields(body []byte) []byte {
	for _, k := range []string{"stream_options"} {
		if gjson.GetBytes(body, k).Exists() {
			body, _ = sjson.DeleteBytes(body, k)
		}
	}
	return body
}

// clientInventedBetas enumerates betas that DO NOT come from Anthropic and
// should be stripped before forwarding upstream. Currently the only known
// entry is Cursor's `max-effort-2026-01-24` (Cursor-internal opt-in for the
// thinking-max alias; Claude Code itself never sends it).
var clientInventedBetas = map[string]struct{}{
	"max-effort-2026-01-24": {},
}

// isClientInventedBeta reports whether the supplied beta name is in the
// client-invented allowlist (i.e. should be stripped from the outgoing
// Anthropic-Beta header).
func isClientInventedBeta(name string) bool {
	_, ok := clientInventedBetas[strings.TrimSpace(name)]
	return ok
}

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
		// Adaptive thinking may also set output_config.effort; remove it to avoid
		// leaking thinking controls when tool_choice forces tool use.
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		if oc := gjson.GetBytes(body, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "output_config")
		}
	}
	return body
}

// normalizeClaudeTemperatureForThinking keeps Anthropic message requests valid when
// thinking is enabled. Anthropic rejects temperatures other than 1 when
// thinking.type is enabled/adaptive/auto.
func normalizeClaudeTemperatureForThinking(body []byte) []byte {
	if !gjson.GetBytes(body, "temperature").Exists() {
		return body
	}

	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
		if temp := gjson.GetBytes(body, "temperature"); temp.Exists() && temp.Type == gjson.Number && temp.Float() == 1 {
			return body
		}
		body, _ = sjson.SetBytes(body, "temperature", 1)
	}
	return body
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// peekableBody wraps a bufio.Reader around the original ReadCloser so that
// magic bytes can be inspected without consuming them from the stream.
type peekableBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableBody) Close() error {
	return p.closer.Close()
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		// No Content-Encoding header.  Attempt best-effort magic-byte detection to
		// handle misbehaving upstreams that compress without setting the header.
		// Only gzip (1f 8b) and zstd (28 b5 2f fd) have reliable magic sequences;
		// br and deflate have none and are left as-is.
		// The bufio wrapper preserves unread bytes so callers always see the full
		// stream regardless of whether decompression was applied.
		pb := &peekableBody{Reader: bufio.NewReader(body), closer: body}
		magic, peekErr := pb.Peek(4)
		if peekErr == nil || (peekErr == io.EOF && len(magic) >= 2) {
			switch {
			case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
				gzipReader, gzErr := gzip.NewReader(pb)
				if gzErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte gzip: failed to create reader: %w", gzErr)
				}
				return &compositeReadCloser{
					Reader: gzipReader,
					closers: []func() error{
						gzipReader.Close,
						pb.Close,
					},
				}, nil
			case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
				decoder, zdErr := zstd.NewReader(pb)
				if zdErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte zstd: failed to create reader: %w", zdErr)
				}
				return &compositeReadCloser{
					Reader: decoder,
					closers: []func() error{
						func() error { decoder.Close(); return nil },
						pb.Close,
					},
				}, nil
			}
		}
		return pb, nil
	}
	encodings := strings.Split(contentEncoding, ",")
	for _, raw := range encodings {
		encoding := strings.TrimSpace(strings.ToLower(raw))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, err := gzip.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create gzip reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: gzipReader,
				closers: []func() error{
					gzipReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "deflate":
			deflateReader := flate.NewReader(body)
			return &compositeReadCloser{
				Reader: deflateReader,
				closers: []func() error{
					deflateReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "br":
			return &compositeReadCloser{
				Reader: brotli.NewReader(body),
				closers: []func() error{
					func() error { return body.Close() },
				},
			}, nil
		case "zstd":
			decoder, err := zstd.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create zstd reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: decoder,
				closers: []func() error{
					func() error { decoder.Close(); return nil },
					func() error { return body.Close() },
				},
			}, nil
		default:
			continue
		}
	}
	return body, nil
}

func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, cfg *config.Config) error {
	if r == nil {
		return nil
	}
	hdrDefault := func(cfgVal, fallback string) string {
		if cfgVal != "" {
			return cfgVal
		}
		return fallback
	}

	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}

	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := r.URL != nil && strings.EqualFold(r.URL.Scheme, "https") && strings.EqualFold(r.URL.Host, "api.anthropic.com")
	if isAnthropicBase && useAPIKey {
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", apiKey)
	} else {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	r.Header.Set("Content-Type", "application/json")

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	stabilizeDeviceProfile := helps.ClaudeDeviceProfileStabilizationEnabled(cfg)
	var deviceProfile helps.ClaudeDeviceProfile
	if stabilizeDeviceProfile {
		var errDeviceProfile error
		deviceProfile, errDeviceProfile = helps.ResolveClaudeDeviceProfileRequired(r.Context(), auth, apiKey, ginHeaders, cfg)
		if errDeviceProfile != nil {
			return errDeviceProfile
		}
	}

	// We are talking to Anthropic via Claude Code OAuth (or fall back to API key
	// against the Anthropic base URL). Either way the most reliable behavior is
	// to send the canonical Claude Code beta set — the literal string a real
	// claude-cli/2.1.156 ships on every /v1/messages call (verified 2026-05-29
	// via Proxyman captures against api.anthropic.com on claude-opus-4-8).
	//
	// Why we ignore the client's inbound Anthropic-Beta header entirely:
	//   - Cursor BYOK Claude traffic sends `Anthropic-Beta: max-effort-2026-01-24`
	//     ONLY on the thinking-max alias. That beta is a Cursor-internal opt-in;
	//     Claude Code itself never sends it. Forwarding it doesn't help and
	//     could trip Anthropic anti-abuse heuristics that distinguish "looks like
	//     Claude Code" from "looks like a 3rd party calling the OAuth surface".
	//   - The previous "overwrite base with client header" path (pre-2026-05-29)
	//     silently dropped claude-code-20250219, structured-outputs-2025-12-15,
	//     fast-mode-2026-02-01, and token-efficient-tools-2026-03-28 from the
	//     wire any time a client sent its own Anthropic-Beta — confirmed by
	//     inspecting docker/logs/cliproxy/cursor-1.0 upstream traces.
	//
	// Betas confirmed in 2026-05-29 Opus 4.8 Proxyman capture (claude-cli/2.1.156):
	//   claude-code-20250219, oauth-2025-04-20, context-1m-2025-08-07,
	//   interleaved-thinking-2025-05-14, redact-thinking-2026-02-12,
	//   thinking-token-count-2026-05-13, context-management-2025-06-27,
	//   prompt-caching-scope-2026-01-05, mid-conversation-system-2026-04-07,
	//   advisor-tool-2026-03-01, effort-2025-11-24, extended-cache-ttl-2025-04-11,
	//   cache-diagnosis-2026-04-07.
	//
	// We deliberately DROP redact-thinking-2026-02-12: it enables Anthropic's
	// encrypted `redacted_thinking` content blocks (hidden from user). Without
	// it Anthropic emits plain `thinking` blocks — more visible reasoning, no
	// encrypted/hidden segments. This is a fork policy choice, not parity gap.
	//
	// We deliberately DROP advisor-tool-2026-03-01: it gates a Claude Code
	// product feature whose semantics we have not characterized. Adding it
	// without understanding it risks unintended server-side behavior. Revisit
	// if/when we explicitly want that feature.
	//
	// We keep context-management-2025-06-27 even though we don't yet inject
	// the matching context_management.edits[] field. The beta header alone
	// is a server-side no-op — it just gates the schema. Keeping it on costs
	// nothing and lets us turn on server-side clear_thinking/clear_tool_uses
	// edits later without a header dance.
	//
	// Extras kept from upstream's curated cross-client safe set: structured-outputs-2025-12-15,
	// fast-mode-2026-02-01, token-efficient-tools-2026-03-28. These don't appear
	// in Claude Code but are additive Anthropic features; harmless to send.
	baseBetas := "claude-code-20250219,oauth-2025-04-20,context-1m-2025-08-07,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,effort-2025-11-24,extended-cache-ttl-2025-04-11,cache-diagnosis-2026-04-07,structured-outputs-2025-12-15,fast-mode-2026-02-01,token-efficient-tools-2026-03-28"
	_ = ginHeaders // client Anthropic-Beta header intentionally ignored; see comment above

	hasClaude1MHeader := false
	if ginHeaders != nil {
		if strings.TrimSpace(ginHeaders.Get(textproto.CanonicalMIMEHeaderKey("X-CPA-CLAUDE-1M"))) != "" {
			hasClaude1MHeader = true
		}
	}
	if !hasClaude1MHeader && auth != nil && auth.Attributes != nil {
		if auth.Attributes["gitlab_duo_force_context_1m"] == "true" {
			hasClaude1MHeader = true
		}
	}

	// Merge extra betas from request body and request flags.
	//
	// Filter: body-extracted betas pass through ONLY if they match the
	// canonical Anthropic beta naming pattern AND are not in a known
	// non-Anthropic / client-invented allowlist. The current explicit
	// reject is `max-effort-2026-01-24`, a Cursor-internal opt-in that
	// Claude Code itself never sends. Forwarding it makes our traffic
	// look less like Claude Code without measurable upside.
	if len(extraBetas) > 0 || hasClaude1MHeader {
		existingSet := make(map[string]bool)
		for _, b := range strings.Split(baseBetas, ",") {
			betaName := strings.TrimSpace(b)
			if betaName != "" {
				existingSet[betaName] = true
			}
		}
		for _, beta := range extraBetas {
			beta = strings.TrimSpace(beta)
			if beta == "" || existingSet[beta] {
				continue
			}
			if isClientInventedBeta(beta) {
				continue
			}
			baseBetas += "," + beta
			existingSet[beta] = true
		}
		if hasClaude1MHeader && !existingSet["context-1m-2025-08-07"] {
			baseBetas += ",context-1m-2025-08-07"
		}
	}
	r.Header.Set("Anthropic-Beta", baseBetas)

	misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Version", "2023-06-01")
	// Only set browser access header for API key mode; real Claude Code CLI does not send it.
	if useAPIKey {
		misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-App", "cli")
	// Values below match Claude Code 2.1.63 / @anthropic-ai/sdk 0.74.0 (updated 2026-02-28).
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Retry-Count", "0")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime", "node")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Lang", "js")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Timeout", hdrDefault(hd.Timeout, "600"))
	// Session ID: stable per auth/apiKey, matches Claude Code's X-Claude-Code-Session-Id header.
	sessionID, errSessionID := helps.CachedSessionIDRequired(r.Context(), apiKey)
	if errSessionID != nil {
		return errSessionID
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-Claude-Code-Session-Id", sessionID)
	// Per-request UUID, matches Claude Code's x-client-request-id for first-party API.
	if isAnthropicBase {
		misc.EnsureHeader(r.Header, ginHeaders, "x-client-request-id", uuid.New().String())
	}
	r.Header.Set("Connection", "keep-alive")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		// SSE streams must not be compressed: the downstream scanner reads
		// line-delimited text and cannot parse compressed bytes.  Using
		// "identity" tells the upstream to send an uncompressed stream.
		r.Header.Set("Accept-Encoding", "identity")
	} else {
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	// Legacy mode keeps OS/Arch runtime-derived; stabilized mode pins OS/Arch
	// to the configured baseline while still allowing newer official
	// User-Agent/package/runtime tuples to upgrade the software fingerprint.
	if stabilizeDeviceProfile {
		helps.ApplyClaudeDeviceProfileHeaders(r, deviceProfile)
	} else {
		helps.ApplyClaudeLegacyDeviceHeaders(r, ginHeaders, cfg)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	// Re-enforce Accept-Encoding: identity after ApplyCustomHeadersFromAttrs, which
	// may override it with a user-configured value.  Compressed SSE breaks the line
	// scanner regardless of user preference, so this is non-negotiable for streams.
	if stream {
		r.Header.Set("Accept-Encoding", "identity")
	}
	return nil
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func checkSystemInstructions(payload []byte) []byte {
	// CountTokens path: no cfg in scope, so global-scope flag stays off here.
	// (Token counting doesn't affect cache anchoring on the real /v1/messages
	// path, which has cfg via applyCloaking.)
	return checkSystemInstructionsWithSigningMode(payload, false, false, false, "2.1.63", "", "", false)
}

func rebuildMidSystemMessagesToTopLevel(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var movedSystemParts []string
	keptMessages := make([]string, 0, int(messages.Get("#").Int()))
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			movedSystemParts = append(movedSystemParts, claudeSystemTextParts(message.Get("content"))...)
			return true
		}
		keptMessages = append(keptMessages, message.Raw)
		return true
	})
	if len(movedSystemParts) == 0 {
		return payload
	}

	systemParts := claudeSystemTextParts(gjson.GetBytes(payload, "system"))
	systemParts = append(systemParts, movedSystemParts...)
	if len(systemParts) > 0 {
		if updated, errSetSystem := sjson.SetRawBytes(payload, "system", rawJSONArray(systemParts)); errSetSystem == nil {
			payload = updated
		}
	}
	if updated, errSetMessages := sjson.SetRawBytes(payload, "messages", rawJSONArray(keptMessages)); errSetMessages == nil {
		payload = updated
	}
	return payload
}

func claudeSystemTextParts(content gjson.Result) []string {
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return []string{string(block)}
	}
	if !content.IsArray() {
		return nil
	}

	var parts []string
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			text := item.String()
			if strings.TrimSpace(text) != "" {
				block := []byte(`{"type":"text","text":""}`)
				block, _ = sjson.SetBytes(block, "text", text)
				parts = append(parts, string(block))
			}
			return true
		}
		if item.IsObject() && item.Get("type").String() == "text" && strings.TrimSpace(item.Get("text").String()) != "" {
			parts = append(parts, item.Raw)
		}
		return true
	})
	return parts
}

func rawJSONArray(items []string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return []byte(builder.String())
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

// prepareClaudeOAuthToolNamesForUpstream applies the Claude OAuth tool-name
// transforms in the same order across request paths. Remap runs before prefixing
// so any future non-empty prefix still composes correctly with the per-request
// reverse map.
func prepareClaudeOAuthToolNamesForUpstream(body []byte, prefix string, prefixDisabled bool) ([]byte, map[string]string) {
	body, reverseMap := remapOAuthToolNames(body)
	if !prefixDisabled {
		body = helps.ApplyClaudeToolPrefix(body, prefix)
	}
	return body, reverseMap
}

// restoreClaudeOAuthToolNamesFromResponse undoes the Claude OAuth tool-name
// transforms for non-stream responses in reverse order.
func restoreClaudeOAuthToolNamesFromResponse(body []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) []byte {
	if !prefixDisabled {
		body = helps.StripClaudeToolPrefixFromResponse(body, prefix)
	}
	return reverseRemapOAuthToolNames(body, reverseMap)
}

// restoreClaudeOAuthToolNamesFromStreamLine undoes the Claude OAuth tool-name
// transforms for SSE lines in reverse order.
func restoreClaudeOAuthToolNamesFromStreamLine(line []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) []byte {
	if !prefixDisabled {
		line = helps.StripClaudeToolPrefixFromStreamLine(line, prefix)
	}
	return reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
}

// remapOAuthToolNames renames third-party tool names to Claude Code equivalents
// and removes tools without an official counterpart. This prevents Anthropic from
// fingerprinting the request as a third-party client via tool naming patterns.
//
// It operates on: tools[].name, tool_choice.name, and all tool_use/tool_reference
// references in messages. Removed tools' corresponding tool_result blocks are preserved
// (they just become orphaned, which is safe for Claude).
//
// The returned map is keyed on the upstream (TitleCase) name and maps to the
// client-supplied original name. Callers MUST pass this map to the reverse
// functions so only names the client actually caused us to rewrite are restored
// on the response. A global reverse map (the previous implementation) incorrectly
// rewrote names the client originally sent in TitleCase (e.g. `Bash`)
// when any OTHER tool in the same request triggered a forward rename (e.g.
// `glob` -> `Glob`), because the global reverse map contained `Bash` -> `bash`
// regardless of what the client originally sent.
func remapOAuthToolNames(body []byte) ([]byte, map[string]string) {
	reverseMap := make(map[string]string, len(oauthToolRenameMap))
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// 1. Rewrite tools array in a single pass (if present).
	// IMPORTANT: do not mutate names first and then rebuild from an older gjson
	// snapshot. gjson results are snapshots of the original bytes; rebuilding from a
	// stale snapshot will preserve removals but overwrite renamed names back to their
	// original lowercase values.
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.IsArray() {

		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			// Keep Anthropic built-in tools (web_search, code_execution, etc.) unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(tool.Raw)
				toolCount++
				return true
			}

			name := tool.Get("name").String()
			if oauthToolsToRemove[name] {
				return true
			}

			toolJSON := tool.Raw
			if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
				updatedTool, err := sjson.Set(toolJSON, "name", newName)
				if err == nil {
					toolJSON = updatedTool
					recordRename(name, newName)
				}
			}

			if toolCount > 0 {
				toolsJSON.WriteByte(',')
			}
			toolsJSON.WriteString(toolJSON)
			toolCount++
			return true
		})
		toolsJSON.WriteByte(']')
		body, _ = sjson.SetRawBytes(body, "tools", []byte(toolsJSON.String()))
	}

	// 2. Rename tool_choice if it references a known tool
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	if toolChoiceType == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if oauthToolsToRemove[tcName] {
			// The chosen tool was removed from the tools array, so drop tool_choice to
			// keep the payload internally consistent and fall back to normal auto tool use.
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		} else if newName, ok := oauthToolRenameMap[tcName]; ok && newName != tcName {
			body, _ = sjson.SetBytes(body, "tool_choice.name", newName)
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
						path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(name, newName)
					}
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if newName, ok := oauthToolRenameMap[toolName]; ok && newName != toolName {
						path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(toolName, newName)
					}
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					toolID := part.Get("tool_use_id").String()
					_ = toolID // tool_use_id stays as-is
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if newName, ok := oauthToolRenameMap[nestedToolName]; ok && newName != nestedToolName {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, newName)
									recordRename(nestedToolName, newName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body, reverseMap
}

// reverseRemapOAuthToolNames reverses the tool name mapping for non-stream responses
// using the per-request map produced by remapOAuthToolNames. Names the client sent
// that were NOT forward-renamed are passed through unchanged.
func reverseRemapOAuthToolNames(body []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if origName, ok := reverseMap[name]; ok {
				path := fmt.Sprintf("content.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if origName, ok := reverseMap[toolName]; ok {
				path := fmt.Sprintf("content.%d.tool_name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		}
		return true
	})
	return body
}

// reverseRemapOAuthToolNamesFromStreamLine reverses the tool name mapping for SSE
// stream lines, using the per-request reverseMap produced by remapOAuthToolNames.
func reverseRemapOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}

	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if origName, ok := reverseMap[name]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.name", origName)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if origName, ok := reverseMap[toolName]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.tool_name", origName)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

<<<<<<< HEAD
=======
func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	// Collect built-in tool names from the authoritative fallback seed list and
	// augment it with any typed built-ins present in the current request body.
	builtinTools := helps.AugmentClaudeBuiltinToolRegistry(body, nil)

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			// Skip built-in tools (web_search, code_execution, etc.) which have
			// a "type" field and require their name to remain unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if !strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if !strings.HasPrefix(toolName, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.tool_name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
		case "tool_result":
			// Handle nested tool_reference blocks inside tool_result.content[]
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if strings.HasPrefix(nestedToolName, prefix) {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
							body, _ = sjson.SetBytes(body, nestedPath, strings.TrimPrefix(nestedToolName, prefix))
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

// getClientUserAgent extracts the client User-Agent from the gin context.
func getClientUserAgent(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.GetHeader("User-Agent")
	}
	return ""
}

// parseEntrypointFromUA extracts the entrypoint from a Claude Code User-Agent.
// Format: "claude-cli/x.y.z (external, cli)" → "cli"
// Format: "claude-cli/x.y.z (external, vscode)" → "vscode"
// Returns "cli" if parsing fails or UA is not Claude Code.
func parseEntrypointFromUA(userAgent string) string {
	// Find content inside parentheses
	start := strings.Index(userAgent, "(")
	end := strings.LastIndex(userAgent, ")")
	if start < 0 || end <= start {
		return "cli"
	}
	inner := userAgent[start+1 : end]
	// Split by comma, take the second part (entrypoint is at index 1, after USER_TYPE)
	// Format: "(USER_TYPE, ENTRYPOINT[, extra...])"
	parts := strings.Split(inner, ",")
	if len(parts) >= 2 {
		ep := strings.TrimSpace(parts[1])
		if ep != "" {
			return ep
		}
	}
	return "cli"
}

// getWorkloadFromContext extracts workload identifier from the gin request headers.
func getWorkloadFromContext(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return strings.TrimSpace(ginCtx.GetHeader("X-CPA-Claude-Workload"))
	}
	return ""
}

// getCloakConfigFromAuth extracts cloak configuration from the auth's attributes,
// falling back to its stored metadata (the raw OAuth/token JSON). Returns
// (cloakMode, strictMode, sensitiveWords, cacheUserID); an empty cloakMode means
// the credential did not explicitly configure a mode.
func getCloakConfigFromAuth(auth *cliproxyauth.Auth) (cloakMode string, strictMode bool, sensitiveWords []string, cacheUserID bool) {
	if auth == nil {
		return "", false, nil, false
	}

	// lookupCloakAttr prefers the executor-facing Attributes, then falls back to the
	// raw metadata blob (e.g. the OAuth/token JSON) so file-based credentials can
	// carry cloak settings without a matching claude-api-key config entry.
	lookupCloakAttr := func(key string) string {
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
		if auth.Metadata != nil {
			if value, ok := auth.Metadata[key].(string); ok {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}

	// An empty cloakMode means this credential did not explicitly configure a mode,
	// allowing the caller to fall back to the global/default behavior.
	cloakMode = lookupCloakAttr("cloak_mode")

	strictMode = strings.EqualFold(lookupCloakAttr("cloak_strict_mode"), "true")

	if wordsStr := lookupCloakAttr("cloak_sensitive_words"); wordsStr != "" {
		sensitiveWords = strings.Split(wordsStr, ",")
		for i := range sensitiveWords {
			sensitiveWords[i] = strings.TrimSpace(sensitiveWords[i])
		}
	}

	cacheUserID = strings.EqualFold(lookupCloakAttr("cloak_cache_user_id"), "true")

	return cloakMode, strictMode, sensitiveWords, cacheUserID
}

// injectFakeUserID generates and injects a fake user ID into the request metadata.
// When useCache is false, a new user ID is generated for every call.
func injectFakeUserID(ctx context.Context, payload []byte, apiKey string, useCache bool) ([]byte, error) {
	generateID := func() (string, error) {
		if useCache {
			return helps.CachedUserIDRequired(ctx, apiKey)
		}
		return helps.GenerateFakeUserID(), nil
	}

	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		userID, errUserID := generateID()
		if errUserID != nil {
			return nil, errUserID
		}
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", userID)
		return payload, nil
	}

	existingUserID := gjson.GetBytes(payload, "metadata.user_id").String()
	if existingUserID == "" || !helps.IsValidUserID(existingUserID) {
		userID, errUserID := generateID()
		if errUserID != nil {
			return nil, errUserID
		}
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", userID)
	}
	return payload, nil
}

// fingerprintSalt is the salt used by Claude Code to compute the 3-char build fingerprint.
const fingerprintSalt = "59cf53e54c78"

// computeFingerprint computes the 3-char build fingerprint that Claude Code embeds in cc_version.
// Algorithm: SHA256(salt + messageText[4] + messageText[7] + messageText[20] + version)[:3]
func computeFingerprint(messageText, version string) string {
	indices := [3]int{4, 7, 20}
	runes := []rune(messageText)
	var sb strings.Builder
	for _, idx := range indices {
		if idx < len(runes) {
			sb.WriteRune(runes[idx])
		} else {
			sb.WriteRune('0')
		}
	}
	input := fingerprintSalt + sb.String() + version
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])[:3]
}

// generateBillingHeader creates the x-anthropic-billing-header text block that
// real Claude Code prepends to every system prompt array.
// Format: x-anthropic-billing-header: cc_version=<ver>.<build>; cc_entrypoint=<ep>; cch=<hash>; [cc_workload=<wl>;]
func generateBillingHeader(payload []byte, experimentalCCHSigning bool, version, messageText, entrypoint, workload string) string {
	if entrypoint == "" {
		entrypoint = "cli"
	}
	buildHash := computeFingerprint(messageText, version)
	workloadPart := ""
	if workload != "" {
		workloadPart = fmt.Sprintf(" cc_workload=%s;", workload)
	}

	if experimentalCCHSigning {
		return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=00000;%s", version, buildHash, entrypoint, workloadPart)
	}

	// Generate a deterministic cch hash from the payload content (system + messages + tools).
	h := sha256.Sum256(payload)
	cch := hex.EncodeToString(h[:])[:5]
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=%s;%s", version, buildHash, entrypoint, cch, workloadPart)
}

>>>>>>> upstream/main
func checkSystemInstructionsWithMode(payload []byte, strictMode bool) []byte {
	return checkSystemInstructionsWithSigningMode(payload, strictMode, false, false, "2.1.63", "", "", false)
}

// checkSystemInstructionsWithSigningMode injects Claude Code-style system blocks:
//
//	system[0]: billing header (no cache_control)
//	system[1]: agent identifier (cache_control ephemeral, scope=org)
//	system[2]: core intro prompt (cache_control ephemeral, scope=global)
//	system[3]: system instructions (no cache_control)
//	system[4]: doing tasks (no cache_control)
//	system[5]: user system messages moved to first user message
func checkSystemInstructionsWithSigningMode(payload []byte, strictMode bool, experimentalCCHSigning bool, oauthMode bool, version, entrypoint, workload string, useGlobalScopeOnLast bool) []byte {
	system := gjson.GetBytes(payload, "system")

	// Extract original message text for fingerprint computation (before billing injection).
	// Use the first system text block's content as the fingerprint source.
	messageText := ""
	if system.IsArray() {
		system.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				messageText = part.Get("text").String()
				return false
			}
			return true
		})
	} else if system.Type == gjson.String {
		messageText = system.String()
	}

	firstText := gjson.GetBytes(payload, "system.0.text").String()
	if strings.HasPrefix(firstText, "x-anthropic-billing-header:") {
		return payload
	}

	effectiveCCHSigning := experimentalCCHSigning || !strictMode
	billingText := helps.GenerateClaudeBillingHeader(payload, effectiveCCHSigning, version, messageText, entrypoint, workload)
	billingBlock := helps.BuildClaudeTextBlock(billingText, nil)
	if !strictMode {
		cacheTTL := "1h"
		if cursorSystemResult, ok := helps.RewriteCursorSystemPromptBlocks(system, billingBlock, cacheTTL, useGlobalScopeOnLast); ok {
			payload, _ = sjson.SetRawBytes(payload, "system", []byte(cursorSystemResult))
			return payload
		}
	}

	// Cache anchor layout MIRRORS the canonical Claude Code 2.1.156 Opus 4.8
	// Proxyman capture, verified 2026-05-29:
	//   system[0] billing header   — NO cache_control
	//   system[1] agentBlock        — NO cache_control
	//   system[2] staticBlock LAST  — cache_control { ttl=1h [+ scope:global] }
	//
	// Why no cache_control on system[0] / system[1]:
	// Anthropic's prefix-chain rule for scope:"global" requires the
	// scope:"global" block to be the FIRST cache_control breakpoint in the
	// (tools → system → messages) chain. If we put a bare-scope cache_control
	// on system[0] (billing) the validator says: "A block with scope:'global'
	// was found after content with a narrower cache scope." Confirmed live
	// 2026-05-29 against Opus 4.8 (Anthropic's prefix-chain validator).
	//
	// Claude Code achieves this by leaving billing + agent intro
	// cache-control-free and ONLY anchoring at the last system block. We do
	// the same. The trade is one breakpoint (down from three), but that one
	// breakpoint at scope:"global" actually shares the cache prefix across
	// the main agent ↔ subagent boundary, which the previous three-bare
	// breakpoints never did.
	//
	// useGlobalScopeOnLast (cfg.ClaudeCursorGlobalCacheScope) decides whether
	// to tag the lone anchor with scope:"global". Off = bare ttl (1 anchor,
	// no global sharing). On = ttl + scope:"global" (Claude Code parity).
	agentBlock := helps.BuildClaudeTextBlock("You are Claude Code, Anthropic's official CLI for Claude.", nil)
	staticPrompt := strings.Join([]string{
		helps.ClaudeCodeIntro,
		helps.ClaudeCodeSystem,
		helps.ClaudeCodeDoingTasks,
		helps.ClaudeCodeToneAndStyle,
		helps.ClaudeCodeOutputEfficiency,
	}, "\n\n")
	lastCache := map[string]string{"ttl": "1h"}
	if useGlobalScopeOnLast {
		lastCache["scope"] = "global"
	}
	staticBlock := helps.BuildClaudeTextBlock(staticPrompt, lastCache)

	systemResult := "[" + billingBlock + "," + agentBlock + "," + staticBlock + "]"
	payload, _ = sjson.SetRawBytes(payload, "system", []byte(systemResult))

	// Collect user system instructions and prepend to first user message
	if !strictMode {
		var userSystemParts []string
		if system.IsArray() {
			system.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					txt := strings.TrimSpace(part.Get("text").String())
					if txt != "" {
						userSystemParts = append(userSystemParts, txt)
					}
				}
				return true
			})
		} else if system.Type == gjson.String && strings.TrimSpace(system.String()) != "" {
			userSystemParts = append(userSystemParts, strings.TrimSpace(system.String()))
		}

		if len(userSystemParts) > 0 {
			combined := strings.Join(userSystemParts, "\n\n")
			if oauthMode {
				combined = helps.SanitizeForwardedSystemPrompt(combined)
			}
			if strings.TrimSpace(combined) != "" {
				payload = helps.PrependClaudeToFirstUserMessage(payload, combined)
			}
		}
	}

	return payload
}

// sanitizeForwardedSystemPrompt reduces forwarded third-party system context to a
// tiny neutral reminder for Claude OAuth cloaking. The goal is to preserve only
// the minimum tool/task guidance while removing virtually all client-specific
// prompt structure that Anthropic may classify as third-party agent traffic.

// buildTextBlock constructs a JSON text block object with proper escaping.
// Uses sjson.SetBytes to handle multi-line text, quotes, and control characters.
// cacheControl is optional; pass nil to omit cache_control.
<<<<<<< HEAD
func applyCloaking(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte, model string, apiKey string) []byte {
	clientUserAgent := helps.GetClaudeClientUserAgent(ctx)
=======
func buildTextBlock(text string, cacheControl map[string]string) string {
	block := []byte(`{"type":"text"}`)
	block, _ = sjson.SetBytes(block, "text", text)
	if cacheControl != nil && len(cacheControl) > 0 {
		// Build cache_control JSON manually to avoid sjson map marshaling issues.
		// sjson.SetBytes with map[string]string may not produce expected structure.
		cc := `{"type":"ephemeral"`
		if t, ok := cacheControl["ttl"]; ok {
			cc += fmt.Sprintf(`,"ttl":"%s"`, t)
		}
		cc += "}"
		block, _ = sjson.SetRawBytes(block, "cache_control", []byte(cc))
	}
	return string(block)
}

// prependToFirstUserMessage prepends text content to the first user message.
// This avoids putting non-Claude-Code system instructions in system[] which
// triggers Anthropic's extra usage billing for OAuth-proxied requests.
func prependToFirstUserMessage(payload []byte, text string) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Find the first user message index
	firstUserIdx := -1
	messages.ForEach(func(idx, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			firstUserIdx = int(idx.Int())
			return false
		}
		return true
	})

	if firstUserIdx < 0 {
		return payload
	}

	prefixBlock := fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)

	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		newBlock := fmt.Sprintf(`{"type":"text","text":%q}`, prefixBlock)
		var newArray string
		if content.Raw == "[]" || content.Raw == "" {
			newArray = "[" + newBlock + "]"
		} else {
			newArray = "[" + newBlock + "," + content.Raw[1:]
		}
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte(newArray))
	} else if content.Type == gjson.String {
		newText := prefixBlock + content.String()
		payload, _ = sjson.SetBytes(payload, contentPath, newText)
	}

	return payload
}

// applyCloaking applies cloaking transformations to the payload based on config and client.
// Cloaking includes: system prompt injection, fake user ID, and sensitive word obfuscation.
func applyCloaking(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte, model string, apiKey string) ([]byte, error) {
	clientUserAgent := getClientUserAgent(ctx)
>>>>>>> upstream/main
	// Enable cch signing for OAuth tokens by default (not just experimental flag).
	oauthToken := isClaudeOAuthToken(apiKey)
	useCCHSigning := oauthToken || experimentalCCHSigningEnabled(cfg, auth)

	// Get cloak config from ClaudeKey configuration
	cloakCfg := resolveClaudeKeyCloakConfig(cfg, auth)
	attrMode, attrStrict, attrWords, attrCache := helps.GetClaudeCloakConfigFromAuth(auth)

	// Determine cloak settings. Precedence (low -> high):
	//   built-in "auto" default
	//   -> global disable-claude-cloak-mode switch (forces "never")
	//   -> per-credential settings from auth attributes/metadata
	//   -> per claude-api-key cloak config
	cloakMode := "auto"
	if cfg != nil && cfg.DisableClaudeCloakMode {
		cloakMode = "never"
	}
	strictMode := attrStrict
	sensitiveWords := attrWords
	cacheUserID := attrCache

	if attrMode != "" {
		cloakMode = attrMode
	}

	if cloakCfg != nil {
		if mode := strings.TrimSpace(cloakCfg.Mode); mode != "" {
			cloakMode = mode
		}
		if cloakCfg.StrictMode {
			strictMode = true
		}
		if len(cloakCfg.SensitiveWords) > 0 {
			sensitiveWords = cloakCfg.SensitiveWords
		}
		if cloakCfg.CacheUserID != nil {
			cacheUserID = *cloakCfg.CacheUserID
		}
	}

	// Determine if cloaking should be applied
	if !helps.ShouldCloak(cloakMode, clientUserAgent) {
		return payload, nil
	}

	// Skip system instructions for claude-3-5-haiku models
	if !strings.HasPrefix(model, "claude-3-5-haiku") {
		billingVersion := helps.DefaultClaudeVersion(cfg)
		entrypoint := helps.ParseClaudeEntrypointFromUA(clientUserAgent)
		workload := helps.GetClaudeWorkloadFromContext(ctx)
		useGlobalScopeOnLast := cfg != nil && cfg.ClaudeCursorGlobalCacheScope
		payload = checkSystemInstructionsWithSigningMode(payload, strictMode, useCCHSigning, oauthToken, billingVersion, entrypoint, workload, useGlobalScopeOnLast)
	}

	// Inject fake user ID
<<<<<<< HEAD
	payload = helps.InjectClaudeFakeUserID(payload, apiKey, cacheUserID)
=======
	var errFakeUserID error
	payload, errFakeUserID = injectFakeUserID(ctx, payload, apiKey, cacheUserID)
	if errFakeUserID != nil {
		return nil, errFakeUserID
	}
>>>>>>> upstream/main

	// Apply sensitive word obfuscation
	if len(sensitiveWords) > 0 {
		matcher := helps.BuildSensitiveWordMatcher(sensitiveWords)
		payload = helps.ObfuscateSensitiveWords(payload, matcher)
	}

	return payload, nil
}

// ensureCacheControl injects cache_control breakpoints into the payload for optimal prompt caching.
// According to Anthropic's documentation, cache prefixes are created in order: tools -> system -> messages.
// This function adds cache_control to:
// 1. The LAST tool in the tools array (caches all tool definitions)
// 2. The LAST system prompt element
// 3. The SECOND-TO-LAST user turn (caches conversation history for multi-turn)
//
// Up to 4 cache breakpoints are allowed per request. Tools, System, and Messages are INDEPENDENT breakpoints.
// This enables up to 90% cost reduction on cached tokens (cache read = 0.1x base price).
// See: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
func ensureModelMaxTokens(body []byte, modelID string) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	if maxTokens := gjson.GetBytes(body, "max_tokens"); maxTokens.Exists() {
		return body
	}

	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(strings.TrimSpace(modelID)) {
		if strings.EqualFold(provider, "claude") {
			maxTokens := defaultModelMaxTokens
			if info := registry.GetGlobalRegistry().GetModelInfo(strings.TrimSpace(modelID), "claude"); info != nil && info.MaxCompletionTokens > 0 {
				maxTokens = info.MaxCompletionTokens
			}
			body, _ = sjson.SetBytes(body, "max_tokens", maxTokens)
			return body
		}
	}

	return body
}

// logClaudeUsageFromLine extracts the JSON payload from a Claude SSE line and
// logs cache creation/read counters when present.
func logClaudeUsageFromLine(model string, line []byte) {
	payload := helps.JSONPayload(line)
	if len(payload) == 0 {
		return
	}
	usageNode := gjson.GetBytes(payload, "message.usage")
	if !usageNode.Exists() {
		usageNode = gjson.GetBytes(payload, "usage")
	}
	if !usageNode.Exists() {
		return
	}
	helps.LogClaudeCacheUsage(model, usageNode)
}

// parseClaudeRetryAfter extracts a retry-after duration from an Anthropic 429
// response so the conductor can cool the credential until its real reset window
// (e.g. the 5-hour subscription cap) instead of falling back to the generic
// 1s..30m quota backoff. Sources, in priority order:
//  1. The standard HTTP "Retry-After" header (delta seconds or HTTP-date).
//  2. Anthropic's "anthropic-ratelimit-unified-reset" header (epoch seconds).
//  3. The error body's "error.resets_at" (epoch) / "error.resets_in_seconds".
//
// Returns nil when the status is not 429 or no hint is present, in which case
// the conductor's default backoff applies.
func parseClaudeRetryAfter(statusCode int, header http.Header, body []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests {
		return nil
	}
	if header != nil {
		if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
			if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
				d := time.Duration(secs) * time.Second
				return &d
			}
			if when, err := http.ParseTime(raw); err == nil {
				if d := when.Sub(now); d > 0 {
					return &d
				}
			}
		}
		for _, key := range []string{"Anthropic-Ratelimit-Unified-Reset", "Anthropic-Ratelimit-Unified-5h-Reset"} {
			if raw := strings.TrimSpace(header.Get(key)); raw != "" {
				if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil && epoch > 0 {
					if d := time.Unix(epoch, 0).Sub(now); d > 0 {
						return &d
					}
				}
			}
		}
	}
	if len(body) > 0 {
		if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
			if d := time.Unix(resetsAt, 0).Sub(now); d > 0 {
				return &d
			}
		}
		if resetsIn := gjson.GetBytes(body, "error.resets_in_seconds").Int(); resetsIn > 0 {
			d := time.Duration(resetsIn) * time.Second
			return &d
		}
	}
	return nil
}

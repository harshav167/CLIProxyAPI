package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	kimiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	kimithinking "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/kimi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// KimiExecutor is a stateless executor for Kimi API using OpenAI-compatible chat completions.
type KimiExecutor struct {
	ClaudeExecutor
	cfg *config.Config
}

// NewKimiExecutor creates a new Kimi executor.
func NewKimiExecutor(cfg *config.Config) *KimiExecutor { return &KimiExecutor{cfg: cfg} }

// Identifier returns the executor identifier.
func (e *KimiExecutor) Identifier() string { return "kimi" }

// PrepareRequest injects Kimi credentials into the outgoing HTTP request.
func (e *KimiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := kimiCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Kimi credentials into the request and executes it.
func (e *KimiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kimi executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming chat completion request to Kimi.
func (e *KimiExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
		return e.ClaudeExecutor.Execute(ctx, auth, req, opts)
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token := kimiCreds(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)

	body, err = sjson.SetBytes(body, "model", strings.TrimSpace(baseModel))
	if err != nil {
		return resp, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "kimi", e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, err = kimithinking.EnforceModelWireThinking(body, baseModel)
	if err != nil {
		return resp, err
	}
	body, err = normalizeKimiToolMessageLinks(body)
	if err != nil {
		return resp, err
	}
	body = normalizeKimiToolSchemaRefs(body)
	body = ensureKimiPromptCacheKey(body)
	reporter.SetTranslatedReasoningEffort(body, e.Identifier())

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyKimiHeadersWithAuth(httpReq, token, false, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
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

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to Kimi.
func (e *KimiExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
		return e.ClaudeExecutor.ExecuteStream(ctx, auth, req, opts)
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token := kimiCreds(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	body, err = sjson.SetBytes(body, "model", strings.TrimSpace(baseModel))
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "kimi", e.Identifier())
	if err != nil {
		return nil, err
	}

	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set stream_options in payload: %w", err)
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, err = kimithinking.EnforceModelWireThinking(body, baseModel)
	if err != nil {
		return nil, err
	}
	body, err = normalizeKimiToolMessageLinks(body)
	if err != nil {
		return nil, err
	}
	body = normalizeKimiToolSchemaRefs(body)
	body = ensureKimiPromptCacheKey(body)
	reporter.SetTranslatedReasoningEffort(body, e.Identifier())

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyKimiHeadersWithAuth(httpReq, token, true, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
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

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kimi executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		doneChunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
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
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for Kimi requests.
func (e *KimiExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
	return e.ClaudeExecutor.CountTokens(ctx, auth, req, opts)
}

// normalizeKimiToolSchemaRefs rewrites JSON-Schema $ref usages in tool
// parameter schemas that Kimi/Moonshot rejects.
//
// Moonshot's schema validator ONLY accepts $ref values that start with
// "#/$defs/". Cursor emits sibling/self references such as
//
//	"final_summary": {"$ref": "#/properties/current_step", "description": "..."}
//
// (reusing another property's type and overriding just the description). Kimi
// 422s the entire request with:
//
//	tools.function.parameters is not a valid moonshot flavored json schema,
//	details: <At path 'properties.final_summary.$ref': references must start
//	with #/$defs/>
//
// which makes every tool call in that request fail (observed in prod when
// subagents were enabled: the UpdateCurrentStep tool carries these refs and no
// $defs block at all).
//
// The fix is to INLINE-resolve any $ref that does not point under #/$defs/:
// replace the ref-bearing object with a copy of the referenced subschema, then
// re-apply the object's own sibling keys (e.g. description) so local overrides
// win. This yields a semantically identical, ref-free schema Moonshot accepts.
// Refs already under #/$defs/ are left untouched. No-op for non-tool traffic.
func normalizeKimiToolSchemaRefs(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	out := body
	idx := -1
	tools.ForEach(func(_, tool gjson.Result) bool {
		idx++
		params := tool.Get("function.parameters")
		if !params.Exists() || !params.IsObject() {
			return true
		}
		if !strings.Contains(params.Raw, `"$ref"`) {
			return true
		}
		var root map[string]any
		if err := json.Unmarshal([]byte(params.Raw), &root); err != nil {
			return true
		}
		if !inlineKimiSchemaRefs(root, root, 0) {
			return true
		}
		rewritten, err := json.Marshal(root)
		if err != nil {
			return true
		}
		if updated, errSet := sjson.SetRawBytes(out, fmt.Sprintf("tools.%d.function.parameters", idx), rewritten); errSet == nil {
			out = updated
		}
		return true
	})
	return out
}

// inlineKimiSchemaRefs walks a parsed JSON-schema node, replacing every $ref
// that does not start with "#/$defs/" with the subschema it points at (resolved
// against root), preserving the ref object's sibling keys as overrides. Returns
// whether it changed anything. Bounded recursion depth guards against ref
// cycles and pathological nesting.
func inlineKimiSchemaRefs(node any, root map[string]any, depth int) bool {
	if depth > 64 {
		return false
	}
	changed := false
	switch typed := node.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && ref != "" && !strings.HasPrefix(ref, "#/$defs/") {
			if resolved, ok := resolveKimiJSONPointer(root, ref); ok {
				// Start from a copy of the resolved target, then overlay the
				// ref object's own keys (except $ref) so local overrides win.
				for k := range typed {
					if k == "$ref" {
						continue
					}
					resolved[k] = typed[k]
				}
				// Replace node contents in place: clear then copy merged result.
				for k := range typed {
					delete(typed, k)
				}
				for k, v := range resolved {
					typed[k] = v
				}
				changed = true
			}
		}
		for _, v := range typed {
			if inlineKimiSchemaRefs(v, root, depth+1) {
				changed = true
			}
		}
	case []any:
		for _, v := range typed {
			if inlineKimiSchemaRefs(v, root, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

// resolveKimiJSONPointer resolves a local JSON pointer like
// "#/properties/current_step" against root and returns a deep copy of the
// target object. Only fragment ("#/...") pointers are supported. Returns
// (copy, true) on success.
func resolveKimiJSONPointer(root map[string]any, ref string) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var cur any = root
	for _, rawTok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok := strings.ReplaceAll(strings.ReplaceAll(rawTok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		next, exists := m[tok]
		if !exists {
			return nil, false
		}
		cur = next
	}
	target, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	copied, ok := deepCopyJSONMap(target)
	if !ok {
		return nil, false
	}
	return copied, true
}

func deepCopyJSONMap(m map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}

// ensureKimiPromptCacheKey injects a deterministic prompt_cache_key derived from
// the conversation's stable prefix (system prompt + first user message) when the
// caller has not supplied one.
//
// Kimi K2.* uses automatic server-side prefix caching, so cache hits already
// occur without this field. But per Kimi's docs, prompt_cache_key is a
// scheduling hint: requests sharing the same key are sticky-routed to the same
// cache cluster, which keeps the prefix-cache hit rate high under load /
// cluster rebalancing. Cursor does not send a session id, so we synthesise a
// stable key from content that does not change across turns of one conversation
// (the system prompt and the first user message). Later turns append messages
// but keep that prefix identical, so the key stays constant for the whole
// conversation and drifts only when a genuinely new conversation starts.
//
// If the caller already set prompt_cache_key we never overwrite it.
func ensureKimiPromptCacheKey(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	if strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()) != "" {
		return body
	}
	key := kimiConversationCacheKey(gjson.GetBytes(body, "messages"))
	if key == "" {
		return body
	}
	updated, err := sjson.SetBytes(body, "prompt_cache_key", key)
	if err != nil {
		return body
	}
	return updated
}

// kimiConversationCacheKey builds a stable session identifier from the first
// system block and the first user message. Returns "" when neither is present
// (nothing stable to key on).
func kimiConversationCacheKey(messages gjson.Result) string {
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	var sys, firstUser string
	haveSys, haveUser := false, false
	for _, msg := range messages.Array() {
		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "system", "developer":
			if !haveSys {
				sys = kimiMessageText(msg)
				haveSys = true
			}
		case "user":
			if !haveUser {
				firstUser = kimiMessageText(msg)
				haveUser = true
			}
		}
		if haveSys && haveUser {
			break
		}
	}
	if sys == "" && firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sys + "\x00" + firstUser))
	return "cpa-" + hex.EncodeToString(sum[:16])
}

// kimiMessageText flattens a message's content (string or array-of-parts) to a
// single string for hashing.
func kimiMessageText(msg gjson.Result) string {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			if t := item.Get("text"); t.Exists() {
				parts = append(parts, t.String())
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func normalizeKimiToolMessageLinks(body []byte) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	msgs := messages.Array()
	out, dropped, err := filterKimiEmptyAssistantMessages(body, msgs)
	if err != nil {
		return body, err
	}
	if dropped > 0 {
		log.WithField("dropped_assistant_messages", dropped).Debug("kimi executor: dropped empty assistant messages")
	}

	messages = gjson.GetBytes(out, "messages")
	msgs = messages.Array()
	pending := make([]string, 0)
	patched := 0
	patchedReasoning := 0
	ambiguous := 0

	removePending := func(id string) {
		for idx := range pending {
			if pending[idx] != id {
				continue
			}
			pending = append(pending[:idx], pending[idx+1:]...)
			return
		}
	}

	for msgIdx := range msgs {
		msg := msgs[msgIdx]
		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "assistant":
			reasoning := msg.Get("reasoning_content")

			toolCalls := msg.Get("tool_calls")
			if !toolCalls.Exists() || !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
				continue
			}

			if !reasoning.Exists() || strings.TrimSpace(reasoning.String()) == "" {
				reasoningText := fallbackAssistantReasoning(msg)
				path := fmt.Sprintf("messages.%d.reasoning_content", msgIdx)
				next, err := sjson.SetBytes(out, path, reasoningText)
				if err != nil {
					return body, fmt.Errorf("kimi executor: failed to set assistant reasoning_content: %w", err)
				}
				out = next
				patchedReasoning++
			}

			for _, tc := range toolCalls.Array() {
				id := strings.TrimSpace(tc.Get("id").String())
				if id == "" {
					continue
				}
				pending = append(pending, id)
			}
		case "tool":
			toolCallID := strings.TrimSpace(msg.Get("tool_call_id").String())
			if toolCallID == "" {
				toolCallID = strings.TrimSpace(msg.Get("call_id").String())
				if toolCallID != "" {
					path := fmt.Sprintf("messages.%d.tool_call_id", msgIdx)
					next, err := sjson.SetBytes(out, path, toolCallID)
					if err != nil {
						return body, fmt.Errorf("kimi executor: failed to set tool_call_id from call_id: %w", err)
					}
					out = next
					patched++
				}
			}
			if toolCallID == "" {
				if len(pending) == 1 {
					toolCallID = pending[0]
					path := fmt.Sprintf("messages.%d.tool_call_id", msgIdx)
					next, err := sjson.SetBytes(out, path, toolCallID)
					if err != nil {
						return body, fmt.Errorf("kimi executor: failed to infer tool_call_id: %w", err)
					}
					out = next
					patched++
				} else if len(pending) > 1 {
					ambiguous++
				}
			}
			if toolCallID != "" {
				removePending(toolCallID)
			}
		}
	}

	if patched > 0 || patchedReasoning > 0 {
		log.WithFields(log.Fields{
			"patched_tool_messages":      patched,
			"patched_reasoning_messages": patchedReasoning,
		}).Debug("kimi executor: normalized tool message fields")
	}
	if ambiguous > 0 {
		log.WithFields(log.Fields{
			"ambiguous_tool_messages": ambiguous,
			"pending_tool_calls":      len(pending),
		}).Warn("kimi executor: tool messages missing tool_call_id with ambiguous candidates")
	}

	return out, nil
}

func filterKimiEmptyAssistantMessages(body []byte, msgs []gjson.Result) ([]byte, int, error) {
	kept := make([]string, 0, len(msgs))
	dropped := 0
	for _, msg := range msgs {
		if shouldDropKimiAssistantMessage(msg) {
			dropped++
			continue
		}
		kept = append(kept, msg.Raw)
	}
	if dropped == 0 {
		return body, 0, nil
	}

	rawMessages := []byte("[" + strings.Join(kept, ",") + "]")
	out, err := sjson.SetRawBytes(body, "messages", rawMessages)
	if err != nil {
		return body, 0, fmt.Errorf("kimi executor: failed to drop empty assistant messages: %w", err)
	}
	return out, dropped, nil
}

func shouldDropKimiAssistantMessage(msg gjson.Result) bool {
	if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
		return false
	}
	if hasKimiToolCalls(msg) || hasKimiLegacyFunctionCall(msg) || hasKimiAssistantReasoning(msg) {
		return false
	}
	return isKimiAssistantContentEmpty(msg.Get("content"))
}

func hasKimiToolCalls(msg gjson.Result) bool {
	toolCalls := msg.Get("tool_calls")
	return toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0
}

func hasKimiLegacyFunctionCall(msg gjson.Result) bool {
	functionCall := msg.Get("function_call")
	if !functionCall.Exists() || functionCall.Type == gjson.Null {
		return false
	}
	if functionCall.IsObject() && strings.TrimSpace(functionCall.Raw) == "{}" {
		return false
	}
	return strings.TrimSpace(functionCall.Raw) != ""
}

func hasKimiAssistantReasoning(msg gjson.Result) bool {
	reasoning := msg.Get("reasoning_content")
	return reasoning.Exists() && strings.TrimSpace(reasoning.String()) != ""
}

func isKimiAssistantContentEmpty(content gjson.Result) bool {
	if !content.Exists() || content.Type == gjson.Null {
		return true
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) == ""
	}
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if !isKimiAssistantContentPartEmpty(part) {
			return false
		}
	}
	return true
}

func isKimiAssistantContentPartEmpty(part gjson.Result) bool {
	if !part.Exists() || part.Type == gjson.Null {
		return true
	}
	if part.Type == gjson.String {
		return strings.TrimSpace(part.String()) == ""
	}
	if !part.IsObject() {
		return false
	}
	if text := part.Get("text"); text.Exists() {
		return strings.TrimSpace(text.String()) == ""
	}
	if strings.TrimSpace(part.Get("type").String()) == "text" {
		return true
	}
	return strings.TrimSpace(part.Raw) == "{}"
}

// fallbackAssistantReasoning derives a reasoning_content value for an assistant
// tool-call turn that arrived without one. Kimi K2.* thinking models require a
// non-empty reasoning_content on assistant turns that carry tool_calls, so the
// field cannot be omitted.
//
// The value MUST be genuine, self-contained reasoning text about THIS turn's own
// action — never a generic placeholder token. Kimi K2.7's thinking model is
// stateful across turns: if a prior assistant turn's reasoning is a bare marker
// like "(continuing)" or "[reasoning unavailable]", the model latches onto it
// and every subsequent turn's reasoning degenerates into echoing that marker
// (confirmed in prod: all displayed thinking became "(continuing)"). Describing
// the tool call the assistant actually made gives the model coherent, per-turn
// reasoning to continue from and avoids that feedback loop.
//
// Precedence: (1) the message's own text content, (2) a sentence synthesised
// from the tool call(s) it made, (3) a last-resort complete sentence.
func fallbackAssistantReasoning(msg gjson.Result) string {
	content := msg.Get("content")
	if content.Type == gjson.String {
		if text := strings.TrimSpace(content.String()); text != "" {
			return text
		}
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	// Synthesise reasoning from the tool call(s) this turn made. This is real,
	// action-specific text the thinking model can coherently continue from.
	if names := kimiToolCallNames(msg); len(names) > 0 {
		if len(names) == 1 {
			return "I'll use the " + names[0] + " tool to make progress on the task."
		}
		return "I'll use these tools to make progress on the task: " + strings.Join(names, ", ") + "."
	}

	// Last resort: a complete, self-contained sentence (never an open-ended
	// marker that the model would parrot).
	return "Proceeding with the next step based on the results so far."
}

// kimiToolCallNames returns the function names of an assistant message's
// tool_calls, in order, skipping empties.
func kimiToolCallNames(msg gjson.Result) []string {
	toolCalls := msg.Get("tool_calls")
	if !toolCalls.IsArray() {
		return nil
	}
	names := make([]string, 0, len(toolCalls.Array()))
	for _, tc := range toolCalls.Array() {
		name := strings.TrimSpace(tc.Get("function.name").String())
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// Refresh refreshes the Kimi token using the refresh token.
func (e *KimiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("kimi executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("kimi executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	client := kimiauth.NewDeviceFlowClientWithDeviceIDAndProxyURL(e.cfg, resolveKimiDeviceID(auth), auth.ProxyURL)
	td, err := client.RefreshToken(ctx, refreshToken)
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
	if td.ExpiresAt > 0 {
		exp := time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
		auth.Metadata["expired"] = exp
	}
	auth.Metadata["type"] = "kimi"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// applyKimiHeaders sets required headers for Kimi API requests.
// Headers match kimi-cli client for compatibility.
func applyKimiHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	// Match kimi-cli headers exactly
	r.Header.Set("User-Agent", "KimiCLI/1.10.6")
	r.Header.Set("X-Msh-Platform", "kimi_cli")
	r.Header.Set("X-Msh-Version", "1.10.6")
	r.Header.Set("X-Msh-Device-Name", getKimiHostname())
	r.Header.Set("X-Msh-Device-Model", getKimiDeviceModel())
	r.Header.Set("X-Msh-Device-Id", getKimiDeviceID())
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func resolveKimiDeviceIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}

	deviceIDRaw, ok := auth.Metadata["device_id"]
	if !ok {
		return ""
	}

	deviceID, ok := deviceIDRaw.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(deviceID)
}

func resolveKimiDeviceIDFromStorage(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}

	storage, ok := auth.Storage.(*kimiauth.KimiTokenStorage)
	if !ok || storage == nil {
		return ""
	}

	return strings.TrimSpace(storage.DeviceID)
}

func resolveKimiDeviceID(auth *cliproxyauth.Auth) string {
	deviceID := resolveKimiDeviceIDFromAuth(auth)
	if deviceID != "" {
		return deviceID
	}
	return resolveKimiDeviceIDFromStorage(auth)
}

func applyKimiHeadersWithAuth(r *http.Request, token string, stream bool, auth *cliproxyauth.Auth) {
	applyKimiHeaders(r, token, stream)

	if deviceID := resolveKimiDeviceID(auth); deviceID != "" {
		r.Header.Set("X-Msh-Device-Id", deviceID)
	}
}

// getKimiHostname returns the machine hostname.
func getKimiHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// getKimiDeviceModel returns a device model string matching kimi-cli format.
func getKimiDeviceModel() string {
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}

// getKimiDeviceID returns a stable device ID, matching kimi-cli storage location.
func getKimiDeviceID() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "cli-proxy-api-device"
	}
	// Check kimi-cli's device_id location first (platform-specific)
	var kimiShareDir string
	switch runtime.GOOS {
	case "darwin":
		kimiShareDir = filepath.Join(homeDir, "Library", "Application Support", "kimi")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(homeDir, "AppData", "Roaming")
		}
		kimiShareDir = filepath.Join(appData, "kimi")
	default: // linux and other unix-like
		kimiShareDir = filepath.Join(homeDir, ".local", "share", "kimi")
	}
	deviceIDPath := filepath.Join(kimiShareDir, "device_id")
	if data, err := os.ReadFile(deviceIDPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "cli-proxy-api-device"
}

// kimiCreds extracts the access token from auth.
func kimiCreds(a *cliproxyauth.Auth) (token string) {
	if a == nil {
		return ""
	}
	// Check metadata first (OAuth flow stores tokens here)
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	// Fallback to attributes (API key style)
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" {
			return v
		}
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	return ""
}

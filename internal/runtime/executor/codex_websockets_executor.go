// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

type codexWebsocketSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*codexWebsocketSession
}

var globalCodexWebsocketSessionStore = &codexWebsocketSessionStore{
	sessions: make(map[string]*codexWebsocketSession),
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	connMu sync.Mutex
	conn   *websocket.Conn
	wsURL  string
	authID string

	writeMu sync.Mutex

	activeMu     sync.Mutex
	activeCh     chan codexWebsocketRead
	activeDone   <-chan struct{}
	activeCancel context.CancelFunc

	readerConn *websocket.Conn

	upstreamDisconnectOnce sync.Once
	upstreamDisconnectCh   chan error

	// connGeneration is incremented each time ensureUpstreamConn dials a new
	// connection for this session. The bridge reads this before and after sending
	// to detect mid-flight reconnects precisely (not heuristically).
	connGeneration uint64

	// wsDisabled is the session-scoped transport policy latch (matches Codex CLI's
	// disable_websockets AtomicBool on ModelClientState). Once set, all subsequent
	// requests in this session skip WS and use HTTP.
	wsDisabled   bool
	wsDisabledAt time.Time

	// turnState holds the x-codex-turn-state sticky routing token captured from
	// server responses (handshake headers + response.created events).
	turnState string

	// windowGen is the window generation counter, incremented on session reset
	// or reconnect. Combined with the conversation ID, produces x-codex-window-id.
	windowGen uint64

	// warmedUp tracks whether a cache-priming warmup request has been sent on
	// this session's WebSocket. Set once after the first successful warmup.
	warmedUp bool
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

type codexWebsocketRead struct {
	conn    *websocket.Conn
	msgType int
	payload []byte
	err     error
}

func (s *codexWebsocketSession) setActive(ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeCancel != nil {
		s.activeCancel()
		s.activeCancel = nil
		s.activeDone = nil
	}
	s.activeCh = ch
	if ch != nil {
		activeCtx, activeCancel := context.WithCancel(context.Background())
		s.activeDone = activeCtx.Done()
		s.activeCancel = activeCancel
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) clearActive(ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeCh == ch {
		s.activeCh = nil
		if s.activeCancel != nil {
			s.activeCancel()
		}
		s.activeCancel = nil
		s.activeDone = nil
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) writeMessage(conn *websocket.Conn, msgType int, payload []byte) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteMessage(msgType, payload)
}

func (s *codexWebsocketSession) configureConn(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	conn.SetPingHandler(func(appData string) error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		// Reply pongs from the same write lock to avoid concurrent writes.
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})
}

func (s *codexWebsocketSession) notifyUpstreamDisconnect(err error) {
	if s == nil {
		return
	}
	s.upstreamDisconnectOnce.Do(func() {
		if s.upstreamDisconnectCh == nil {
			return
		}
		select {
		case s.upstreamDisconnectCh <- err:
		default:
		}
		close(s.upstreamDisconnectCh)
	})
}

func (e *CodexWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return e.CodexExecutor.executeCompact(ctx, auth, req, opts)
	}

	parsed := thinking.ParseModel(req.Model)
	modelStr := parsed.Stripped
	baseModel := parsed.BaseModel
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, modelStr, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = applyCursorGPTUpgradeIfEnabled(ctx, e.cfg, baseModel, body)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex websockets executor", body)

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return resp, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(from, req, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		sess.reqMu.Lock()
		defer sess.reqMu.Unlock()
		wsHeaders.Set("session_id", executionSessionID)
	}

	ginHeaders := extractCodexClientMetadataHeaders(ctx)
	_, wsReqBody := applyCurrentSessionMetadata(sess, executionSessionID, wsHeaders, body, ginHeaders)
	wsReqLog := helps.UpstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders.Clone(),
		Body:      wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			disableWSSession(sess, executionSessionID, "426 from upstream")
			return e.CodexExecutor.Execute(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			return resp, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		return resp, errDial
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, respHS)
	captureTurnStateFromHandshake(sess, respHS)
	_, wsReqBody = applyCurrentSessionMetadata(sess, executionSessionID, wsHeaders, body, ginHeaders)
	// Upstream 94c1b251: start the response-TTFT clock once handshake succeeds.
	reporter.StartResponseTTFT()
	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
		defer func() {
			reason := "completed"
			if err != nil {
				reason = "error"
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, reason, err)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(readCh)
		defer sess.clearActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a fresh websocket connection. This is mainly to handle
			// upstream closing the socket between sequential requests within the same
			// execution session.
			connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry == nil && connRetry != nil {
				captureTurnStateFromHandshake(sess, respHSRetry)
				_, wsReqBodyRetry := applyCurrentSessionMetadata(sess, executionSessionID, wsHeaders, body, ginHeaders)
				helps.RecordAPIWebsocketRequest(ctx, e.cfg, helps.UpstreamRequestLog{
					URL:       wsURL,
					Method:    "WEBSOCKET",
					Headers:   wsHeaders.Clone(),
					Body:      wsReqBodyRetry,
					Provider:  e.Identifier(),
					AuthID:    authID,
					AuthLabel: authLabel,
					AuthType:  authType,
					AuthValue: authValue,
				})
				recordAPIWebsocketHandshake(ctx, e.cfg, respHSRetry)
				reporter.StartResponseTTFT()
				if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry == nil {
					conn = connRetry
					wsReqBody = wsReqBodyRetry
				} else {
					e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
					helps.RecordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
					return resp, errSendRetry
				}
			} else {
				closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
				helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
				return resp, errDialRetry
			}
		} else {
			helps.RecordAPIWebsocketError(ctx, e.cfg, "send", errSend)
			return resp, errSend
		}
	}

	for {
		if ctx != nil && ctx.Err() != nil {
			return resp, ctx.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			helps.RecordAPIWebsocketError(ctx, e.cfg, "read", errRead)
			return resp, errRead
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				err = fmt.Errorf("codex websockets executor: unexpected binary message")
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", err)
				return resp, err
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		reporter.MarkFirstResponseByte()
		helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
			return resp, wsErr
		}

		payload = normalizeCodexWebsocketCompletion(payload)
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "response.completed" {
			if detail, ok := helps.ParseCodexUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}
			var param any
			out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, payload, &param)
			resp = cliproxyexecutor.Response{Payload: out}
			return resp, nil
		}
	}
}

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	log.Debugf("Executing Codex Websockets stream request with auth ID: %s, model: %s", auth.ID, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	parsed := thinking.ParseModel(req.Model)
	modelStr := parsed.Stripped
	baseModel := parsed.BaseModel
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	var originalTranslated []byte
	if len(opts.OriginalRequest) == 0 {
		originalTranslated = body
	} else {
		originalTranslated = sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, true)
	}

	body, err = thinking.ApplyThinking(body, modelStr, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, helps.PayloadRequestPath(opts))
	body = applyCursorGPTUpgradeIfEnabled(ctx, e.cfg, baseModel, body)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	// Remote compaction is for NON-Cursor chat-completions clients that don't
	// do their own compaction and hit upstream's context ceiling. Cursor has
	// native compaction and must keep owning that lifecycle, so it is excluded
	// here at the call site (and defensively again inside maybeRemoteCompact).
	// Pre-fix this was gated on IsCursorClient(ctx), which — combined with the
	// in-helper Cursor skip — made maybeRemoteCompact unreachable for everyone.
	if !IsCursorClient(ctx) {
		if compacted, ok := maybeRemoteCompact(ctx, e.cfg, auth, body, helps.ExecutionSessionIDFromOptions(opts)); ok {
			body = compacted
		}
	}
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex websockets executor", body)

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(from, req, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)

	var authID, authLabel, authType, authValue string
	authID = auth.ID
	authLabel = auth.Label
	authType, authValue = auth.AccountInfo()

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		if sess != nil {
			sess.reqMu.Lock()
		}
		wsHeaders.Set("session_id", executionSessionID)
	}

	ginHeaders := extractCodexClientMetadataHeaders(ctx)
	_, wsReqBody := applyCurrentSessionMetadata(sess, executionSessionID, wsHeaders, body, ginHeaders)
	wsReqLog := helps.UpstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders.Clone(),
		Body:      wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			disableWSSession(sess, executionSessionID, "426 from upstream")
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			// Unlock the per-session request mutex before returning. Without
			// this, a non-426 handshake rejection (401/403/429/500) on an
			// execution-session stream permanently strands sess.reqMu locked,
			// deadlocking every later request that reuses the same
			// executionSessionID.
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return nil, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, errDial
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, respHS)
	captureTurnStateFromHandshake(sess, respHS)
	_, wsReqBody = applyCurrentSessionMetadata(sess, executionSessionID, wsHeaders, body, ginHeaders)
	// Upstream 94c1b251: start the response-TTFT clock once handshake succeeds.
	reporter.StartResponseTTFT()

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a new websocket connection for the same execution session.
			connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry != nil || connRetry == nil {
				closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
				helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errDialRetry
			}
			captureTurnStateFromHandshake(sess, respHSRetry)
			_, wsReqBodyRetry := applyCurrentSessionMetadata(sess, executionSessionID, wsHeaders, body, ginHeaders)
			helps.RecordAPIWebsocketRequest(ctx, e.cfg, helps.UpstreamRequestLog{
				URL:       wsURL,
				Method:    "WEBSOCKET",
				Headers:   wsHeaders.Clone(),
				Body:      wsReqBodyRetry,
				Provider:  e.Identifier(),
				AuthID:    authID,
				AuthLabel: authLabel,
				AuthType:  authType,
				AuthValue: authValue,
			})
			recordAPIWebsocketHandshake(ctx, e.cfg, respHSRetry)
			reporter.StartResponseTTFT()
			if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry != nil {
				helps.RecordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
				e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errSendRetry
			}
			conn = connRetry
			wsReqBody = wsReqBodyRetry
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		keepaliveStop := make(chan struct{})
		var keepaliveStopOnce sync.Once
		stopKeepalive := func() {
			keepaliveStopOnce.Do(func() { close(keepaliveStop) })
		}
		defer stopKeepalive()
		keepaliveStarted := false

		defer close(out)
		defer func() {
			if sess != nil {
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var param any
		for {
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				terminateReason = "read_error"
				terminateErr = errRead
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read", errRead)
				reporter.PublishFailure(ctx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					err = fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = err
					helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", err)
					reporter.PublishFailure(ctx, err)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: err})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			reporter.MarkFirstResponseByte()
			helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				terminateReason = "upstream_error"
				terminateErr = wsErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
				reporter.PublishFailure(ctx, wsErr)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			payload = normalizeCodexWebsocketCompletion(payload)
			eventType := gjson.GetBytes(payload, "type").String()
			if eventType == "response.completed" || eventType == "response.done" {
				if detail, ok := helps.ParseCodexUsage(payload); ok {
					reporter.Publish(ctx, detail)
				}
			}
			if !shouldForwardEventToClient(ctx, eventType) {
				continue
			}

			line := encodeCodexWebsocketAsSSE(payload)
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayloadSource, body, line, &param)
			if len(chunks) == 0 && from.String() == "openai" && needsRawFallback(eventType) {
				if !send(cliproxyexecutor.StreamChunk{Payload: synthesizeChatCompletionsErrorChunk(payload)}) {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					return
				}
			} else {
				for i := range chunks {
					if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
						terminateReason = "context_done"
						terminateErr = ctx.Err()
						return
					}
				}
			}
			switch eventType {
			case "response.completed", "response.done", "response.failed",
				"response.incomplete", "response.error":
				stopKeepalive()
			}
			switch eventType {
			case "response.in_progress":
				if !keepaliveStarted && shouldStartCursorKeepalive(ctx, e.cfg, from) && len(chunks) > 0 {
					cachedChunks := make([][]byte, len(chunks))
					for i, c := range chunks {
						cachedChunks[i] = bytes.Clone(c)
					}
					interval := cursorKeepaliveInterval(e.cfg)
					go runCursorKeepalive(ctx, send, cachedChunks, interval, keepaliveStop, executionSessionID)
					keepaliveStarted = true
				}
			case "response.output_text.delta",
				"response.output_text.done",
				"response.reasoning_summary_text.delta",
				"response.reasoning_summary_text.done",
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_part.done",
				"response.reasoning_text.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.added",
				"response.output_item.done",
				"response.content_part.added",
				"response.content_part.done":
				stopKeepalive()
			}
			switch eventType {
			case "response.completed", "response.done":
				// Successful terminal events: stream is done, close cleanly.
				return
			case "response.failed", "response.incomplete", "response.error":
				// Terminal FAILURE events. The failure payload was already
				// forwarded to the client above; we must also stop the read
				// loop. Without this return the goroutine keeps blocking on
				// readCodexWebsocketMessage and the client hangs after a
				// terminal failure until the idle timeout fires.
				terminateReason = eventType
				terminateErr = fmt.Errorf("codex websockets: upstream terminal event %s", eventType)
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if conn != nil {
		// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
		// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
		conn.EnableWriteCompression(false)
	}
	return conn, resp, err
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func buildCodexWebsocketRequestBody(body []byte, clientMetadata map[string]string) []byte {
	if len(body) == 0 {
		return nil
	}

	// Match codex-rs websocket v2 semantics: every request is `response.create`.
	// Incremental follow-up turns continue on the same websocket using
	// `previous_response_id` + incremental `input`, not `response.append`.
	wsReqBody, errSet := sjson.SetBytes(bytes.Clone(body), "type", "response.create")
	if errSet != nil || len(wsReqBody) == 0 {
		wsReqBody = bytes.Clone(body)
		wsReqBody, _ = sjson.SetBytes(wsReqBody, "type", "response.create")
	}

	if len(clientMetadata) > 0 {
		keys := make([]string, 0, len(clientMetadata))
		for k := range clientMetadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := clientMetadata[k]
			if v == "" {
				continue
			}
			wsReqBody, _ = sjson.SetBytes(wsReqBody, "client_metadata."+k, v)
		}
	}

	return wsReqBody
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		return msgType, payload, errRead
	}
	if conn == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, ev.err
			}
			return ev.msgType, ev.payload, nil
		}
	}
}

func newProxyAwareWebsocketDialer(cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL == "" {
		return dialer
	}

	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: %v", errParse)
		return dialer
	}

	switch setting.Mode {
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer
	case proxyutil.ModeProxy:
	default:
		return dialer
	}

	switch setting.URL.Scheme {
	case "socks5", "socks5h":
		var proxyAuth *proxy.Auth
		if setting.URL.User != nil {
			username := setting.URL.User.Username()
			password, _ := setting.URL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		socksDialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("codex websockets executor: create SOCKS5 dialer failed: %v", errSOCKS5)
			return dialer
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(setting.URL)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", setting.URL.Scheme)
	}

	return dialer
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("codex websockets executor: unsupported responses websocket URL scheme %q", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("codex websockets executor: responses websocket URL host is empty")
	}
	return parsed.String(), nil
}

func applyCodexPromptCacheHeaders(from sdktranslator.Format, req cliproxyexecutor.Request, rawJSON []byte) ([]byte, http.Header) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers
	}

	var cache helps.CodexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			if cached, ok := helps.GetCodexCache(key); ok {
				cache = cached
			} else {
				cache = helps.CodexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				helps.SetCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
		setHeaderCasePreserved(headers, "session_id", cache.ID)
		headers.Set("Conversation_id", cache.ID)
	}

	return rawJSON, headers
}

func applyCodexWebsocketHeaders(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, token string, cfg *config.Config) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}

	var ginHeaders http.Header
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
	}

	isAPIKey := codexAuthUsesAPIKey(auth)
	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, ginHeaders, "x-codex-beta-features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-state", "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-metadata", "")
	misc.EnsureHeader(headers, ginHeaders, "x-client-request-id", "")
	misc.EnsureHeader(headers, ginHeaders, "x-responsesapi-include-timing-metrics", "")
	misc.EnsureHeader(headers, ginHeaders, "Version", "")
	if isAPIKey {
		ensureHeaderWithPriority(headers, ginHeaders, "User-Agent", "", "")
	} else {
		ensureHeaderWithConfigPrecedence(headers, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)
	}

	betaHeader := strings.TrimSpace(headers.Get("OpenAI-Beta"))
	if betaHeader == "" && ginHeaders != nil {
		betaHeader = strings.TrimSpace(ginHeaders.Get("OpenAI-Beta"))
	}
	if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
		betaHeader = codexResponsesWebsocketBetaHeaderValue
	}
	headers.Set("OpenAI-Beta", betaHeader)
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") {
		ensureHeaderCasePreserved(headers, ginHeaders, "session_id", "", uuid.NewString())
	}
	ensureHeaderCasePreserved(headers, ginHeaders, "session_id", "", "")
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		headers.Set("Originator", originator)
	} else if !isAPIKey {
		headers.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					setHeaderCasePreserved(headers, "ChatGPT-Account-ID", trimmed)
				}
			}
		}
	}

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)

	return headers
}

func codexAuthUsesAPIKey(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) != ""
}

func ensureHeaderCasePreserved(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(headerValueCaseInsensitive(target, key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(headerValueCaseInsensitive(source, key)); val != "" {
			setHeaderCasePreserved(target, key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		setHeaderCasePreserved(target, key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		setHeaderCasePreserved(target, key, val)
	}
}

func setHeaderCasePreserved(headers http.Header, key string, value string) {
	if headers == nil {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	deleteHeaderCaseInsensitive(headers, key)
	headers[key] = []string{value}
}

func headerValueCaseInsensitive(headers http.Header, key string) string {
	key = strings.TrimSpace(key)
	if headers == nil || key == "" {
		return ""
	}
	if val := strings.TrimSpace(headers.Get(key)); val != "" {
		return val
	}
	for existingKey, values := range headers {
		if !strings.EqualFold(existingKey, key) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func deleteHeaderCaseInsensitive(headers http.Header, key string) {
	for existingKey := range headers {
		if strings.EqualFold(existingKey, key) {
			delete(headers, existingKey)
		}
	}
}

func codexHeaderDefaults(cfg *config.Config, auth *cliproxyauth.Auth) (string, string) {
	if cfg == nil || auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return "", ""
		}
	}
	return strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent), strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

func ensureHeaderWithPriority(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func ensureHeaderWithConfigPrecedence(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

type statusErrWithHeaders struct {
	statusErr
	headers http.Header
}

func (e statusErrWithHeaders) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

func parseCodexWebsocketError(payload []byte) (error, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return nil, false
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		return nil, false
	}

	out := buildCodexWebsocketErrorPayload(payload, status)
	headers := parseCodexWebsocketErrorHeaders(payload)
	statusError := statusErr{code: status, msg: string(out)}
	if retryAfter := parseCodexRetryAfter(status, out, time.Now()); retryAfter != nil {
		statusError.retryAfter = retryAfter
	} else if isCodexWebsocketConnectionLimitError(payload) {
		retryAfter := time.Duration(0)
		statusError.retryAfter = &retryAfter
	}
	return statusErrWithHeaders{
		statusErr: statusError,
		headers:   headers,
	}, true
}

func buildCodexWebsocketErrorPayload(payload []byte, status int) []byte {
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "status", status)

	if bodyNode := gjson.GetBytes(payload, "body"); bodyNode.Exists() {
		out, _ = sjson.SetRawBytes(out, "body", []byte(bodyNode.Raw))
		if bodyErrorNode := bodyNode.Get("error"); bodyErrorNode.Exists() {
			out, _ = sjson.SetRawBytes(out, "error", []byte(bodyErrorNode.Raw))
			return out
		}
	}

	if errNode := gjson.GetBytes(payload, "error"); errNode.Exists() {
		out, _ = sjson.SetRawBytes(out, "error", []byte(errNode.Raw))
		return out
	}

	out, _ = sjson.SetBytes(out, "error.type", "server_error")
	out, _ = sjson.SetBytes(out, "error.message", http.StatusText(status))
	return out
}

func isCodexWebsocketConnectionLimitError(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	for _, path := range []string{"error.code", "error.type", "body.error.code", "body.error.type", "code", "error"} {
		if strings.TrimSpace(gjson.GetBytes(payload, path).String()) == "websocket_connection_limit_reached" {
			return true
		}
	}
	return false
}

func parseCodexWebsocketErrorHeaders(payload []byte) http.Header {
	headersNode := gjson.GetBytes(payload, "headers")
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	mapped := make(http.Header)
	headersNode.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			return true
		}
		switch value.Type {
		case gjson.String:
			if v := strings.TrimSpace(value.String()); v != "" {
				mapped.Set(name, v)
			}
		case gjson.Number, gjson.True, gjson.False:
			if v := strings.TrimSpace(value.Raw); v != "" {
				mapped.Set(name, v)
			}
		default:
		}
		return true
	})
	if len(mapped) == 0 {
		return nil
	}
	return mapped
}

func normalizeCodexWebsocketCompletion(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

func encodeCodexWebsocketAsSSE(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	line := make([]byte, 0, len("data: ")+len(payload))
	line = append(line, []byte("data: ")...)
	line = append(line, payload...)
	return line
}

func websocketUpgradeRequestLog(info helps.UpstreamRequestLog) helps.UpstreamRequestLog {
	upgradeInfo := info
	upgradeInfo.URL = helps.WebsocketUpgradeRequestURL(info.URL)
	upgradeInfo.Method = http.MethodGet
	upgradeInfo.Body = nil
	upgradeInfo.Headers = info.Headers.Clone()
	if upgradeInfo.Headers == nil {
		upgradeInfo.Headers = make(http.Header)
	}
	if strings.TrimSpace(upgradeInfo.Headers.Get("Connection")) == "" {
		upgradeInfo.Headers.Set("Connection", "Upgrade")
	}
	if strings.TrimSpace(upgradeInfo.Headers.Get("Upgrade")) == "" {
		upgradeInfo.Headers.Set("Upgrade", "websocket")
	}
	return upgradeInfo
}

func recordAPIWebsocketHandshake(ctx context.Context, cfg *config.Config, resp *http.Response) {
	if resp == nil {
		return
	}
	helps.RecordAPIWebsocketHandshake(ctx, cfg, resp.StatusCode, resp.Header.Clone())
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
}

func websocketHandshakeBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
	if len(body) == 0 {
		return nil
	}
	return body
}

func closeHTTPResponseBody(resp *http.Response, logPrefix string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("%s: %v", logPrefix, errClose)
	}
}

func executionSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func (e *CodexWebsocketsExecutor) getOrCreateSession(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	if e == nil {
		return nil
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.sessions == nil {
		store.sessions = make(map[string]*codexWebsocketSession)
	}
	if sess, ok := store.sessions[sessionID]; ok && sess != nil {
		return sess
	}
	sess := &codexWebsocketSession{
		sessionID:            sessionID,
		upstreamDisconnectCh: make(chan error, 1),
	}
	store.sessions[sessionID] = sess
	return sess
}

func (e *CodexWebsocketsExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	sess := e.getOrCreateSession(sessionID)
	if sess == nil {
		return nil
	}
	return sess.upstreamDisconnectCh
}

func (e *CodexWebsocketsExecutor) ensureUpstreamConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		return e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	}

	sess.connMu.Lock()
	conn := sess.conn
	readerConn := sess.readerConn
	boundAuthID := sess.authID
	boundWSURL := sess.wsURL
	sess.connMu.Unlock()
	if conn != nil {
		// The scheduler/auth rotation may have selected a different auth (or
		// upstream URL) than the one this session's live connection is bound
		// to. Reusing the existing socket in that case would silently route the
		// turn through the previous auth, bypassing rotation/failover. Tear the
		// stale connection down and redial on the newly selected auth/URL.
		if boundAuthID != authID || boundWSURL != wsURL {
			e.invalidateUpstreamConn(sess, conn, "auth_or_url_changed", nil)
		} else {
			if readerConn != conn {
				sess.connMu.Lock()
				sess.readerConn = conn
				sess.connMu.Unlock()
				sess.configureConn(conn)
				go e.readUpstreamLoop(sess, conn)
			}
			return conn, nil, nil
		}
	}

	conn, resp, errDial := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	if errDial != nil {
		return nil, resp, errDial
	}

	sess.connMu.Lock()
	if sess.conn != nil {
		previous := sess.conn
		sess.connMu.Unlock()
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
		return previous, nil, nil
	}
	sess.conn = conn
	sess.wsURL = wsURL
	sess.authID = authID
	sess.readerConn = conn
	sess.connGeneration++
	// New connection: sticky turn state and window id generation reset.
	sess.turnState = ""
	sess.windowGen++
	sess.connMu.Unlock()

	sess.configureConn(conn)
	go e.readUpstreamLoop(sess, conn)
	gen := sess.connGeneration
	go e.pingUpstreamLoop(sess, conn, gen)
	logCodexWebsocketConnected(sess.sessionID, authID, wsURL)
	return conn, resp, nil
}

func (e *CodexWebsocketsExecutor) pingUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn, gen uint64) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	const pingInterval = 30 * time.Second
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for range t.C {
		sess.connMu.Lock()
		stale := sess.conn != conn || sess.connGeneration != gen
		sess.connMu.Unlock()
		if stale {
			return
		}
		sess.writeMu.Lock()
		err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
		sess.writeMu.Unlock()
		if err != nil {
			log.Debugf("codex websockets: ping write failed gen=%d: %v — invalidating to trigger reconnect", gen, err)
			e.invalidateUpstreamConn(sess, conn, "ping_write_failed", err)
			return
		}
	}
}

func (e *CodexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			sess.activeMu.Lock()
			ch := sess.activeCh
			done := sess.activeDone
			sess.activeMu.Unlock()
			if ch != nil {
				select {
				case ch <- codexWebsocketRead{conn: conn, err: errRead}:
				case <-done:
				default:
				}
				sess.clearActive(ch)
				close(ch)
			}
			e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
			return
		}

		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
				sess.activeMu.Lock()
				ch := sess.activeCh
				done := sess.activeDone
				sess.activeMu.Unlock()
				if ch != nil {
					select {
					case ch <- codexWebsocketRead{conn: conn, err: errBinary}:
					case <-done:
					default:
					}
					sess.clearActive(ch)
					close(ch)
				}
				e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				return
			}
			continue
		}

		sess.activeMu.Lock()
		ch := sess.activeCh
		done := sess.activeDone
		sess.activeMu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, payload: payload}:
		case <-done:
		}
	}
}

func (e *CodexWebsocketsExecutor) invalidateUpstreamConn(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	if sess == nil || conn == nil {
		return
	}

	sess.connMu.Lock()
	current := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	if current == nil || current != conn {
		sess.connMu.Unlock()
		return
	}
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()

	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	sess.notifyUpstreamDisconnect(err)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func (e *CodexWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil {
		return
	}
	if sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		// Executor replacement can happen during hot reload (config/credential changes).
		// Do not force-close upstream websocket sessions here, otherwise in-flight
		// downstream websocket requests get interrupted.
		return
	}

	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	sess := store.sessions[sessionID]
	delete(store.sessions, sessionID)
	store.mu.Unlock()

	e.closeExecutionSession(sess, "session_closed")
}

func (e *CodexWebsocketsExecutor) closeAllExecutionSessions(reason string) {
	if e == nil {
		return
	}

	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	sessions := make([]*codexWebsocketSession, 0, len(store.sessions))
	for sessionID, sess := range store.sessions {
		delete(store.sessions, sessionID)
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	store.mu.Unlock()

	for i := range sessions {
		e.closeExecutionSession(sessions[i], reason)
	}
}

func (e *CodexWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
	closeCodexWebsocketSession(sess, reason)
}

func closeCodexWebsocketSession(sess *codexWebsocketSession, reason string) {
	if sess == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_closed"
	}

	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()

	if conn == nil {
		return
	}
	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func logCodexWebsocketConnected(sessionID string, authID string, wsURL string) {
	log.Infof("codex websockets: upstream connected session=%s auth=%s url=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL))
}

func logCodexWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	if err != nil {
		log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s err=%v", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason), err)
		return
	}
	log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason))
}

// CloseCodexWebsocketSessionsForAuthID closes all active Codex upstream websocket sessions
// associated with the supplied auth ID.
func CloseCodexWebsocketSessionsForAuthID(authID string, reason string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "auth_removed"
	}

	store := globalCodexWebsocketSessionStore
	if store == nil {
		return
	}

	type sessionItem struct {
		sessionID string
		sess      *codexWebsocketSession
	}

	store.mu.Lock()
	items := make([]sessionItem, 0, len(store.sessions))
	for sessionID, sess := range store.sessions {
		items = append(items, sessionItem{sessionID: sessionID, sess: sess})
	}
	store.mu.Unlock()

	matches := make([]sessionItem, 0)
	for i := range items {
		sess := items[i].sess
		if sess == nil {
			continue
		}
		sess.connMu.Lock()
		sessAuthID := strings.TrimSpace(sess.authID)
		sess.connMu.Unlock()
		if sessAuthID == authID {
			matches = append(matches, items[i])
		}
	}
	if len(matches) == 0 {
		return
	}

	toClose := make([]*codexWebsocketSession, 0, len(matches))
	store.mu.Lock()
	for i := range matches {
		current, ok := store.sessions[matches[i].sessionID]
		if !ok || current == nil || current != matches[i].sess {
			continue
		}
		delete(store.sessions, matches[i].sessionID)
		toClose = append(toClose, current)
	}
	store.mu.Unlock()

	for i := range toClose {
		closeCodexWebsocketSession(toClose[i], reason)
	}
}

// captureTurnStateFromHandshake extracts x-codex-turn-state from the WS handshake
// response headers and stores it on the session for replay on future requests.
func captureTurnStateFromHandshake(sess *codexWebsocketSession, respHS *http.Response) {
	if sess == nil || respHS == nil || respHS.Header == nil {
		return
	}
	ts := strings.TrimSpace(respHS.Header.Get("x-codex-turn-state"))
	if ts == "" {
		return
	}
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	if sess.turnState != ts {
		sess.turnState = ts
		log.Debugf("codex http-ws bridge: captured turn_state for session %s (len=%d)", sess.sessionID, len(ts))
	}
}

const cursorKeepaliveDefaultInterval = 1500 * time.Millisecond

func shouldStartCursorKeepalive(ctx context.Context, cfg *config.Config, from sdktranslator.Format) bool {
	if cfg == nil || !cfg.CursorKeepalive.Enabled {
		return false
	}
	if from.String() != "openai" {
		return false
	}
	return IsCursorClient(ctx)
}

func cursorKeepaliveInterval(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.CursorKeepalive.IntervalMs <= 0 {
		return cursorKeepaliveDefaultInterval
	}
	return time.Duration(cfg.CursorKeepalive.IntervalMs) * time.Millisecond
}

func runCursorKeepalive(ctx context.Context, send func(cliproxyexecutor.StreamChunk) bool, cachedChunks [][]byte, interval time.Duration, stop <-chan struct{}, executionSessionID string) {
	if len(cachedChunks) == 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	count := 0
	for {
		select {
		case <-stop:
			if count > 0 {
				log.Debugf("codex ws bridge: cursor keepalive stopped session=%s emissions=%d (first content event arrived)", executionSessionID, count)
			}
			return
		case <-ctx.Done():
			if count > 0 {
				log.Debugf("codex ws bridge: cursor keepalive stopped session=%s emissions=%d (ctx done)", executionSessionID, count)
			}
			return
		case <-ticker.C:
			for _, chunk := range cachedChunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
					log.Debugf("codex ws bridge: cursor keepalive stopped session=%s emissions=%d (send failed — client closed)", executionSessionID, count)
					return
				}
			}
			count++
		}
	}
}

var (
	unsupportedEventWarnSeenMu     sync.Mutex
	unsupportedEventWarnSeen       = map[string]time.Time{}
	unsupportedEventWarnPrunerOnce sync.Once
)

const (
	unsupportedEventWarnTTL           = 24 * time.Hour
	unsupportedEventWarnPruneInterval = 1 * time.Hour
)

func recordUnsupportedEventWarn(sessionID, eventType string) bool {
	startUnsupportedEventWarnPruner()
	key := sessionID + "\x00" + eventType
	unsupportedEventWarnSeenMu.Lock()
	defer unsupportedEventWarnSeenMu.Unlock()
	if when, ok := unsupportedEventWarnSeen[key]; ok && time.Since(when) < unsupportedEventWarnTTL {
		return false
	}
	unsupportedEventWarnSeen[key] = time.Now()
	return true
}

func startUnsupportedEventWarnPruner() {
	unsupportedEventWarnPrunerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(unsupportedEventWarnPruneInterval)
			defer ticker.Stop()
			for range ticker.C {
				pruneUnsupportedEventWarnSeen()
			}
		}()
	})
}

func pruneUnsupportedEventWarnSeen() {
	now := time.Now()
	unsupportedEventWarnSeenMu.Lock()
	defer unsupportedEventWarnSeenMu.Unlock()
	for k, when := range unsupportedEventWarnSeen {
		if now.Sub(when) >= unsupportedEventWarnTTL {
			delete(unsupportedEventWarnSeen, k)
		}
	}
}

func applyCurrentSessionMetadata(
	sess *codexWebsocketSession,
	executionSessionID string,
	wsHeaders http.Header,
	body []byte,
	hdrs codexClientMetadataHeaders,
) (clientMetadata map[string]string, wsReqBody []byte) {
	if sess != nil {
		sess.connMu.Lock()
		if ts := sess.turnState; ts != "" {
			wsHeaders.Set("x-codex-turn-state", ts)
		} else {
			wsHeaders.Del("x-codex-turn-state")
		}
		sess.connMu.Unlock()
	}
	clientMetadata = buildCodexClientMetadata(executionSessionID, sess, hdrs)
	if winID := clientMetadata["x-codex-window-id"]; winID != "" {
		wsHeaders.Set("x-codex-window-id", winID)
	} else {
		wsHeaders.Del("x-codex-window-id")
	}
	wsReqBody = buildCodexWebsocketRequestBody(body, clientMetadata)
	return clientMetadata, wsReqBody
}

type codexClientMetadataHeaders struct {
	TurnMetadata   string
	Subagent       string
	ParentThreadID string
}

func extractCodexClientMetadataHeaders(ctx context.Context) codexClientMetadataHeaders {
	if ctx == nil {
		return codexClientMetadataHeaders{}
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return codexClientMetadataHeaders{}
	}
	h := ginCtx.Request.Header
	return codexClientMetadataHeaders{
		TurnMetadata:   strings.TrimSpace(h.Get("x-codex-turn-metadata")),
		Subagent:       strings.TrimSpace(h.Get("x-openai-subagent")),
		ParentThreadID: strings.TrimSpace(h.Get("x-codex-parent-thread-id")),
	}
}

func buildCodexClientMetadata(sessionKey string, sess *codexWebsocketSession, hdrs codexClientMetadataHeaders) map[string]string {
	metadata := make(map[string]string, 4)
	if sessionKey != "" {
		gen := uint64(1)
		if sess != nil {
			sess.connMu.Lock()
			if sess.windowGen == 0 {
				sess.windowGen = 1
			}
			gen = sess.windowGen
			sess.connMu.Unlock()
		}
		metadata["x-codex-window-id"] = fmt.Sprintf("%s:%d", sessionKey, gen)
	}
	if hdrs.TurnMetadata != "" {
		metadata["x-codex-turn-metadata"] = hdrs.TurnMetadata
	}
	if hdrs.Subagent != "" {
		metadata["x-openai-subagent"] = hdrs.Subagent
	}
	if hdrs.ParentThreadID != "" {
		metadata["x-codex-parent-thread-id"] = hdrs.ParentThreadID
	}
	return metadata
}

const codexForcePassthroughGinKey = "codex-force-passthrough"

func forcePassthroughFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		return ginCtx.GetBool(codexForcePassthroughGinKey)
	}
	return false
}

func bridgeSessionKey(opts cliproxyexecutor.Options, payload []byte) string {
	if key := helps.ExecutionSessionIDFromOptions(opts); key != "" {
		return key
	}
	pck := gjson.GetBytes(payload, "prompt_cache_key").String()
	if strings.HasPrefix(pck, "cli-proxy-") {
		return ""
	}
	return pck
}

func bridgeOpts(opts cliproxyexecutor.Options, sessionKey string) cliproxyexecutor.Options {
	o := opts
	if o.Metadata == nil {
		o.Metadata = make(map[string]interface{})
	}
	o.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = sessionKey
	return o
}

func stripHTTPOnlyFields(payload []byte) []byte {
	payload, _ = sjson.DeleteBytes(payload, "safety_identifier")
	payload, _ = sjson.DeleteBytes(payload, "prompt_cache_retention")
	return payload
}

const wsDisabledRecoveryWindow = 5 * time.Minute

func disableWSSession(sess *codexWebsocketSession, sessionKey, reason string) {
	if sess == nil {
		return
	}
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	if sess.wsDisabled {
		return
	}
	sess.wsDisabled = true
	sess.wsDisabledAt = time.Now()
	log.Warnf("codex http-ws bridge: WS transport disabled for session %s (%s, will recover after %v)", sessionKey, reason, wsDisabledRecoveryWindow)
}

func isUpgradeRequiredError(err error) bool {
	if err == nil {
		return false
	}
	var se statusErr
	if errors.As(err, &se) && se.code == http.StatusUpgradeRequired {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "426") && strings.Contains(errStr, "upgrade required")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func authIDForBridge(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

// CodexAutoExecutor routes Codex requests to the websocket transport only when:
//  1. The downstream transport is websocket, and
//  2. The selected auth enables websockets.
//
// For non-websocket downstream requests, it always uses the legacy HTTP implementation.
type CodexAutoExecutor struct {
	cfg      *config.Config
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		cfg:      cfg,
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) isWSDisabled(sessionKey string) bool {
	sess := e.getWSSession(sessionKey)
	if sess == nil {
		return false
	}
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	if !sess.wsDisabled {
		return false
	}
	if time.Since(sess.wsDisabledAt) > wsDisabledRecoveryWindow {
		sess.wsDisabled = false
		log.Infof("codex http-ws bridge: WS transport re-enabled for session %s after %v recovery window", sessionKey, wsDisabledRecoveryWindow)
		return false
	}
	return true
}

func (e *CodexAutoExecutor) disableWS(sessionKey string) {
	disableWSSession(e.getWSSession(sessionKey), sessionKey, "auto bridge fallback")
}

func (e *CodexAutoExecutor) getWSSession(sessionKey string) *codexWebsocketSession {
	if e == nil || e.wsExec == nil || e.wsExec.store == nil {
		return nil
	}
	store := e.wsExec.store
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.sessions[sessionKey]
}

func (e *CodexAutoExecutor) connGeneration(sessionKey string) uint64 {
	if e == nil || e.wsExec == nil || e.wsExec.store == nil {
		return 0
	}
	store := e.wsExec.store
	store.mu.Lock()
	defer store.mu.Unlock()
	sess, ok := store.sessions[sessionKey]
	if !ok || sess == nil {
		return 0
	}
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	return sess.connGeneration
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if forcePassthroughFromContext(ctx) {
		return e.httpExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		if sk := helps.ExecutionSessionIDFromOptions(opts); sk == "" || !e.isWSDisabled(sk) {
			return e.wsExec.Execute(ctx, auth, req, opts)
		}
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if forcePassthroughFromContext(ctx) {
		return e.httpExec.ExecuteStream(ctx, auth, req, opts)
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		if sk := helps.ExecutionSessionIDFromOptions(opts); sk == "" || !e.isWSDisabled(sk) {
			return e.wsExec.ExecuteStream(ctx, auth, req, opts)
		}
	}
	chainingEnabled := e.cfg != nil && e.cfg.CodexResponseChaining.Enabled
	if chainingEnabled && codexWebsocketsEnabled(auth) {
		if result, err, ok := e.bridgedExecuteStream(ctx, auth, req, opts); ok {
			return result, err
		}
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) bridgedExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error, bool) {
	sessionKey := bridgeSessionKey(opts, req.Payload)
	if sessionKey == "" {
		return nil, nil, false
	}
	if e.isWSDisabled(sessionKey) {
		return nil, nil, false
	}

	wsOpts := bridgeOpts(opts, sessionKey)
	bridge := getHTTPWSBridge()
	model := gjson.GetBytes(req.Payload, "model").String()
	if model == "" {
		model = req.Model
	}
	authID := authIDForBridge(auth)

	turn := e.snapshotBridgeTurn(sessionKey, bridge)
	result, err, sentDelta := e.doBridgedStream(ctx, auth, req, wsOpts, sessionKey, bridge, authID)
	turn.finalize(e.connGeneration(sessionKey), sentDelta, currentInputCount(req.Payload))

	if err != nil || turn.reconnectedDelta {
		if isUpgradeRequiredError(err) {
			e.disableWS(sessionKey)
			bridge.Reset(sessionKey)
			return nil, nil, false
		}
		if turn.sentDelta {
			if turn.reconnectedDelta {
				log.Infof("codex http-ws bridge: WS reconnected during delta session=%s (gen %d→%d), retrying full", sessionKey, turn.genBefore, turn.genAfter)
			} else {
				log.Infof("codex http-ws bridge: delta failed session=%s, retrying full: %v", sessionKey, err)
			}
			if result != nil && result.Chunks != nil {
				go func(ch <-chan cliproxyexecutor.StreamChunk) {
					for range ch {
					}
				}(result.Chunks)
			}
			bridge.Reset(sessionKey)
			turn = e.snapshotBridgeTurn(sessionKey, bridge)
			var retrySentDelta bool
			result, err, retrySentDelta = e.doBridgedStream(ctx, auth, req, wsOpts, sessionKey, bridge, authID)
			turn.finalize(e.connGeneration(sessionKey), retrySentDelta, currentInputCount(req.Payload))
			if err != nil {
				log.Warnf("codex http-ws bridge: full retry also failed session=%s, falling back to HTTP: %v", sessionKey, err)
				bridge.Reset(sessionKey)
				return nil, nil, false
			}
		} else {
			log.Warnf("codex http-ws bridge: ExecuteStream failed session=%s, falling back to HTTP: %v", sessionKey, err)
			bridge.Reset(sessionKey)
			return nil, nil, false
		}
	}

	telemetry := turn.telemetry(e.connGeneration(sessionKey))
	result = e.wrapBridgedStreamForCapture(sessionKey, model, authID, req.Payload, result, telemetry)
	return result, nil, true
}

type bridgeTurn struct {
	baselineBefore   int
	gap              time.Duration
	firstTurn        bool
	genBefore        uint64
	genAfter         uint64
	sentDelta        bool
	reconnectedDelta bool
	deltaItems       int
}

func (e *CodexAutoExecutor) snapshotBridgeTurn(sessionKey string, bridge *HTTPToWSBridge) bridgeTurn {
	gap, firstTurn := bridge.GapSinceLastCompleted(sessionKey)
	return bridgeTurn{
		baselineBefore: bridge.BaselineCount(sessionKey),
		gap:            gap,
		firstTurn:      firstTurn,
		genBefore:      e.connGeneration(sessionKey),
	}
}

func (t *bridgeTurn) finalize(genAfter uint64, sentDelta bool, currentInputCount int) {
	t.genAfter = genAfter
	t.sentDelta = sentDelta
	t.reconnectedDelta = sentDelta && genAfter != t.genBefore
	if sentDelta && currentInputCount > t.baselineBefore {
		t.deltaItems = currentInputCount - t.baselineBefore
	}
}

type bridgeTurnTelemetry struct {
	sentDelta      bool
	reconnected    bool
	connGen        uint64
	gapSinceLast   time.Duration
	firstTurn      bool
	baselineBefore int
	deltaItems     int
}

func (t bridgeTurn) telemetry(currentConnGen uint64) bridgeTurnTelemetry {
	return bridgeTurnTelemetry{
		sentDelta:      t.sentDelta,
		reconnected:    t.reconnectedDelta,
		connGen:        currentConnGen,
		gapSinceLast:   t.gap,
		firstTurn:      t.firstTurn,
		baselineBefore: t.baselineBefore,
		deltaItems:     t.deltaItems,
	}
}

func currentInputCount(payload []byte) int {
	if inputArr := gjson.GetBytes(payload, "input"); inputArr.IsArray() {
		return len(inputArr.Array())
	}
	return 0
}

func (e *CodexAutoExecutor) doBridgedStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, wsOpts cliproxyexecutor.Options, sessionKey string, bridge *HTTPToWSBridge, authID string) (*cliproxyexecutor.StreamResult, error, bool) {
	payload := req.Payload
	wsPayload := stripHTTPOnlyFields(bytes.Clone(payload))

	deltaJSON, prevRespID := bridge.ComputeDelta(sessionKey, payload, authID)
	sentDelta := false
	if deltaJSON != nil && prevRespID != "" {
		wsPayload, _ = sjson.SetRawBytes(wsPayload, "input", deltaJSON)
		wsPayload, _ = sjson.SetBytes(wsPayload, "previous_response_id", prevRespID)
		sentDelta = true
		log.Debugf("codex http-ws bridge: ExecuteStream delta session=%s prev_resp=%s", sessionKey, prevRespID[:minInt(20, len(prevRespID))])
	} else {
		log.Debugf("codex http-ws bridge: ExecuteStream full (turn 1) session=%s", sessionKey)
	}

	wsReq := req
	wsReq.Payload = wsPayload
	result, err := e.wsExec.ExecuteStream(ctx, auth, wsReq, wsOpts)
	return result, err, sentDelta
}

func (e *CodexAutoExecutor) wrapBridgedStreamForCapture(sessionKey, model, authID string, requestPayload []byte, result *cliproxyexecutor.StreamResult, telemetry bridgeTurnTelemetry) *cliproxyexecutor.StreamResult {
	if result != nil && result.Headers != nil {
		primary := result.Headers.Get("x-codex-primary-used-percent")
		secondary := result.Headers.Get("x-codex-secondary-primary-used-percent")
		if primary != "" || secondary != "" {
			log.Infof("codex http-ws bridge: quota session=%s primary_used=%s%% weekly_used=%s%%", sessionKey, primary, secondary)
		}
	}

	wrappedCh := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(wrappedCh)
		outputItemsByIndex := map[int64][]byte{}
		outputItemsFallback := [][]byte{}
		for chunk := range result.Chunks {
			wrappedCh <- chunk
			if len(chunk.Payload) == 0 {
				continue
			}
			for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
				if !bytes.HasPrefix(line, dataTag) {
					continue
				}
				eventData := bytes.TrimSpace(line[5:])
				eventType := gjson.GetBytes(eventData, "type").String()

				if eventType == "response.output_item.done" {
					outputItemsFallback = collectOutputItem(eventData, outputItemsByIndex, outputItemsFallback)
				}

				if eventType == "response.created" {
					ts := gjson.GetBytes(eventData, "response.turn_state").String()
					if ts == "" {
						ts = gjson.GetBytes(eventData, "turn_state").String()
					}
					if ts != "" {
						if wsSess := e.getWSSession(sessionKey); wsSess != nil {
							wsSess.connMu.Lock()
							if wsSess.turnState != ts {
								wsSess.turnState = ts
								log.Debugf("codex http-ws bridge: captured turn_state from response.created for session %s", sessionKey)
							}
							wsSess.connMu.Unlock()
						}
					}
				}

				if eventType == "response.completed" || eventType == "response.done" {
					respID := extractResponseIDFromSSEPayload(eventData)
					if respID != "" {
						capturePayload, patched := patchResponseOutputIfMissing(eventData, outputItemsByIndex, outputItemsFallback)
						if patched {
							log.Debugf("codex http-ws bridge: patched response.output for session %s with %d items", sessionKey, len(outputItemsByIndex)+len(outputItemsFallback))
						}
						bridge := getHTTPWSBridge()
						bridge.CaptureResponse(sessionKey, respID, model, authID, requestPayload, capturePayload)
						bridge.MarkCompleted(sessionKey)
						outputItemsByIndex = map[int64][]byte{}
						outputItemsFallback = [][]byte{}
					}
					detail, ok := helps.ParseCodexUsage(eventData)
					if ok {
						var cachePct float64
						if detail.InputTokens > 0 {
							cachePct = float64(detail.CachedTokens) / float64(detail.InputTokens) * 100
						}
						class := "full_send"
						if telemetry.sentDelta {
							switch {
							case telemetry.reconnected:
								class = "reconnect_miss"
							case detail.CachedTokens == 0:
								class = "cold_miss"
							case cachePct >= 80:
								class = "warm_hit"
							default:
								class = "partial_hit"
							}
						}
						gapStr := "first"
						if !telemetry.firstTurn {
							gapStr = telemetry.gapSinceLast.Round(time.Millisecond).String()
						}
						log.Infof("codex http-ws bridge: turn session=%s model=%s class=%s input=%d cached=%d (%.1f%%) output=%d total=%d sent_delta=%v reconnected=%v conn_gen=%d gap=%s baseline=%d delta_items=%d",
							sessionKey, model, class,
							detail.InputTokens, detail.CachedTokens, cachePct, detail.OutputTokens, detail.TotalTokens,
							telemetry.sentDelta, telemetry.reconnected, telemetry.connGen,
							gapStr, telemetry.baselineBefore, telemetry.deltaItems)
						if telemetry.sentDelta && (class == "partial_hit" || class == "cold_miss") {
							if bridge := getHTTPWSBridge(); bridge != nil {
								bridge.Reset(sessionKey)
								log.Warnf("codex http-ws bridge: cache-miss recovery — reset session %s after %s (cache=%.1f%%)", sessionKey, class, cachePct)
							}
						}
					}
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: wrappedCh}
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	if e.cfg != nil && e.cfg.CodexResponseChaining.Enabled && sessionID != "" {
		getHTTPWSBridge().Reset(sessionID)
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

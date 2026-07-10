package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// codexContinueFoldContext bundles the closures a fold loop needs to open
// continuation rounds: rebuilding the upstream request through the same
// cacheHelper/headers/httpClient path the original round used. Keeping it
// in one struct lets the fold helper stay a pure method with no surprises.
type codexContinueFoldContext struct {
	cfg              *config.CodexContinueConfig
	rootConfig       *config.Config
	executor         *CodexExecutor
	auth             *cliproxyauth.Auth
	req              cliproxyexecutor.Request
	from             sdktranslator.Format
	to               sdktranslator.Format
	originalPayload  []byte
	baseBody         []byte // agent's translated request body (post-thinking, pre-round-shape)
	url              string
	apiKey           string
	identityState    codexIdentityConfuseState
	httpClient       *http.Client
	responseFormat   sdktranslator.Format
	openContinuation codexContinueRoundOpener
	appendResponse   codexContinueAppendResponse
}

type codexContinueRound struct {
	body          io.ReadCloser
	identityState codexIdentityConfuseState
	statusCode    int
}

type (
	codexContinueRoundOpener    func(ctx context.Context, body []byte) (*codexContinueRound, error)
	codexContinueForwardEvent   func(line []byte, identityState codexIdentityConfuseState)
	codexContinueAppendResponse func(ctx context.Context, payload []byte)
)

// foldActive reports whether the fold is enabled for this request.
func (fx *codexContinueFoldContext) foldActive() bool {
	return fx.cfg != nil && fx.cfg.Enabled
}

func (fx *codexContinueFoldContext) config() *config.Config {
	if fx == nil {
		return nil
	}
	if fx.rootConfig != nil {
		return fx.rootConfig
	}
	if fx.executor != nil {
		return fx.executor.cfg
	}
	return nil
}

func (fx *codexContinueFoldContext) appendResponsePayload(ctx context.Context, payload []byte) {
	if fx != nil && fx.appendResponse != nil {
		fx.appendResponse(ctx, payload)
		return
	}
	helps.AppendAPIResponseChunk(ctx, fx.config(), payload)
}

func codexContinueConfigForBody(cfg *config.Config, body []byte, route string) *config.CodexContinueConfig {
	var raw *config.CodexContinueConfig
	if cfg != nil && cfg.CodexContinueThinking != nil {
		clone := *cfg.CodexContinueThinking
		raw = &clone
	}
	foldCfg := helps.NormalizeCodexContinueConfig(raw)
	if foldCfg.Enabled && !helps.ReasoningEnabled(body) {
		log.Infof("codex continue: fold configured but reasoning disabled in %s request; falling through to legacy path", route)
		foldCfg.Enabled = false
	}
	return foldCfg
}

// runFoldLoop drives the multi-round fold. Round 1's body is already open
// (caller opened it). Continuation rounds are opened here via openContinuationRound.
//
// Returns when the response is finished (clean or stopped). All chunks are
// written to out. The caller closes out and the first round body.
//
// When fold is disabled, this is a thin wrapper around scanOneRound that
// preserves the legacy behavior exactly.
func (fx *codexContinueFoldContext) runFoldLoop(
	ctx context.Context,
	firstBody io.ReadCloser,
	firstIdentityState codexIdentityConfuseState,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	replayScope codexReasoningReplayScope,
	forwardEvent codexContinueForwardEvent,
) {
	defer func() {
		if errClose := firstBody.Close(); errClose != nil {
			log.Errorf("codex continue: close response body error: %v", errClose)
		}
	}()

	// forwardEvent translates one SSE line and writes the resulting chunks
	// to out, respecting ctx cancellation. Mirrors the legacy goroutine body.
	// The identity state is passed per-call because each continuation round may
	// carry its own confuse/expose mapping; using the first round's state for
	// every round would de-obfuscate later responses with the wrong keys.
	//
	// translatorParam MUST be declared outside the closure and shared across
	// every forwardEvent call. The stream translators
	// (ConvertOpenAIChatCompletionsResponseToOpenAIResponses etc.) store
	// stateful accumulators on *param across chunks: text buffers, tool-call
	// argument buffers, output-index maps, response IDs, "item added" flags.
	// A fresh `var param any` per call resets all of that on every chunk, which
	// makes the translator emit a new response.created per chunk, drop all
	// accumulated text, and lose tool-call arguments — presenting downstream as
	// empty responses with no tool calls.
	if forwardEvent == nil {
		var translatorParam any
		forwardEvent = func(line []byte, state codexIdentityConfuseState) {
			line = applyCodexIdentityExposeResponsePayload(line, state)
			payload := codexDataPayload(line)
			if payload != nil && fx.from.String() == "openai" {
				if incompleteChunk, ok := translateCodexIncompleteToChatChunk(payload); ok {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: incompleteChunk}:
					case <-ctx.Done():
					}
					return
				}
				if needsRawFallback(gjson.GetBytes(payload, "type").String()) {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: synthesizeChatCompletionsErrorChunk(payload)}:
					case <-ctx.Done():
					}
					return
				}
			}
			chunks := sdktranslator.TranslateStream(ctx, fx.to, fx.responseFormat, fx.req.Model,
				fx.originalPayload, fx.baseBody, line, &translatorParam)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
	}

	if !fx.foldActive() {
		// Legacy path: scan + forward, no buffering, no continuation.
		fx.scanOneRound(ctx, firstBody, firstIdentityState, out, reporter, replayScope, fx.baseBody, forwardEvent)
		return
	}

	if codexFoldBodyHasCompactionTrigger(fx.baseBody) {
		fx.cfg.Enabled = false
		fx.scanOneRound(ctx, firstBody, firstIdentityState, out, reporter, replayScope, fx.baseBody, forwardEvent)
		return
	}

	// Fold active: multi-round loop.
	// Ownership: firstBody belongs to the caller.
	// Every continuation response opened by this loop must be closed by this loop.
	state := newCodexFoldState(fx.baseBody)
	roundNo := 0
	totalOutputTokens := 0
	round := &codexContinueRound{body: firstBody, identityState: firstIdentityState}
	identityState := firstIdentityState

	closeContinuationRound := func(r *codexContinueRound) {
		if r == nil || r.body == nil || r.body == firstBody {
			return
		}
		if errClose := r.body.Close(); errClose != nil {
			log.Errorf("codex continue: close continuation response body error: %v", errClose)
		}
	}

	for {
		roundNo++
		roundOut := fx.scanFoldRound(ctx, round.body, identityState, out, reporter, replayScope, state, forwardEvent)
		if roundOut.terminalEvent == nil {
			log.Warnf("codex continue: round %d upstream EOF no terminal event", roundNo)
			fx.flushBufferedRound(ctx, state, roundOut, identityState, forwardEvent)
			if roundOut.terminalErr != nil {
				fx.forwardFoldError(ctx, out, reporter, roundOut.terminalErr)
			} else {
				incompleteEvt := fx.buildSyntheticIncomplete(state, roundOut, "upstream_eof", roundNo, totalOutputTokens)
				forwardEvent(incompleteEvt, identityState)
			}
			closeContinuationRound(round)
			return
		}

		// Parse usage.reasoning_tokens from the terminal event. terminalEvent is
		// stored with the leading "data: " SSE framing; strip it before parsing.
		terminalPayload := codexSSEDataPayload(roundOut.terminalEvent)
		tokens, _ := helps.ReasoningTokens(terminalPayload)
		totalOutputTokens += tokens
		state.addRound(roundNo, roundOut, tokens)

		terminalOutcome := helps.ClassifyCodexResponsesEvent(terminalPayload)
		if terminalOutcome.Failure || roundOut.terminalErr != nil {
			fx.flushBufferedRound(ctx, state, roundOut, identityState, forwardEvent)
			if roundOut.terminalErr != nil {
				fx.forwardFoldError(ctx, out, reporter, roundOut.terminalErr)
			} else {
				forwardEvent(roundOut.terminalEvent, identityState)
			}
			closeContinuationRound(round)
			return
		}
		if terminalOutcome.Incomplete {
			fx.flushBufferedRound(ctx, state, roundOut, identityState, forwardEvent)
			forwardEvent(roundOut.terminalEvent, identityState)
			closeContinuationRound(round)
			return
		}

		// Truncation check.
		step := fx.cfg.TruncationStep
		if step <= 0 {
			step = helps.CodexContinueDefaultStep
		}
		hasEnc := helps.HasEncryptedReasoning(roundOut.reasoningItems)
		truncated := helps.IsTruncationPattern(tokens, step)
		liveCommitBarrier := codexCommitPolicy(fx.cfg) == codexCommitLiveBarrier && state.hasLiveCommitBarrier(roundOut)
		zeroStall := roundNo > 1 && tokens == 0
		nearContextLimit := truncated && fx.codexFoldRoundNearContextLimit(roundOut)
		shouldCont := fx.cfg.Enabled &&
			!liveCommitBarrier &&
			!nearContextLimit &&
			helps.ShouldContinue(tokens, step, fx.cfg.MinN, fx.cfg.MaxN) &&
			hasEnc &&
			(fx.cfg.MaxContinue == 0 || roundNo <= fx.cfg.MaxContinue) &&
			(fx.cfg.MaxTotalOutputTokens == 0 || totalOutputTokens < fx.cfg.MaxTotalOutputTokens)
		shouldRetryZeroStall := zeroStall && state.openedAttempts <= fx.maxFoldAttempts()

		if !shouldCont && !shouldRetryZeroStall {
			// Stopped. Was it because of a guard while still truncated?
			stoppedReason := ""
			if liveCommitBarrier && truncated {
				stoppedReason = "committed_live_output"
			} else if nearContextLimit {
				stoppedReason = "near_context_limit"
			} else if helps.IsTruncationPattern(tokens, step) {
				stoppedReason = helps.StoppedReasonWhenTruncated(
					tokens, step, hasEnc, roundNo,
					fx.cfg.MaxContinue, totalOutputTokens, fx.cfg.MaxTotalOutputTokens)
			} else if zeroStall {
				stoppedReason = "zero_reasoning_stall"
			}
			if stoppedReason == "zero_reasoning_stall" || stoppedReason == "near_context_limit" {
				// Discard tentative buffered output — do not flush a truncated
				// near-limit or stalled answer downstream.
				incompleteEvt := fx.buildSyntheticIncomplete(state, roundOut, stoppedReason, roundNo, totalOutputTokens)
				forwardEvent(incompleteEvt, identityState)
				closeContinuationRound(round)
				return
			}
			// Clean stop: flush this round's buffered tentative output as the
			// real answer BEFORE the terminal event.
			fx.flushBufferedRound(ctx, state, roundOut, identityState, forwardEvent)
			// Forward the reconstructed terminal event downstream.
			terminal := fx.buildFoldedTerminal(state, roundOut, stoppedReason)
			forwardEvent(terminal, identityState)
			// Cache reasoning replay from the completed event (same as legacy).
			if helps.ClassifyCodexResponsesEvent(codexSSEDataPayload(terminal)).Success {
				cacheCodexReasoningReplayFromCompleted(replayScope, codexSSEDataPayload(terminal))
			}
			if reporter != nil {
				reporter.Publish(ctx, state.billedUsage.detail())
			}
			closeContinuationRound(round)
			return
		}

		// Continue: open another round with replayed reasoning + commentary marker.
		log.Infof("codex continue: round %d truncated at reasoning_tokens=%d (tier n=%d), opening continuation",
			roundNo, tokens, helps.TierN(tokens, step))

		// Build the continuation request body from the agent's original
		// translated body (baseBody), with our replayed input.
		if zeroStall {
			state.zeroStalls++
		} else {
			state.appendReplay(roundOut.reasoningItems, helps.CommentaryMessage(fx.cfg.MarkerText))
		}
		contBody := helps.BuildContinuationPayload(fx.baseBody, state.continuationInput(),
			true /*force_include_encrypted*/)

		// Open the continuation request through the same path as round 1.
		var errOpen error
		prevRound := round
		state.openedAttempts++
		round, errOpen = fx.openContinuationRound(ctx, contBody)
		if round != nil {
			identityState = round.identityState
		}
		// Continuation rounds beyond the first are owned by this loop. Round 1
		// is owned by the caller.
		closeContinuationRound(prevRound)
		if errOpen != nil {
			log.Warnf("codex continue: round %d continuation open failed: %v", roundNo+1, errOpen)
			incompleteEvt := fx.buildSyntheticIncomplete(state, roundOut, "upstream_error", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt, identityState)
			return
		}
		if round.statusCode >= 400 {
			body, _ := io.ReadAll(round.body)
			closeContinuationRound(round)
			log.Warnf("codex continue: round %d continuation HTTP %d: %s",
				roundNo+1, round.statusCode, string(body[:min(len(body), 2000)]))
			incompleteEvt := fx.buildSyntheticIncomplete(state, roundOut, "upstream_error", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt, identityState)
			return
		}
		// Loop back to scanOneRound with the new response.
	}
}

func (fx *codexContinueFoldContext) maxFoldAttempts() int {
	if fx == nil || fx.cfg == nil || fx.cfg.MaxContinue <= 0 {
		return 4
	}
	return fx.cfg.MaxContinue
}

func codexFoldBodyHasCompactionTrigger(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "compaction_trigger" {
				return true
			}
		}
	}
	return gjson.GetBytes(body, "type").String() == "compaction_trigger"
}

// openContinuationRound opens the next upstream round through the configured
// transport. HTTP uses the CodexExecutor request path; WebSocket callers inject
// their own opener so the fold state machine stays transport-neutral.
func (fx *codexContinueFoldContext) openContinuationRound(
	ctx context.Context,
	body []byte,
) (*codexContinueRound, error) {
	if fx.openContinuation != nil {
		return fx.openContinuation(ctx, body)
	}
	return fx.openHTTPContinuationRound(ctx, body)
}

// openHTTPContinuationRound builds + sends a continuation request through the
// same cacheHelper + applyCodexHeaders + httpClient path the original round
// used. Returns the open streaming response (caller closes the body).
func (fx *codexContinueFoldContext) openHTTPContinuationRound(
	ctx context.Context,
	body []byte,
) (*codexContinueRound, error) {
	if fx.executor == nil {
		return nil, fmt.Errorf("codex continue: HTTP continuation executor is nil")
	}
	httpReq, _, identityState, err := fx.executor.cacheHelper(ctx, fx.from, fx.url, fx.auth, fx.req, fx.baseBody, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, fx.auth, fx.apiKey, true, fx.config())
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	httpResp, err := fx.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, fx.config(), httpResp.StatusCode, httpResp.Header.Clone())
	return &codexContinueRound{
		body:          httpResp.Body,
		identityState: identityState,
		statusCode:    httpResp.StatusCode,
	}, nil
}

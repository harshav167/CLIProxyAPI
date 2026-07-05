package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexContinueFoldContext bundles the closures a fold loop needs to open
// continuation rounds: rebuilding the upstream request through the same
// cacheHelper/headers/httpClient path the original round used. Keeping it
// in one struct lets the fold helper stay a pure method with no surprises.
type codexContinueFoldContext struct {
	cfg             *config.CodexContinueConfig
	executor        *CodexExecutor
	reqCtx          context.Context
	auth            *cliproxyauth.Auth
	req             cliproxyexecutor.Request
	opts            cliproxyexecutor.Options
	baseModel       string
	from            sdktranslator.Format
	to              sdktranslator.Format
	originalPayload []byte
	baseBody        []byte // agent's translated request body (post-thinking, pre-round-shape)
	url             string
	apiKey          string
	identityState   codexIdentityConfuseState
	httpClient      *http.Client
	streamChunkCh   chan<- cliproxyexecutor.StreamChunk
	translatorParam *any
	responseFormat  sdktranslator.Format
}

// codexContinueFoldOutput is the per-round decision returned by scanOneRound.
type codexContinueFoldOutput struct {
	terminalEvent  []byte // raw `data: <json>` line for response.completed / response.failed / response.incomplete
	terminalType   string
	reasoningItems []map[string]any // reasoning items collected this round (for replay on continue)
	usage          map[string]any   // response.usage from terminal event
}

// scanOneRound reads one upstream SSE stream and forwards every event
// downstream via the translator. When fold is inactive, it replicates the
// legacy goroutine verbatim: collects output_item.done items, patches
// response.completed output, surfaces stream errors, caches reasoning
// replay. When fold is active, it classifies each output_index as either
// "reasoning" (forward live) or "buffered" (held back until we know whether
// the round is truncated), and on the terminal event returns the raw
// terminal line + collected reasoning + usage for the fold driver.
func (fx *codexContinueFoldContext) scanOneRound(
	ctx context.Context,
	httpResp *http.Response,
	identityState codexIdentityConfuseState,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	replayScope codexReasoningReplayScope,
	body []byte,
	// forwardEvent forwards one translated SSE line downstream.
	forwardEvent func(line []byte),
) codexContinueFoldOutput {
	_ = identityState // already applied per-line by the caller

	var result codexContinueFoldOutput
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800) // 50MB — matches the legacy scanner

	// Legacy-only state: output_item.done collection + completion patching.
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte

	itemKind := map[int64]string{} // upstream output_index → "reasoning" | "buffered"
	oiMap := map[int64]int64{}     // upstream output_index → downstream output_index
	_ = oiMap                      // reasoning keeps its own output_index when fold is active

	for scanner.Scan() {
		rawLine := scanner.Bytes()
		line := applyCodexIdentityConfuseResponsePayload(rawLine, identityState)
		helps.AppendAPIResponseChunk(ctx, fx.executor.cfg, line)
		translatedLine := bytes.Clone(line)

		if bytes.HasPrefix(line, dataTag) {
			data := bytes.TrimSpace(line[5:])

			// Legacy: surface terminal stream errors as chunk errors.
			if streamErr, terminalBody, ok := codexTerminalStreamErr(data); ok {
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
					helps.RecordAPIResponseError(ctx, fx.executor.cfg, errClearReplay)
					reporter.PublishFailure(ctx, errClearReplay)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errClearReplay}:
					case <-ctx.Done():
					}
					return result
				}
				helps.RecordAPIResponseError(ctx, fx.executor.cfg, streamErr)
				reporter.PublishFailure(ctx, streamErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return result
			}

			eventType := gjson.GetBytes(data, "type").String()

			// Terminal event → record + break out (fold path) OR handle
			// legacy-style and forward.
			if eventType == "response.completed" || eventType == "response.failed" ||
				eventType == "response.incomplete" || eventType == "error" {
				if !fx.foldActive() {
					// Legacy path: handle response.completed inline.
					if eventType == "response.completed" {
						if detail, ok := helps.ParseCodexUsage(data); ok {
							reporter.Publish(ctx, detail)
						}
						publishCodexImageToolUsage(ctx, reporter, body, data)
						data = patchCodexCompletedOutput(data, outputItemsByIndex, outputItemsFallback)
						cacheCodexReasoningReplayFromCompleted(replayScope, data)
						translatedLine = append([]byte("data: "), data...)
					}
					forwardEvent(translatedLine)
					return result
				}
				// Fold path: record terminal + usage + reasoning for the driver.
				result.terminalEvent = append([]byte("data: "), data...)
				result.terminalType = eventType
				if u := gjson.GetBytes(data, "response.usage"); u.Exists() {
					_ = json.Unmarshal([]byte(u.Raw), &result.usage)
				}
				return result
			}

			// Fold-path classification (only matters when fold is active).
			if fx.foldActive() && eventType == "response.output_item.added" {
				itemType := gjson.GetBytes(data, "item.type").String()
				upOI := gjson.GetBytes(data, "output_index").Int()
				if itemType == "reasoning" {
					itemKind[upOI] = "reasoning"
					oiMap[upOI] = upOI
					forwardEvent(translatedLine)
					continue
				}
				itemKind[upOI] = "buffered"
				// Do NOT forward buffered items when fold is active — they
				// may be discarded if the round is truncated.
				continue
			}

			// Legacy: collect output_item.done for response.completed patching.
			if eventType == "response.output_item.done" {
				collectCodexOutputItemDone(data, outputItemsByIndex, &outputItemsFallback)
				// Fold path: also record reasoning items for replay.
				if fx.foldActive() {
					item := gjson.GetBytes(data, "item")
					if item.Exists() && item.IsObject() {
						if gjson.GetBytes(data, "item.type").String() == "reasoning" {
							var m map[string]any
							_ = json.Unmarshal([]byte(item.Raw), &m)
							if m != nil {
								result.reasoningItems = append(result.reasoningItems, m)
							}
						}
					}
				}
			}

			// Fold path: drop buffered-item events.
			if fx.foldActive() {
				upOI := gjson.GetBytes(data, "output_index").Int()
				if kind, ok := itemKind[upOI]; ok && kind == "buffered" {
					continue
				}
			}

			forwardEvent(translatedLine)
			continue
		}

		// Non-data lines (event:, comments, blank) — forward as-is.
		forwardEvent(translatedLine)
	}
	if errScan := scanner.Err(); errScan != nil {
		helps.RecordAPIResponseError(ctx, fx.executor.cfg, errScan)
		reporter.PublishFailure(ctx, errScan)
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
		case <-ctx.Done():
		}
	}
	return result
}

// foldActive reports whether the fold is enabled for this request.
func (fx *codexContinueFoldContext) foldActive() bool {
	return fx.cfg != nil && fx.cfg.Enabled
}

// runFoldLoop drives the multi-round fold. Round 1's httpResp is already open
// (caller opened it). Continuation rounds are opened here via openContinuationRound.
//
// Returns when the response is finished (clean or stopped). All chunks are
// written to out. The caller closes out and httpResp.Body.
//
// When fold is disabled, this is a thin wrapper around scanOneRound that
// preserves the legacy behavior exactly.
func (fx *codexContinueFoldContext) runFoldLoop(
	ctx context.Context,
	firstResp *http.Response,
	firstIdentityState codexIdentityConfuseState,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	replayScope codexReasoningReplayScope,
) {
	defer func() {
		if errClose := firstResp.Body.Close(); errClose != nil {
			log.Errorf("codex continue: close response body error: %v", errClose)
		}
	}()

	// forwardEvent translates one SSE line and writes the resulting chunks
	// to out, respecting ctx cancellation. Mirrors the legacy goroutine body.
	var param any
	fx.translatorParam = &param
	forwardEvent := func(line []byte) {
		line = applyCodexIdentityExposeResponsePayload(line, firstIdentityState)
		chunks := sdktranslator.TranslateStream(ctx, fx.to, fx.responseFormat, fx.req.Model,
			fx.originalPayload, fx.baseBody, line, fx.translatorParam)
		for i := range chunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
			case <-ctx.Done():
				return
			}
		}
	}

	if !fx.foldActive() {
		// Legacy path: scan + forward, no buffering, no continuation.
		fx.scanOneRound(ctx, firstResp, firstIdentityState, out, reporter, replayScope, fx.baseBody, forwardEvent)
		return
	}

	// Fold active: multi-round loop.
	origInput := gjson.GetBytes(fx.baseBody, "input")
	var origInputItems []any
	if origInput.Exists() && origInput.IsArray() {
		_ = json.Unmarshal([]byte(origInput.Raw), &origInputItems)
	}
	var replayTail []any
	roundNo := 0
	totalOutputTokens := 0
	resp := firstResp
	identityState := firstIdentityState
	var lastReasoningItems []map[string]any

	for {
		roundNo++
		roundOut := fx.scanOneRound(ctx, resp, identityState, out, reporter, replayScope, fx.baseBody, forwardEvent)
		if roundOut.terminalEvent == nil {
			// Upstream EOF with no terminal event — emit incomplete.
			log.Warnf("codex continue: round %d upstream EOF no terminal event", roundNo)
			incompleteEvt := fx.buildSyntheticIncomplete(roundOut, "upstream_eof", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt)
			return
		}

		// Parse usage.reasoning_tokens from the terminal event.
		tokens, _ := helps.ReasoningTokens(roundOut.terminalEvent)
		totalOutputTokens += tokens
		// Publish usage for round.
		if detail, ok := helps.ParseCodexUsage(roundOut.terminalEvent); ok {
			reporter.Publish(ctx, detail)
		}
		lastReasoningItems = roundOut.reasoningItems

		// Truncation check.
		step := fx.cfg.TruncationStep
		if step <= 0 {
			step = helps.CodexContinueDefaultStep
		}
		hasEnc := helps.HasEncryptedReasoning(roundOut.reasoningItems)
		shouldCont := fx.cfg.Enabled &&
			helps.ShouldContinue(tokens, step, fx.cfg.MinN, fx.cfg.MaxN) &&
			hasEnc &&
			(fx.cfg.MaxContinue == 0 || roundNo <= fx.cfg.MaxContinue) &&
			(fx.cfg.MaxTotalOutputTokens == 0 || totalOutputTokens < fx.cfg.MaxTotalOutputTokens)

		if !shouldCont {
			// Stopped. Was it because of a guard while still truncated?
			stoppedReason := ""
			if helps.IsTruncationPattern(tokens, step) {
				stoppedReason = helps.StoppedReasonWhenTruncated(
					tokens, step, hasEnc, roundNo,
					fx.cfg.MaxContinue, totalOutputTokens, fx.cfg.MaxTotalOutputTokens)
			}
			_ = stoppedReason // metadata-only for now; TODO: surface in proxy metadata
			// Forward the terminal event downstream as-is.
			forwardEvent(roundOut.terminalEvent)
			// Cache reasoning replay from the completed event (same as legacy).
			if roundOut.terminalType == "response.completed" {
				cacheCodexReasoningReplayFromCompleted(replayScope, roundOut.terminalEvent[5:])
			}
			return
		}

		// Continue: open another round with replayed reasoning + commentary marker.
		log.Infof("codex continue: round %d truncated at reasoning_tokens=%d (tier n=%d), opening continuation",
			roundNo, tokens, helps.TierN(tokens, step))

		// Build continuation marker items.
		var markerItems []any
		if fx.cfg.Method == helps.CodexContinueMethodToolPair {
			// tool_pair (legacy): synthetic function_call + function_call_output.
			lastID := ""
			if len(lastReasoningItems) > 0 {
				if id, ok := lastReasoningItems[len(lastReasoningItems)-1]["id"].(string); ok {
					lastID = id
				}
			}
			call, callOut := helps.ContinuePair(lastID, "continue_thinking",
				"Please continue thinking about the query.")
			markerItems = []any{call, callOut}
		} else {
			// commentary (default): single phase:"commentary" assistant message.
			markerItems = []any{helps.CommentaryMessage(fx.cfg.MarkerText)}
		}

		// Replay tail = this round's reasoning items + marker.
		for _, r := range lastReasoningItems {
			replayTail = append(replayTail, r)
		}
		replayTail = append(replayTail, markerItems...)

		// Build the continuation request body from the agent's original
		// translated body (baseBody), with our replayed input.
		inputItems := append(append([]any{}, origInputItems...), replayTail...)
		contBody := helps.BuildContinuationPayload(fx.baseBody, inputItems,
			true /*force_include_encrypted*/)

		// Open the continuation request through the same path as round 1.
		var errOpen error
		resp, identityState, errOpen = fx.openContinuationRound(ctx, contBody)
		if errOpen != nil {
			log.Warnf("codex continue: round %d continuation open failed: %v", roundNo+1, errOpen)
			incompleteEvt := fx.buildSyntheticIncomplete(roundOut, "upstream_error", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt)
			return
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			log.Warnf("codex continue: round %d continuation HTTP %d: %s",
				roundNo+1, resp.StatusCode, string(body[:min(len(body), 2000)]))
			incompleteEvt := fx.buildSyntheticIncomplete(roundOut, "upstream_error", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt)
			return
		}
		// Loop back to scanOneRound with the new response.
	}
}

// openContinuationRound builds + sends a continuation request through the
// same cacheHelper + applyCodexHeaders + httpClient path the original round
// used. Returns the open streaming response (caller closes the body).
func (fx *codexContinueFoldContext) openContinuationRound(
	ctx context.Context,
	body []byte,
) (*http.Response, codexIdentityConfuseState, error) {
	httpReq, _, identityState, err := fx.executor.cacheHelper(ctx, fx.from, fx.url, fx.auth, fx.req, fx.baseBody, body)
	if err != nil {
		return nil, codexIdentityConfuseState{}, err
	}
	applyCodexHeaders(httpReq, fx.auth, fx.apiKey, true, fx.executor.cfg)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	httpResp, err := fx.httpClient.Do(httpReq)
	if err != nil {
		return nil, codexIdentityConfuseState{}, err
	}
	helps.RecordAPIResponseMetadata(ctx, fx.executor.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	return httpResp, identityState, nil
}

// buildSyntheticIncomplete constructs a response.incomplete event carrying
// proxy metadata about why the fold stopped. Used when continuation fails or
// upstream EOFs mid-stream.
func (fx *codexContinueFoldContext) buildSyntheticIncomplete(
	roundOut codexContinueFoldOutput,
	reason string,
	roundNo int,
	totalOutputTokens int,
) []byte {
	resp := map[string]any{
		"status":             "incomplete",
		"incomplete_details": map[string]any{"reason": reason},
	}
	if len(roundOut.usage) > 0 {
		resp["usage"] = roundOut.usage
	}
	resp["metadata"] = map[string]any{
		"proxy_stopped_reason": reason,
		"proxy_round":          roundNo,
	}
	payload, _ := json.Marshal(map[string]any{
		"type":     "response.incomplete",
		"response": resp,
	})
	out, _ := sjson.SetBytes([]byte{}, "type", "response.incomplete")
	_ = out
	formatted := append([]byte("data: "), payload...)
	return formatted
}

// codexContinueEnabled reports whether the fold is configured and enabled.
// Used by the executor to decide whether to enter the fold path at all.
func codexContinueEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.CodexContinueThinking != nil && cfg.CodexContinueThinking.Enabled
}

// min returns the smaller of a, b (Go 1.21+ has builtin min; this is a fallback
// for older Go or to avoid depending on the version).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensure import usage for sync (used by sync.Once-style guards if needed in future).
var _ sync.WaitGroup

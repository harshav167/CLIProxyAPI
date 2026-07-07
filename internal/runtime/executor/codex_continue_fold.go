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

type codexContinueRoundOpener func(ctx context.Context, body []byte) (*codexContinueRound, error)
type codexContinueForwardEvent func(line []byte, identityState codexIdentityConfuseState)
type codexContinueAppendResponse func(ctx context.Context, payload []byte)

// codexBufferedItem holds the ordered raw SSE lines for one buffered
// (message | function_call) output_index. The fold holds these back during a
// round because a truncated round's tentative output must be discarded; on a
// clean terminal they are flushed downstream in arrival order (mirrors the
// reference CodexCont `out_buffer` + `_flush_entry`).
type codexBufferedItem struct {
	outputIndex int64
	lines       [][]byte // raw upstream SSE lines (already identity-confused), in arrival order
}

// codexContinueFoldOutput is the per-round decision returned by scanOneRound.
type codexContinueFoldOutput struct {
	terminalEvent  []byte // raw `data: <json>` line for response.completed / response.failed / response.incomplete
	terminalType   string
	reasoningItems []map[string]any // reasoning items collected this round (for replay on continue)
	usage          map[string]any   // response.usage from terminal event
	// bufferedItems holds the round's tentative (message | function_call)
	// output, buffered rather than forwarded live. On a clean terminal the
	// driver flushes these downstream (they ARE the final answer); on a
	// truncated round it discards them. Without this, the final answer never
	// reaches the client and the turn renders empty.
	bufferedItems []*codexBufferedItem
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
	roundBody io.Reader,
	identityState codexIdentityConfuseState,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	replayScope codexReasoningReplayScope,
	body []byte,
	// forwardEvent forwards one translated SSE line downstream. It receives the
	// identity state for the current round so expose/de-obfuscation uses the
	// same mapping the round's request was confused with.
	forwardEvent func(line []byte, identityState codexIdentityConfuseState),
) codexContinueFoldOutput {
	// Identity state is used below to ensure any remaining original keys in the
	// upstream response are consistently confused before forwardEvent exposes them
	// back with the same round's identity state.

	var result codexContinueFoldOutput
	scanner := bufio.NewScanner(roundBody)
	scanner.Buffer(nil, 52_428_800) // 50MB — matches the legacy scanner

	// Legacy-only state: output_item.done collection + completion patching.
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte

	itemKind := map[int64]string{} // upstream output_index → "reasoning" | "buffered"

	for scanner.Scan() {
		rawLine := scanner.Bytes()
		line := applyCodexIdentityConfuseResponsePayload(rawLine, identityState)
		fx.appendResponsePayload(ctx, line)
		translatedLine := bytes.Clone(line)

		if bytes.HasPrefix(line, dataTag) {
			data := bytes.TrimSpace(line[5:])

			// Legacy: surface terminal stream errors as chunk errors.
			if streamErr, terminalBody, ok := codexTerminalStreamErr(data); ok {
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
					helps.RecordAPIResponseError(ctx, fx.config(), errClearReplay)
					if reporter != nil {
						reporter.PublishFailure(ctx, errClearReplay)
					}
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errClearReplay}:
					case <-ctx.Done():
					}
					return result
				}
				helps.RecordAPIResponseError(ctx, fx.config(), streamErr)
				if reporter != nil {
					reporter.PublishFailure(ctx, streamErr)
				}
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
					forwardEvent(translatedLine, identityState)
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
			//
			// Only `message` items are BUFFERED (held back, discardable on
			// truncation). Everything else streams LIVE:
			//   - reasoning: always live (the thinking panel).
			//   - function_call / custom_tool_call: live so agentic tool use
			//     fires in real time instead of serializing behind round
			//     termination. Fixture evidence (CodexCont R1 reasoning_tokens=516
			//     and R2 reasoning_tokens=2588, both truncated) shows truncated
			//     rounds emit ONLY reasoning + message — never a tool call — so
			//     streaming tool calls live does not leak a truncated-round side
			//     effect. Paranoid deployments can opt back into buffering via
			//     cfg.BufferToolCalls.
			//   - unknown types: live (default). Streaming an unknown item beats
			//     silently buffering-then-dropping a type OpenAI may add later.
			if fx.foldActive() && eventType == "response.output_item.added" {
				itemType := gjson.GetBytes(data, "item.type").String()
				upOI := gjson.GetBytes(data, "output_index").Int()
				if codexFoldItemBuffered(itemType, fx.cfg) {
					itemKind[upOI] = "buffered"
					// Held back — may be discarded on truncation, flushed on a
					// clean terminal. Store the identity-confused line;
					// forwardEvent applies identity-expose at flush time.
					entry := &codexBufferedItem{outputIndex: upOI, lines: [][]byte{bytes.Clone(line)}}
					result.bufferedItems = append(result.bufferedItems, entry)
					continue
				}
				itemKind[upOI] = "live"
				forwardEvent(translatedLine, identityState)
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

			// Fold path: buffer subsequent events for a buffered item (content
			// parts, output_text deltas, output_item.done) rather than
			// forwarding them live. They flush together on a clean terminal.
			if fx.foldActive() {
				upOI := gjson.GetBytes(data, "output_index").Int()
				if kind, ok := itemKind[upOI]; ok && kind == "buffered" {
					if entry := findCodexBufferedItem(result.bufferedItems, upOI); entry != nil {
						entry.lines = append(entry.lines, bytes.Clone(line))
					}
					continue
				}
			}

			forwardEvent(translatedLine, identityState)
			continue
		}

		// Non-data lines (event:, comments, blank) — forward as-is.
		forwardEvent(translatedLine, identityState)
	}
	if errScan := scanner.Err(); errScan != nil {
		helps.RecordAPIResponseError(ctx, fx.config(), errScan)
		if reporter != nil {
			reporter.PublishFailure(ctx, errScan)
		}
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
		case <-ctx.Done():
		}
	}
	return result
}

// codexFoldItemBuffered reports whether a fold-active round should BUFFER an
// output item of the given type (hold it back, discardable on truncation) or
// stream it LIVE.
//
//   - message         → buffered (the discardable tentative final answer).
//   - function_call   → live by default; buffered only if cfg.BufferToolCalls.
//   - custom_tool_call → live by default; buffered only if cfg.BufferToolCalls.
//   - reasoning        → live (never buffered).
//   - anything else    → live (stream unknown types rather than drop them).
//
// Buffering only the message minimizes the fold's blast radius: reasoning and
// tool calls stream in real time; only the prose answer pays the buffer tax,
// and only because a truncated round's answer must be discardable and SSE
// cannot unsend.
func codexFoldItemBuffered(itemType string, cfg *config.CodexContinueConfig) bool {
	switch itemType {
	case "message":
		return true
	case "function_call", "custom_tool_call":
		return cfg != nil && cfg.BufferToolCalls
	default:
		return false
	}
}

const codexRechunkDefaultSize = 24

// rechunkCodexBufferedMessage re-slices the buffered message's
// response.output_text.delta run into uniform chunks so the flush streams
// smoothly instead of bursting. Non-delta lines (output_item.added,
// content_part.added/done, output_item.done, and any others) are preserved in
// their original order and position; only the contiguous run of
// output_text.delta lines is replaced.
//
// The re-emitted delta lines clone the FIRST original delta line's structure
// (item_id, content_index, output_index, type) and replace only the `delta`
// field, so downstream translation sees well-formed codex delta events. Slicing
// is by rune to avoid splitting multi-byte characters (the fixture answer is
// CJK). Mirrors the reference CodexCont `_flush_entry` rechunk path.
func rechunkCodexBufferedMessage(lines [][]byte, size int) [][]byte {
	if size <= 0 {
		size = codexRechunkDefaultSize
	}
	// Find the delta run and collect merged text + a template delta line.
	var template []byte
	var merged strings.Builder
	deltaIdx := -1 // position of the first delta line in the output
	out := make([][]byte, 0, len(lines))
	for _, line := range lines {
		payload := codexDataPayload(line)
		if payload != nil && gjson.GetBytes(payload, "type").String() == "response.output_text.delta" {
			if template == nil {
				template = bytes.Clone(payload)
				deltaIdx = len(out)
				out = append(out, nil) // placeholder; filled after we know the full text
			}
			merged.WriteString(gjson.GetBytes(payload, "delta").String())
			continue
		}
		out = append(out, line)
	}
	if template == nil {
		// No text deltas (e.g. a tool_call buffered under BufferToolCalls, or an
		// empty message) — nothing to rechunk, return as-is.
		return lines
	}

	// Build the re-sliced delta lines from the merged text.
	text := []rune(merged.String())
	sliced := make([][]byte, 0, len(text)/size+1)
	for i := 0; i < len(text); i += size {
		end := i + size
		if end > len(text) {
			end = len(text)
		}
		newLine, err := sjson.SetBytes(template, "delta", string(text[i:end]))
		if err != nil {
			// Fall back to verbatim on any rewrite error — never drop the answer.
			return lines
		}
		sliced = append(sliced, append([]byte("data: "), newLine...))
	}
	if len(sliced) == 0 {
		// Empty text — drop the placeholder, keep the rest.
		return append(out[:deltaIdx], out[deltaIdx+1:]...)
	}

	// Splice the sliced deltas into the placeholder position.
	result := make([][]byte, 0, len(out)+len(sliced)-1)
	result = append(result, out[:deltaIdx]...)
	result = append(result, sliced...)
	result = append(result, out[deltaIdx+1:]...)
	return result
}

// codexDataPayload returns the JSON payload of a `data: {...}` SSE line, or nil
// for non-data lines ([DONE], event:, comments, blanks).
func codexDataPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, dataTag) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len(dataTag):])
	if len(payload) == 0 || payload[0] != '{' {
		return nil
	}
	return payload
}

// findCodexBufferedItem returns the buffer entry for the given upstream
// output_index, or nil. Mirrors the reference CodexCont `_find_buffer`.
func findCodexBufferedItem(items []*codexBufferedItem, outputIndex int64) *codexBufferedItem {
	for _, entry := range items {
		if entry.outputIndex == outputIndex {
			return entry
		}
	}
	return nil
}

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

	// Fold active: multi-round loop.
	// Ownership: firstBody belongs to the caller.
	// Every continuation response opened by this loop must be closed by this loop.
	origInput := gjson.GetBytes(fx.baseBody, "input")
	var origInputItems []any
	if origInput.Exists() && origInput.IsArray() {
		_ = json.Unmarshal([]byte(origInput.Raw), &origInputItems)
	}
	roundNo := 0
	totalOutputTokens := 0
	round := &codexContinueRound{body: firstBody, identityState: firstIdentityState}
	identityState := firstIdentityState
	var lastReasoningItems []map[string]any

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
		roundOut := fx.scanOneRound(ctx, round.body, identityState, out, reporter, replayScope, fx.baseBody, forwardEvent)
		if roundOut.terminalEvent == nil {
			// Upstream EOF with no terminal event — emit incomplete.
			log.Warnf("codex continue: round %d upstream EOF no terminal event", roundNo)
			incompleteEvt := fx.buildSyntheticIncomplete(roundOut, "upstream_eof", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt, identityState)
			closeContinuationRound(round)
			return
		}

		// Parse usage.reasoning_tokens from the terminal event. terminalEvent is
		// stored with the leading "data: " SSE framing; strip it before parsing.
		terminalPayload := bytes.TrimSpace(roundOut.terminalEvent)
		if bytes.HasPrefix(terminalPayload, []byte("data:")) {
			terminalPayload = bytes.TrimSpace(terminalPayload[5:])
		}
		tokens, _ := helps.ReasoningTokens(terminalPayload)
		totalOutputTokens += tokens
		// Publish usage for round.
		if detail, ok := helps.ParseCodexUsage(terminalPayload); ok {
			if reporter != nil {
				reporter.Publish(ctx, detail)
			}
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
			// Clean stop: flush this round's buffered tentative output as the
			// real answer BEFORE the terminal event. These message/function_call
			// items were held back during the round (they'd be discarded if the
			// round had truncated); on a clean terminal they ARE the final
			// answer. Without this flush the client sees reasoning + an empty
			// terminal and renders the turn as no output. Mirrors the reference
			// CodexCont `for entry in out_buffer: yield _flush_entry(...)`.
			//
			// RechunkFinalAnswer smooths the flush: instead of replaying the
			// upstream's original output_text.delta boundaries back-to-back (a
			// burst), it re-slices the merged answer text into uniform chunks so
			// the answer streams like a normal response.
			for _, entry := range roundOut.bufferedItems {
				lines := entry.lines
				if fx.cfg != nil && fx.cfg.RechunkFinalAnswer {
					lines = rechunkCodexBufferedMessage(lines, fx.cfg.RechunkSize)
				}
				for _, bufLine := range lines {
					forwardEvent(bufLine, identityState)
				}
			}
			// Forward the terminal event downstream as-is.
			forwardEvent(roundOut.terminalEvent, identityState)
			// Cache reasoning replay from the completed event (same as legacy).
			if roundOut.terminalType == "response.completed" {
				cacheCodexReasoningReplayFromCompleted(replayScope, roundOut.terminalEvent[5:])
			}
			closeContinuationRound(round)
			return
		}

		// Continue: open another round with replayed reasoning + commentary marker.
		log.Infof("codex continue: round %d truncated at reasoning_tokens=%d (tier n=%d), opening continuation",
			roundNo, tokens, helps.TierN(tokens, step))

		// Build continuation marker: a single phase:"commentary" assistant message.
		markerItems := []any{helps.CommentaryMessage(fx.cfg.MarkerText)}

		// Replay tail for this round only: the last round's reasoning items +
		// one marker. Do not accumulate across rounds — CodexCont only replays
		// the latest truncated reasoning, and carrying history would duplicate
		// markers and grow the payload quadratically.
		replayTail := make([]any, 0, len(lastReasoningItems)+len(markerItems))
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
		prevRound := round
		round, errOpen = fx.openContinuationRound(ctx, contBody)
		if round != nil {
			identityState = round.identityState
		}
		// Continuation rounds beyond the first are owned by this loop. Round 1
		// is owned by the caller.
		closeContinuationRound(prevRound)
		if errOpen != nil {
			log.Warnf("codex continue: round %d continuation open failed: %v", roundNo+1, errOpen)
			incompleteEvt := fx.buildSyntheticIncomplete(roundOut, "upstream_error", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt, identityState)
			return
		}
		if round.statusCode >= 400 {
			body, _ := io.ReadAll(round.body)
			closeContinuationRound(round)
			log.Warnf("codex continue: round %d continuation HTTP %d: %s",
				roundNo+1, round.statusCode, string(body[:min(len(body), 2000)]))
			incompleteEvt := fx.buildSyntheticIncomplete(roundOut, "upstream_error", roundNo, totalOutputTokens)
			forwardEvent(incompleteEvt, identityState)
			return
		}
		// Loop back to scanOneRound with the new response.
	}
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
	formatted := append([]byte("data: "), payload...)
	return formatted
}

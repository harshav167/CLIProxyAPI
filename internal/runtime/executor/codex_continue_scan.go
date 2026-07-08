package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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
	terminalErr    error
	responseID     string
	reasoningItems []map[string]any // reasoning items collected this round (for replay on continue)
	usage          map[string]any   // response.usage from terminal event
	// bufferedItems holds the round's tentative (message | unknown |
	// opt-in function_call) output, buffered rather than forwarded live.
	// On a clean terminal the
	// driver flushes these downstream (they ARE the final answer); on a
	// truncated round it discards them. Without this, the final answer never
	// reaches the client and the turn renders empty.
	bufferedItems []*codexBufferedItem
	liveItems     []*codexBufferedItem
}

func (fx *codexContinueFoldContext) scanFoldRound(
	ctx context.Context,
	roundBody io.Reader,
	identityState codexIdentityConfuseState,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	replayScope codexReasoningReplayScope,
	state *codexFoldState,
	forwardEvent func(line []byte, identityState codexIdentityConfuseState),
) codexContinueFoldOutput {
	return fx.scanOneRound(ctx, roundBody, identityState, out, reporter, replayScope, fx.baseBody, func(line []byte, stateForLine codexIdentityConfuseState) {
		payload := codexDataPayload(line)
		if payload != nil {
			eventType := gjson.GetBytes(payload, "type").String()
			if eventType == "response.created" || eventType == "response.in_progress" {
				originalID := gjson.GetBytes(payload, "response.id").String()
				if state != nil && state.responseIdentity.visibleResponseID != "" && originalID != "" && originalID != state.responseIdentity.visibleResponseID {
					return
				}
				payload = codexRewriteLifecyclePayload(payload, state)
				forwardEvent(append([]byte("data: "), payload...), stateForLine)
				return
			}
			if eventType == "response.output_item.done" && gjson.GetBytes(payload, "item.type").String() == "reasoning" {
				payload, _ = sjson.SetBytes(payload, "output_index", state.nextOutputIndex)
				state.nextOutputIndex++
				state.noteForwardedPayload(payload)
				forwardEvent(append([]byte("data: "), payload...), stateForLine)
				return
			}
			state.noteForwardedPayload(payload)
		}
		forwardEvent(line, stateForLine)
	})
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

	itemKind := map[int64]string{} // upstream output_index → "reasoning" | "buffered" | "live"

	for scanner.Scan() {
		rawLine := scanner.Bytes()
		line := applyCodexIdentityConfuseResponsePayload(rawLine, identityState)
		fx.appendResponsePayload(ctx, line)
		translatedLine := bytes.Clone(line)

		if bytes.HasPrefix(line, dataTag) {
			data := bytes.TrimSpace(line[5:])

			eventType := gjson.GetBytes(data, "type").String()

			// Legacy: surface terminal stream errors as chunk errors.
			if streamErr, terminalBody, ok := codexTerminalStreamErr(data); ok {
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
					helps.RecordAPIResponseError(ctx, fx.config(), errClearReplay)
					if reporter != nil {
						reporter.PublishFailure(ctx, errClearReplay)
					}
					if fx.foldActive() {
						result.terminalEvent = append([]byte("data: "), data...)
						result.terminalType = eventType
						result.terminalErr = errClearReplay
						result.responseID = gjson.GetBytes(data, "response.id").String()
						if u := gjson.GetBytes(data, "response.usage"); u.Exists() {
							_ = json.Unmarshal([]byte(u.Raw), &result.usage)
						}
						return result
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
				if fx.foldActive() {
					result.terminalEvent = append([]byte("data: "), data...)
					result.terminalType = eventType
					result.terminalErr = streamErr
					result.responseID = gjson.GetBytes(data, "response.id").String()
					if u := gjson.GetBytes(data, "response.usage"); u.Exists() {
						_ = json.Unmarshal([]byte(u.Raw), &result.usage)
					}
					return result
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return result
			}

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
				result.responseID = gjson.GetBytes(data, "response.id").String()
				if u := gjson.GetBytes(data, "response.usage"); u.Exists() {
					_ = json.Unmarshal([]byte(u.Raw), &result.usage)
				}
				return result
			}

			// Fold-path classification (only matters when fold is active).
			//
			// `message` and unknown item types are BUFFERED (held back,
			// discardable on truncation). Everything else streams LIVE:
			//   - reasoning: always live (the thinking panel).
			//   - function_call / custom_tool_call: live so agentic tool use
			//     fires in real time instead of serializing behind round
			//     termination. Fixture evidence (CodexCont R1 reasoning_tokens=516
			//     and R2 reasoning_tokens=2588, both truncated) shows truncated
			//     rounds emit ONLY reasoning + message — never a tool call — so
			//     streaming tool calls live does not leak a truncated-round side
			//     effect. Paranoid deployments can opt back into buffering via
			//     cfg.BufferToolCalls or cfg.OutputCommitPolicy="on_clean_terminal".
			//   - unknown types: BUFFERED (default). A truncated round's tentative
			//     output is discarded; buffering unknowns prevents leaking an
			//     unrecallable side effect if OpenAI adds a new output type later.
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
				result.liveItems = append(result.liveItems, &codexBufferedItem{outputIndex: upOI, lines: [][]byte{bytes.Clone(line)}})
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
				if kind, ok := itemKind[upOI]; ok && kind == "live" {
					if entry := findCodexBufferedItem(result.liveItems, upOI); entry != nil {
						entry.lines = append(entry.lines, bytes.Clone(line))
					}
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
		if fx.foldActive() {
			result.terminalErr = errScan
			return result
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
// stream it LIVE. Messages and unknown future items buffer; executable tool
// calls stream live only under the default live-commit-barrier policy.
func codexFoldItemBuffered(itemType string, cfg *config.CodexContinueConfig) bool {
	switch itemType {
	case "message":
		return true
	case "function_call", "custom_tool_call":
		return cfg != nil && (cfg.BufferToolCalls || codexCommitPolicy(cfg) == codexCommitOnCleanTerminal)
	case "reasoning":
		return false
	default:
		return true
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

// codexSSEDataPayload strips leading `data:` SSE framing from a fold terminal
// line. Prefer this over a hard-coded [5:] slice so a leading space after
// `data:` cannot corrupt cached / parsed JSON.
func codexSSEDataPayload(line []byte) []byte {
	if payload := codexDataPayload(line); payload != nil {
		return payload
	}
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, dataTag) {
		return bytes.TrimSpace(trimmed[len(dataTag):])
	}
	return trimmed
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

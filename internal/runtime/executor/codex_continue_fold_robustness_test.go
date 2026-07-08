package executor

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type codexFoldTestRun struct {
	forwarded    [][]byte
	continuation [][]byte
	errs         []error
}

func runCodexFoldTest(t *testing.T, cfg *config.CodexContinueConfig, baseBody string, rounds ...string) codexFoldTestRun {
	t.Helper()
	run := runCodexFoldTestAllowErrors(t, cfg, baseBody, rounds...)
	if len(run.errs) > 0 {
		t.Fatalf("unexpected stream chunk error: %v", run.errs[0])
	}
	return run
}

func runCodexFoldTestAllowErrors(t *testing.T, cfg *config.CodexContinueConfig, baseBody string, rounds ...string) codexFoldTestRun {
	t.Helper()
	if len(rounds) == 0 {
		t.Fatal("need at least one upstream round")
	}
	if cfg == nil {
		cfg = &config.CodexContinueConfig{Enabled: true, MaxContinue: 4}
	}
	fx := &codexContinueFoldContext{
		cfg:        cfg,
		rootConfig: &config.Config{},
		baseBody:   []byte(baseBody),
		appendResponse: func(context.Context, []byte) {
		},
	}

	var run codexFoldTestRun
	nextRound := 1
	fx.openContinuation = func(_ context.Context, body []byte) (*codexContinueRound, error) {
		run.continuation = append(run.continuation, bytes.Clone(body))
		if nextRound >= len(rounds) {
			t.Fatalf("unexpected continuation request %d; payload=%s", nextRound, body)
		}
		round := &codexContinueRound{body: respBody(rounds[nextRound]), statusCode: 200}
		nextRound++
		return round, nil
	}

	out := make(chan cliproxyexecutor.StreamChunk, 16)
	fx.runFoldLoop(
		context.Background(),
		respBody(rounds[0]),
		codexIdentityConfuseState{},
		out,
		nil,
		codexReasoningReplayScope{},
		func(line []byte, _ codexIdentityConfuseState) {
			run.forwarded = append(run.forwarded, bytes.Clone(line))
		},
	)
	for {
		select {
		case chunk := <-out:
			if chunk.Err != nil {
				run.errs = append(run.errs, chunk.Err)
			}
		default:
			return run
		}
	}
}

func foldSSE(events ...string) string {
	return "data: " + strings.Join(events, "\ndata: ") + "\n"
}

func foldBaseBody(input string) string {
	return `{"model":"gpt-5-codex","stream":true,"input":[` + input + `]}`
}

func foldUserInput(text string) string {
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`
}

func foldCreated(id string, seq int) string {
	return `{"type":"response.created","sequence_number":` + strconv.Itoa(seq) + `,"response":{"id":"` + id + `","created_at":1,"model":"gpt-5-codex","status":"in_progress"}}`
}

func foldInProgress(id string, seq int) string {
	return `{"type":"response.in_progress","sequence_number":` + strconv.Itoa(seq) + `,"response":{"id":"` + id + `","status":"in_progress"}}`
}

func foldReasoningDone(index int, id string, encrypted string, seq int) string {
	return `{"type":"response.output_item.done","sequence_number":` + strconv.Itoa(seq) + `,"output_index":` + strconv.Itoa(index) + `,"item":{"id":"` + id + `","type":"reasoning","summary":[],"encrypted_content":"` + encrypted + `"}}`
}

func foldMessageEvents(index int, id string, text string, seq int) []string {
	idx := strconv.Itoa(index)
	return []string{
		`{"type":"response.output_item.added","sequence_number":` + strconv.Itoa(seq) + `,"output_index":` + idx + `,"item":{"id":"` + id + `","type":"message","role":"assistant"}}`,
		`{"type":"response.output_text.delta","sequence_number":` + strconv.Itoa(seq+1) + `,"output_index":` + idx + `,"item_id":"` + id + `","delta":"` + text + `"}`,
		`{"type":"response.output_item.done","sequence_number":` + strconv.Itoa(seq+2) + `,"output_index":` + idx + `,"item":{"id":"` + id + `","type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`,
	}
}

func foldCompleted(id string, inputTokens, cachedTokens, outputTokens, reasoningTokens, seq int) string {
	totalTokens := inputTokens + outputTokens
	return `{"type":"response.completed","sequence_number":` + strconv.Itoa(seq) + `,"response":{"id":"` + id + `","status":"completed","output":[],"usage":{"input_tokens":` + strconv.Itoa(inputTokens) + `,"input_tokens_details":{"cached_tokens":` + strconv.Itoa(cachedTokens) + `},"output_tokens":` + strconv.Itoa(outputTokens) + `,"output_tokens_details":{"reasoning_tokens":` + strconv.Itoa(reasoningTokens) + `},"total_tokens":` + strconv.Itoa(totalTokens) + `}}}`
}

func foldPayloadsOfType(t *testing.T, lines [][]byte, eventType string) [][]byte {
	t.Helper()
	var payloads [][]byte
	for _, line := range lines {
		payload := codexDataPayload(line)
		if payload == nil {
			continue
		}
		if gjson.GetBytes(payload, "type").String() == eventType {
			payloads = append(payloads, bytes.Clone(payload))
		}
	}
	return payloads
}

func lastFoldPayloadOfType(t *testing.T, lines [][]byte, eventType string) []byte {
	t.Helper()
	payloads := foldPayloadsOfType(t, lines, eventType)
	if len(payloads) == 0 {
		t.Fatalf("missing %s in:\n%s", eventType, bytes.Join(lines, []byte("\n")))
	}
	return payloads[len(payloads)-1]
}

func TestCodexContinueFoldAccumulatesReplayTailAcrossHiddenRounds(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1Events := append([]string{foldCreated("resp-1", 0), foldReasoningDone(0, "rs-1", "enc-1", 2)}, foldMessageEvents(1, "msg-1", "truncated one", 3)...)
	round1Events = append(round1Events, foldCompleted("resp-1", 100, 9, 516, 516, 9))
	round2Events := append([]string{foldCreated("resp-2", 0), foldReasoningDone(0, "rs-2", "enc-2", 2)}, foldMessageEvents(1, "msg-2", "truncated two", 3)...)
	round2Events = append(round2Events, foldCompleted("resp-2", 8, 8, 1034, 1034, 9))
	round3Events := append([]string{foldCreated("resp-3", 0)}, foldMessageEvents(0, "msg-3", "final answer", 1)...)
	round3Events = append(round3Events, foldCompleted("resp-3", 6, 6, 20, 20, 9))

	run := runCodexFoldTest(t, nil, baseBody, foldSSE(round1Events...), foldSSE(round2Events...), foldSSE(round3Events...))

	if len(run.continuation) != 2 {
		t.Fatalf("continuation requests = %d, want 2", len(run.continuation))
	}
	secondInput := gjson.GetBytes(run.continuation[1], "input").Array()
	if len(secondInput) != 5 {
		t.Fatalf("second continuation input len = %d, want original + rs1 + marker + rs2 + marker; payload=%s", len(secondInput), run.continuation[1])
	}
	if got := secondInput[1].Get("encrypted_content").String(); got != "enc-1" {
		t.Fatalf("first hidden reasoning = %q, want enc-1; payload=%s", got, run.continuation[1])
	}
	if got := secondInput[3].Get("encrypted_content").String(); got != "enc-2" {
		t.Fatalf("second hidden reasoning = %q, want enc-2; payload=%s", got, run.continuation[1])
	}
	if secondInput[2].Get("phase").String() != "commentary" || secondInput[4].Get("phase").String() != "commentary" {
		t.Fatalf("missing accumulated commentary markers; payload=%s", run.continuation[1])
	}
}

func TestCodexContinueFoldReconstructsTerminalIdentityOutputMetadataAndUsage(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1Events := append([]string{foldCreated("resp-visible", 0), foldInProgress("resp-visible", 1), foldReasoningDone(0, "rs-1", "enc-1", 2)}, foldMessageEvents(1, "msg-1", "discard me", 3)...)
	round1Events = append(round1Events, foldCompleted("resp-hidden-1", 100, 10, 516, 516, 9))
	round2Events := append([]string{foldCreated("resp-hidden-2", 0), foldInProgress("resp-hidden-2", 1), foldReasoningDone(0, "rs-2", "enc-2", 2)}, foldMessageEvents(1, "msg-2", "final answer", 3)...)
	round2Events = append(round2Events, foldCompleted("resp-hidden-2", 7, 7, 25, 20, 9))

	run := runCodexFoldTest(t, nil, baseBody, foldSSE(round1Events...), foldSSE(round2Events...))
	terminal := lastFoldPayloadOfType(t, run.forwarded, "response.completed")

	if got := gjson.GetBytes(terminal, "response.id").String(); got != "resp-visible" {
		t.Fatalf("visible response id = %q, want resp-visible; terminal=%s", got, terminal)
	}
	if got := len(gjson.GetBytes(terminal, "response.metadata.proxy_rounds").Array()); got != 2 {
		t.Fatalf("proxy_rounds len = %d, want 2; terminal=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.metadata.proxy_upstream_previous_response_id").String(); got != "resp-hidden-2" {
		t.Fatalf("upstream previous id = %q, want resp-hidden-2; terminal=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.usage.input_tokens").Int(); got != 100 {
		t.Fatalf("agent input_tokens = %d, want first round input; terminal=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.usage.output_tokens_details.reasoning_tokens").Int(); got != 536 {
		t.Fatalf("agent reasoning_tokens = %d, want summed reasoning; terminal=%s", got, terminal)
	}
	output := gjson.GetBytes(terminal, "response.output").Raw
	if !strings.Contains(output, "enc-1") || !strings.Contains(output, "enc-2") || !strings.Contains(output, "final answer") {
		t.Fatalf("terminal output missing folded committed items: %s", output)
	}
	if strings.Contains(output, "discard me") {
		t.Fatalf("terminal output leaked discarded truncated answer: %s", output)
	}
}

func TestCodexContinueFoldLiveToolCallStopsAsCommittedBarrier(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		`{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"id":"fc-1","type":"function_call","call_id":"call-1","name":"Shell","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":4,"output_index":1,"item_id":"fc-1","delta":"{}"}`,
		`{"type":"response.output_item.done","sequence_number":5,"output_index":1,"item":{"id":"fc-1","type":"function_call","call_id":"call-1","name":"Shell","arguments":"{}"}}`,
		foldCompleted("resp-1", 10, 0, 516, 516, 9),
	)
	run := runCodexFoldTest(t, nil, baseBody, round1)
	if len(run.continuation) != 0 {
		t.Fatalf("live committed tool call must stop fold, got %d continuation requests", len(run.continuation))
	}
	terminal := lastFoldPayloadOfType(t, run.forwarded, "response.completed")
	if got := gjson.GetBytes(terminal, "response.metadata.proxy_stopped_reason").String(); got != "committed_live_output" {
		t.Fatalf("stopped reason = %q, want committed_live_output; terminal=%s", got, terminal)
	}
}

func TestCodexContinueFoldUnknownOutputDoesNotLeakFromDiscardedRound(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		`{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"id":"novel-1","type":"future_side_effect","payload":"must-not-leak"}}`,
		foldCompleted("resp-1", 10, 0, 516, 516, 9),
	)
	round2 := foldSSE(foldCreated("resp-2", 0), foldCompleted("resp-2", 1, 0, 1, 1, 2))

	run := runCodexFoldTest(t, nil, baseBody, round1, round2)
	if strings.Contains(string(bytes.Join(run.forwarded, []byte("\n"))), "must-not-leak") {
		t.Fatalf("unknown output item from discarded truncated round leaked downstream:\n%s", bytes.Join(run.forwarded, []byte("\n")))
	}
}

func TestCodexContinueFoldRetriesZeroReasoningStallWithinAttemptBound(t *testing.T) {
	cfg := &config.CodexContinueConfig{Enabled: true, MaxContinue: 3}
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		foldCompleted("resp-1", 10, 0, 516, 516, 9),
	)
	round2Events := append([]string{foldCreated("resp-2", 0)}, foldMessageEvents(0, "msg-stall", "stall answer", 1)...)
	round2Events = append(round2Events, foldCompleted("resp-2", 2, 0, 3, 0, 9))
	round3Events := append([]string{foldCreated("resp-3", 0)}, foldMessageEvents(0, "msg-final", "final answer", 1)...)
	round3Events = append(round3Events, foldCompleted("resp-3", 2, 0, 7, 2, 9))

	run := runCodexFoldTest(t, cfg, baseBody, round1, foldSSE(round2Events...), foldSSE(round3Events...))
	if len(run.continuation) != 2 {
		t.Fatalf("continuations = %d, want retry after zero-reasoning stall", len(run.continuation))
	}
	joined := string(bytes.Join(run.forwarded, []byte("\n")))
	if strings.Contains(joined, "stall answer") {
		t.Fatalf("zero-reasoning stall tentative output leaked downstream:\n%s", joined)
	}
	if !strings.Contains(joined, "final answer") {
		t.Fatalf("retry final answer missing downstream:\n%s", joined)
	}
}

func TestCodexContinueFoldCompactionTriggerPassthroughDoesNotContinue(t *testing.T) {
	baseBody := foldBaseBody(`{"type":"compaction_trigger"}`)
	round1 := foldSSE(
		foldCreated("resp-compact", 0),
		foldReasoningDone(0, "rs-compact", "enc-compact", 2),
		foldCompleted("resp-compact", 10, 0, 516, 516, 9),
	)

	run := runCodexFoldTest(t, nil, baseBody, round1)
	if len(run.continuation) != 0 {
		t.Fatalf("compaction trigger must pass through without continuation, got %d requests", len(run.continuation))
	}
	if got := len(foldPayloadsOfType(t, run.forwarded, "response.completed")); got != 1 {
		t.Fatalf("compaction passthrough terminal count = %d, want 1; forwarded=%s", got, bytes.Join(run.forwarded, []byte("\n")))
	}
}

func TestCodexContinueFoldUsesSingleVisibleLifecycleAndResponseID(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-visible", 0),
		foldInProgress("resp-visible", 1),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		foldCompleted("resp-hidden-1", 10, 0, 516, 516, 9),
	)
	round2 := foldSSE(
		foldCreated("resp-hidden-2", 0),
		foldInProgress("resp-hidden-2", 1),
		foldCompleted("resp-hidden-2", 4, 0, 2, 2, 2),
	)

	run := runCodexFoldTest(t, nil, baseBody, round1, round2)
	for _, eventType := range []string{"response.created", "response.in_progress", "response.completed"} {
		payloads := foldPayloadsOfType(t, run.forwarded, eventType)
		if len(payloads) != 1 {
			t.Fatalf("%s count = %d, want 1; forwarded=%s", eventType, len(payloads), bytes.Join(run.forwarded, []byte("\n")))
		}
		if got := gjson.GetBytes(payloads[0], "response.id").String(); got != "resp-visible" {
			t.Fatalf("%s response.id = %q, want resp-visible; payload=%s", eventType, got, payloads[0])
		}
	}

	prevSeq := int64(-1)
	for _, line := range run.forwarded {
		payload := codexDataPayload(line)
		if payload == nil {
			continue
		}
		seq := gjson.GetBytes(payload, "sequence_number")
		if !seq.Exists() {
			continue
		}
		if seq.Int() <= prevSeq {
			t.Fatalf("sequence_number did not increase: previous=%d current=%d payload=%s", prevSeq, seq.Int(), payload)
		}
		prevSeq = seq.Int()
	}
}

func TestCodexContinueFoldBridgeCapturesVisibleIDMappedToFinalUpstreamChainID(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "folded-bridge-session"
	requestPayload := []byte(foldBaseBody(foldUserInput("start")))
	foldedTerminal := []byte(`{"type":"response.completed","response":{"id":"resp-visible","metadata":{"proxy_upstream_previous_response_id":"resp-upstream-final"},"output":[{"id":"msg-final","type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]}]}}`)
	bridge.CaptureResponse(sessionKey, "resp-visible", "gpt-5-codex", "auth-1", requestPayload, foldedTerminal)

	nextPayload := []byte(`{"model":"gpt-5-codex","stream":true,"input":[` + foldUserInput("start") + `,{"id":"msg-final","type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, nextPayload, "auth-1")
	if prevRespID != "resp-upstream-final" {
		t.Fatalf("bridge previous_response_id = %q, want internal final upstream id; delta=%s", prevRespID, delta)
	}
	if got := string(delta); got != `[{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]` {
		t.Fatalf("delta = %s", got)
	}
}

func TestCodexContinueFoldOnCleanTerminalBuffersToolCallAndContinues(t *testing.T) {
	cfg := &config.CodexContinueConfig{Enabled: true, MaxContinue: 2, OutputCommitPolicy: "on_clean_terminal"}
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		`{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"id":"fc-1","type":"function_call","call_id":"call-1","name":"Shell","arguments":""}}`,
		`{"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"id":"fc-1","type":"function_call","call_id":"call-1","name":"Shell","arguments":"{}"}}`,
		foldCompleted("resp-1", 10, 0, 516, 516, 9),
	)
	round2 := foldSSE(foldCreated("resp-2", 0), foldCompleted("resp-2", 2, 0, 2, 2, 2))

	run := runCodexFoldTest(t, cfg, baseBody, round1, round2)
	if len(run.continuation) != 1 {
		t.Fatalf("strict on-clean-terminal policy should continue once, got %d continuations", len(run.continuation))
	}
	if strings.Contains(string(bytes.Join(run.forwarded, []byte("\n"))), `"fc-1"`) {
		t.Fatalf("buffered tool call from truncated round leaked downstream:\n%s", bytes.Join(run.forwarded, []byte("\n")))
	}
}

func TestCodexContinueFoldSurfacesContinuationContextLengthErrorAndFlushesBufferedOutput(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		foldCompleted("resp-1", 10, 0, 516, 516, 9),
	)
	round2Events := append([]string{foldCreated("resp-2", 0)}, foldMessageEvents(0, "msg-partial", "partial answer before error", 1)...)
	round2Events = append(round2Events, `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model.","param":"input"},"sequence_number":5}`)

	run := runCodexFoldTestAllowErrors(t, nil, baseBody, round1, foldSSE(round2Events...))
	joined := string(bytes.Join(run.forwarded, []byte("\n")))
	if !strings.Contains(joined, "partial answer before error") {
		t.Fatalf("buffered partial answer was not flushed before terminal error:\n%s", joined)
	}
	if strings.Contains(joined, "upstream_eof") {
		t.Fatalf("terminal error was misreported as upstream_eof:\n%s", joined)
	}
	if !strings.Contains(joined, "context_length_exceeded") {
		t.Fatalf("upstream context-length error payload was not forwarded:\n%s", joined)
	}
	if len(run.errs) != 1 {
		t.Fatalf("stream errors = %d, want one surfaced upstream error; errs=%v", len(run.errs), run.errs)
	}
	if got := statusCodeFromTestError(t, run.errs[0]); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400; err=%v", got, run.errs[0])
	}
	assertCodexErrorCode(t, run.errs[0].Error(), "invalid_request_error", "context_length_exceeded")
}

func TestCodexContinueFoldFlushesBufferedOutputOnEOFNoTerminal(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("start"))
	round1 := foldSSE(
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
		foldCompleted("resp-1", 10, 0, 516, 516, 9),
	)
	round2Events := append([]string{foldCreated("resp-2", 0)}, foldMessageEvents(0, "msg-partial", "partial answer before eof", 1)...)

	run := runCodexFoldTest(t, nil, baseBody, round1, foldSSE(round2Events...))
	joined := string(bytes.Join(run.forwarded, []byte("\n")))
	if !strings.Contains(joined, "partial answer before eof") {
		t.Fatalf("buffered partial answer was not flushed on no-terminal EOF:\n%s", joined)
	}
	terminal := lastFoldPayloadOfType(t, run.forwarded, "response.incomplete")
	if got := gjson.GetBytes(terminal, "response.metadata.proxy_stopped_reason").String(); got != "upstream_eof" {
		t.Fatalf("stopped reason = %q, want upstream_eof; terminal=%s", got, terminal)
	}
	if !strings.Contains(gjson.GetBytes(terminal, "response.output").Raw, "partial answer before eof") {
		t.Fatalf("synthetic incomplete terminal missing flushed output: %s", terminal)
	}
}

func TestCodexContinueFoldNearContextLimitDoesNotOpenContinuation(t *testing.T) {
	baseBody := foldBaseBody(foldUserInput("near limit"))
	round1Events := append([]string{
		foldCreated("resp-1", 0),
		foldReasoningDone(0, "rs-1", "enc-1", 2),
	}, foldMessageEvents(1, "msg-1", "tentative truncated answer", 3)...)
	round1Events = append(round1Events, foldCompleted("resp-1", 260000, 0, 516, 516, 9))

	run := runCodexFoldTest(t, nil, baseBody, foldSSE(round1Events...))
	if len(run.continuation) != 0 {
		t.Fatalf("near-limit truncated round must not open continuation, got %d requests", len(run.continuation))
	}
	joined := string(bytes.Join(run.forwarded, []byte("\n")))
	if strings.Contains(joined, "tentative truncated answer") {
		t.Fatalf("near-limit guard leaked tentative truncated answer:\n%s", joined)
	}
	terminal := lastFoldPayloadOfType(t, run.forwarded, "response.incomplete")
	if got := gjson.GetBytes(terminal, "response.metadata.proxy_stopped_reason").String(); got != "near_context_limit" {
		t.Fatalf("stopped reason = %q, want near_context_limit; terminal=%s", got, terminal)
	}
}

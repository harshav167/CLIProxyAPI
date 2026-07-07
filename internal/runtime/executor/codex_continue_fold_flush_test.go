package executor

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// respBody wraps a string as an io.ReadCloser for a fake *http.Response.
func respBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

// newFoldTestContext builds a minimal codexContinueFoldContext whose only job is
// to let scanOneRound run. Fold is enabled so the buffered-item classification
// path is exercised. No real upstream/translator is needed because the test
// drives scanOneRound directly and captures forwarded lines via the callback.
func newFoldTestContext(t *testing.T) *codexContinueFoldContext {
	t.Helper()
	cfg := &config.Config{}
	return &codexContinueFoldContext{
		cfg:      &config.CodexContinueConfig{Enabled: true},
		executor: &CodexExecutor{cfg: cfg},
	}
}

// scanRound runs scanOneRound over an SSE string and returns the fold output
// plus the ordered list of lines that were forwarded live downstream.
func scanRound(t *testing.T, sse string) (codexContinueFoldOutput, [][]byte) {
	t.Helper()
	fx := newFoldTestContext(t)
	body := respBody(sse)
	out := make(chan cliproxyexecutor.StreamChunk, 64)
	var forwarded [][]byte
	forwardEvent := func(line []byte, _ codexIdentityConfuseState) {
		forwarded = append(forwarded, bytes.Clone(line))
	}
	result := fx.scanOneRound(
		context.Background(), body, codexIdentityConfuseState{}, out,
		nil /* reporter unused on this path */, codexReasoningReplayScope{}, []byte(`{}`), forwardEvent,
	)
	return result, forwarded
}

func forwardedContains(forwarded [][]byte, substr string) bool {
	for _, l := range forwarded {
		if bytes.Contains(l, []byte(substr)) {
			return true
		}
	}
	return false
}

// TestScanOneRound_BuffersFinalAnswerNotForwardedLive is the regression test for
// the dropped-final-answer bug. When the fold is active, a message
// (final-answer) item must be BUFFERED (collected into result.bufferedItems),
// NOT forwarded live and NOT dropped. Reasoning items must forward live.
func TestScanOneRound_BuffersFinalAnswerNotForwardedLive(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"abc"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","delta":"thinking"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"abc"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"Hello"}`,
		`data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":" world"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":10,"output_tokens_details":{"reasoning_tokens":2}}}}`,
		"",
	}, "\n")

	result, forwarded := scanRound(t, sse)

	// Reasoning forwarded live.
	if !forwardedContains(forwarded, `"type":"reasoning"`) {
		t.Fatalf("reasoning item should be forwarded live; forwarded=%d lines", len(forwarded))
	}
	// Final-answer message must NOT be forwarded live (it is buffered).
	if forwardedContains(forwarded, `"id":"msg_1"`) {
		t.Fatalf("final-answer message must NOT be forwarded live; it must be buffered")
	}
	// The message item must be collected into bufferedItems.
	if len(result.bufferedItems) != 1 {
		t.Fatalf("expected exactly 1 buffered item (the message); got %d", len(result.bufferedItems))
	}
	entry := result.bufferedItems[0]
	if entry.outputIndex != 1 {
		t.Fatalf("buffered item output_index: want 1, got %d", entry.outputIndex)
	}
	// The buffer must hold the full item lifecycle: added + 2 deltas + done = 4 lines.
	if len(entry.lines) != 4 {
		t.Fatalf("buffered item should hold 4 lines (added + 2 deltas + done); got %d", len(entry.lines))
	}
	joined := string(bytes.Join(entry.lines, []byte("\n")))
	for _, want := range []string{"response.output_item.added", "Hello", " world", "response.output_item.done"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buffered lines missing %q; got:\n%s", want, joined)
		}
	}
	// Terminal captured.
	if result.terminalType != "response.completed" {
		t.Fatalf("terminal type: want response.completed, got %q", result.terminalType)
	}
}

// TestScanOneRound_ToolCallStreamsLiveMessageBuffers verifies Change 1 of the
// fold UX redesign: a function_call streams LIVE (fires in real time) while the
// message BUFFERS (held back for discard-on-truncation). Only the message ends
// up in result.bufferedItems; the function_call's events are forwarded live.
func TestScanOneRound_ToolCallStreamsLiveMessageBuffers(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"x"}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"x"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"answer"}`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","name":"do_thing"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"a\":1}"}`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","name":"do_thing","arguments":"{\"a\":1}"}}`,
		`data: {"type":"response.completed","response":{"id":"r","usage":{"output_tokens":5,"output_tokens_details":{"reasoning_tokens":1}}}}`,
		"",
	}, "\n")

	result, forwarded := scanRound(t, sse)

	// Only the message buffers. The function_call streams live.
	if len(result.bufferedItems) != 1 {
		t.Fatalf("expected exactly 1 buffered item (the message); got %d", len(result.bufferedItems))
	}
	if findCodexBufferedItem(result.bufferedItems, 1) == nil {
		t.Fatalf("message (output_index 1) must be buffered")
	}
	if findCodexBufferedItem(result.bufferedItems, 2) != nil {
		t.Fatalf("function_call (output_index 2) must NOT be buffered — it streams live")
	}
	// The function_call events (added + args delta + done) must be forwarded live.
	if !forwardedContains(forwarded, `"id":"fc_1"`) {
		t.Fatalf("function_call added event must be forwarded live; forwarded=%d lines", len(forwarded))
	}
	if !forwardedContains(forwarded, `"a\":1`) && !forwardedContains(forwarded, `{\"a\":1}`) {
		t.Fatalf("function_call argument delta must be forwarded live")
	}
	// The message must NOT be forwarded live (it's buffered).
	if forwardedContains(forwarded, `"id":"msg_1"`) {
		t.Fatalf("message must be buffered, not forwarded live")
	}
}

// TestScanOneRound_BufferToolCallsFlagBuffersToolCalls verifies the opt-in
// paranoid posture: with BufferToolCalls=true, function_call items ALSO buffer.
func TestScanOneRound_BufferToolCallsFlagBuffersToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"do_thing"}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"do_thing","arguments":"{}"}}`,
		`data: {"type":"response.completed","response":{"id":"r","usage":{"output_tokens":5,"output_tokens_details":{"reasoning_tokens":1}}}}`,
		"",
	}, "\n")

	result, _ := scanRoundWithCfg(t, sse, true)
	// With the flag on, both message and function_call buffer.
	if len(result.bufferedItems) != 2 {
		t.Fatalf("BufferToolCalls=true: expected 2 buffered items; got %d", len(result.bufferedItems))
	}
}

// TestRechunkCodexBufferedMessage_ReslicesDeltasUniformly verifies Change 2:
// the buffered message's output_text.delta run is re-sliced into uniform
// size-char chunks, non-delta lines are preserved, and the concatenated text is
// unchanged.
func TestRechunkCodexBufferedMessage_ReslicesDeltasUniformly(t *testing.T) {
	lines := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}`),
		[]byte(`data: {"type":"response.content_part.added","output_index":1,"item_id":"msg_1","content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","content_index":0,"delta":"Hello"}`),
		[]byte(`data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","content_index":0,"delta":" world"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message"}}`),
	}
	out := rechunkCodexBufferedMessage(lines, 4) // 4-char slices of "Hello world" (11 chars) -> 3 slices

	// Count delta lines and non-delta lines.
	var deltaTexts []string
	nonDelta := 0
	for _, l := range out {
		p := codexDataPayload(l)
		if p != nil && gjson.GetBytes(p, "type").String() == "response.output_text.delta" {
			deltaTexts = append(deltaTexts, gjson.GetBytes(p, "delta").String())
		} else {
			nonDelta++
		}
	}
	// "Hello world" = 11 chars, size 4 -> "Hell","o wo","rld" = 3 slices.
	if len(deltaTexts) != 3 {
		t.Fatalf("expected 3 rechunked delta lines, got %d: %v", len(deltaTexts), deltaTexts)
	}
	if got := strings.Join(deltaTexts, ""); got != "Hello world" {
		t.Fatalf("rechunked text must equal original; want %q got %q", "Hello world", got)
	}
	// The 3 non-delta lines (added, content_part.added, output_item.done) preserved.
	if nonDelta != 3 {
		t.Fatalf("expected 3 non-delta lines preserved, got %d", nonDelta)
	}
	// First slice is exactly 4 chars.
	if len(deltaTexts[0]) != 4 {
		t.Fatalf("first slice should be 4 chars, got %q", deltaTexts[0])
	}
	// Each rechunked delta keeps the item_id from the template.
	for _, l := range out {
		p := codexDataPayload(l)
		if p != nil && gjson.GetBytes(p, "type").String() == "response.output_text.delta" {
			if gjson.GetBytes(p, "item_id").String() != "msg_1" {
				t.Fatalf("rechunked delta lost item_id: %s", l)
			}
		}
	}
}

// TestRechunkCodexBufferedMessage_NoDeltasReturnsVerbatim verifies rechunk is a
// no-op when there are no output_text.delta lines (e.g. a buffered tool call).
func TestRechunkCodexBufferedMessage_NoDeltasReturnsVerbatim(t *testing.T) {
	lines := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","arguments":"{}"}}`),
	}
	out := rechunkCodexBufferedMessage(lines, 4)
	if len(out) != len(lines) {
		t.Fatalf("no-delta input must be returned verbatim; want %d lines got %d", len(lines), len(out))
	}
}

// TestScanRound_RechunkFlushProducesSmoothDeltas is an end-to-end check: a
// buffered message with 2 upstream deltas, flushed with RechunkFinalAnswer on
// and a small size, produces MORE (smaller) downstream deltas than the upstream
// sent — proving the burst was smoothed.
func TestScanRound_RechunkFlushProducesSmoothDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"abcdefghij"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
		`data: {"type":"response.completed","response":{"id":"r","usage":{"output_tokens":5,"output_tokens_details":{"reasoning_tokens":1}}}}`,
		"",
	}, "\n")
	result, _ := scanRoundWithCfg(t, sse, false)
	if len(result.bufferedItems) != 1 {
		t.Fatalf("expected 1 buffered message; got %d", len(result.bufferedItems))
	}
	rechunked := rechunkCodexBufferedMessage(result.bufferedItems[0].lines, 3) // "abcdefghij"=10 -> 4 slices
	deltas := 0
	for _, l := range rechunked {
		p := codexDataPayload(l)
		if p != nil && gjson.GetBytes(p, "type").String() == "response.output_text.delta" {
			deltas++
		}
	}
	if deltas != 4 {
		t.Fatalf("size-3 rechunk of 10 chars should yield 4 deltas, got %d", deltas)
	}
}

// scanRoundWithCfg runs scanOneRound with an explicit BufferToolCalls setting.
func scanRoundWithCfg(t *testing.T, sse string, bufferToolCalls bool) (codexContinueFoldOutput, [][]byte) {
	t.Helper()
	fx := newFoldTestContext(t)
	fx.cfg.BufferToolCalls = bufferToolCalls
	body := respBody(sse)
	out := make(chan cliproxyexecutor.StreamChunk, 64)
	var forwarded [][]byte
	result := fx.scanOneRound(context.Background(), body, codexIdentityConfuseState{}, out, nil, codexReasoningReplayScope{}, []byte(`{}`),
		func(line []byte, _ codexIdentityConfuseState) { forwarded = append(forwarded, bytes.Clone(line)) })
	return result, forwarded
}

package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketWarmupBody_AddsGenerateFalse(t *testing.T) {
	in := []byte(`{"type":"response.create","model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi"}],"tools":[]}`)
	out, ok := BuildCodexWebsocketWarmupBody(in)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if gjson.GetBytes(out, "generate").Exists() != true {
		t.Fatalf("generate field missing: %s", out)
	}
	if gjson.GetBytes(out, "generate").Bool() != false {
		t.Fatalf("generate must be false, got %v", gjson.GetBytes(out, "generate").Value())
	}
	// Original fields preserved (warmup body == turn body + generate:false).
	if gjson.GetBytes(out, "type").String() != "response.create" {
		t.Fatalf("type not preserved: %s", out)
	}
	if gjson.GetBytes(out, "model").String() != "gpt-5.5" {
		t.Fatalf("model not preserved: %s", out)
	}
	if !gjson.GetBytes(out, "input").IsArray() {
		t.Fatalf("input not preserved: %s", out)
	}
	// Input must not be dropped/stripped — warmup primes the same prefix.
	if len(gjson.GetBytes(out, "input").Array()) != 1 {
		t.Fatalf("input items changed: %s", out)
	}
}

func TestBuildCodexWebsocketWarmupBody_InvalidInput(t *testing.T) {
	for _, tc := range [][]byte{nil, {}, []byte("not json"), []byte("[]not")} {
		if _, ok := BuildCodexWebsocketWarmupBody(tc); ok {
			t.Fatalf("expected ok=false for %q", tc)
		}
	}
}

func TestParseCodexWebsocketWarmupEvent_Completed(t *testing.T) {
	evt := ParseCodexWebsocketWarmupEvent([]byte(`data: {"type":"response.completed","response":{"id":"resp_abc123"}}`))
	if !evt.Terminal || !evt.Completed {
		t.Fatalf("expected terminal+completed, got %+v", evt)
	}
	if evt.ResponseID != "resp_abc123" {
		t.Fatalf("response id = %q, want resp_abc123", evt.ResponseID)
	}
}

func TestParseCodexWebsocketWarmupEvent_CompletedNoFraming(t *testing.T) {
	// Same event without the leading "data: " SSE framing.
	evt := ParseCodexWebsocketWarmupEvent([]byte(`{"type":"response.completed","response":{"id":"resp_x"}}`))
	if !evt.Completed || evt.ResponseID != "resp_x" {
		t.Fatalf("expected completed with id resp_x, got %+v", evt)
	}
}

// response.done must be treated as a successful terminal identical to
// response.completed — the WS executor accepts both (and normalizes done ->
// completed). If only completed were accepted, a done-terminated warmup would
// hang the drain loop until the socket idle timeout.
func TestParseCodexWebsocketWarmupEvent_DoneIsSuccessfulTerminal(t *testing.T) {
	evt := ParseCodexWebsocketWarmupEvent([]byte(`data: {"type":"response.done","response":{"id":"resp_done_1"}}`))
	if !evt.Terminal || !evt.Completed {
		t.Fatalf("response.done must be terminal+completed, got %+v", evt)
	}
	if evt.ResponseID != "resp_done_1" {
		t.Fatalf("response id = %q, want resp_done_1", evt.ResponseID)
	}
}

// Some event shapes carry the id at top level rather than under response.id.
func TestParseCodexWebsocketWarmupEvent_TopLevelIDFallback(t *testing.T) {
	evt := ParseCodexWebsocketWarmupEvent([]byte(`{"type":"response.completed","id":"resp_top"}`))
	if !evt.Completed || evt.ResponseID != "resp_top" {
		t.Fatalf("expected completed with top-level id resp_top, got %+v", evt)
	}
}

func TestParseCodexWebsocketWarmupEvent_FailedIsTerminalNotCompleted(t *testing.T) {
	for _, typ := range []string{"response.failed", "response.incomplete", "response.error"} {
		evt := ParseCodexWebsocketWarmupEvent([]byte(`{"type":"` + typ + `"}`))
		if !evt.Terminal {
			t.Fatalf("%s must be terminal", typ)
		}
		if evt.Completed {
			t.Fatalf("%s must NOT be completed", typ)
		}
		if evt.ResponseID != "" {
			t.Fatalf("%s must not carry a response id", typ)
		}
	}
}

func TestParseCodexWebsocketWarmupEvent_NonTerminalIgnored(t *testing.T) {
	for _, typ := range []string{
		"response.created", "response.in_progress",
		"response.output_text.delta", "response.reasoning_summary_text.delta",
		"response.output_item.done",
	} {
		evt := ParseCodexWebsocketWarmupEvent([]byte(`{"type":"` + typ + `"}`))
		if evt.Terminal {
			t.Fatalf("%s must be non-terminal", typ)
		}
	}
}

func TestParseCodexWebsocketWarmupEvent_Garbage(t *testing.T) {
	for _, tc := range [][]byte{nil, {}, []byte("data: [DONE]"), []byte("data: not json"), []byte(": comment")} {
		evt := ParseCodexWebsocketWarmupEvent(tc)
		if evt.Terminal || evt.Completed || evt.ResponseID != "" {
			t.Fatalf("garbage %q must yield zero event, got %+v", tc, evt)
		}
	}
}

func TestApplyCodexWebsocketPreviousResponseID(t *testing.T) {
	in := []byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":"go"}]}`)
	out := ApplyCodexWebsocketPreviousResponseID(in, "resp_prev")
	if gjson.GetBytes(out, "previous_response_id").String() != "resp_prev" {
		t.Fatalf("previous_response_id not set: %s", out)
	}
	// input preserved.
	if !gjson.GetBytes(out, "input").IsArray() {
		t.Fatalf("input dropped: %s", out)
	}
}

func TestApplyCodexWebsocketPreviousResponseID_EmptyIDNoChange(t *testing.T) {
	in := []byte(`{"type":"response.create"}`)
	out := ApplyCodexWebsocketPreviousResponseID(in, "")
	if gjson.GetBytes(out, "previous_response_id").Exists() {
		t.Fatalf("empty id must not set previous_response_id: %s", out)
	}
}

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

func TestClassifyCodexResponsesEvent(t *testing.T) {
	tests := []struct {
		name             string
		payload          string
		terminal         bool
		success          bool
		incomplete       bool
		failure          bool
		incompleteReason string
		responseID       string
	}{
		{name: "completed with SSE framing", payload: `data: {"type":"response.completed","response":{"id":"resp_nested"}}`, terminal: true, success: true, responseID: "resp_nested"},
		{name: "done with top-level id", payload: `{"type":"response.done","id":"resp_top"}`, terminal: true, success: true, responseID: "resp_top"},
		{name: "failed", payload: `{"type":"response.failed"}`, terminal: true, failure: true},
		{name: "incomplete max output tokens", payload: `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, terminal: true, incomplete: true, incompleteReason: "max_output_tokens"},
		{name: "incomplete content filter", payload: `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`, terminal: true, incomplete: true, incompleteReason: "content_filter"},
		{name: "response error", payload: `{"type":"response.error"}`, terminal: true, failure: true},
		{name: "top-level error", payload: `{"type":"error"}`, terminal: true, failure: true},
		{name: "created remains open", payload: `{"type":"response.created"}`},
		{name: "unknown remains open", payload: `{"type":"response.future_event"}`},
		{name: "malformed remains open", payload: `data: not json`},
		{name: "empty remains open"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := ClassifyCodexResponsesEvent([]byte(tt.payload))
			if event.Terminal != tt.terminal || event.Success != tt.success || event.Incomplete != tt.incomplete || event.Failure != tt.failure || event.IncompleteReason != tt.incompleteReason || event.ResponseID != tt.responseID {
				t.Fatalf("ClassifyCodexResponsesEvent() = %+v, want terminal=%v success=%v incomplete=%v failure=%v incompleteReason=%q responseID=%q", event, tt.terminal, tt.success, tt.incomplete, tt.failure, tt.incompleteReason, tt.responseID)
			}
		})
	}
}

func TestIsCodexPreviousResponseNotFoundEvent(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "nested response failed", payload: `{"type":"response.failed","response":{"error":{"code":"previous_response_not_found"}}}`, want: true},
		{name: "nested response error", payload: `data: {"type":"response.error","response":{"error":{"message":"previous_response_id was not found"}}}`, want: true},
		{name: "top level error", payload: `{"type":"error","error":{"code":"PREVIOUS_RESPONSE_NOT_FOUND"}}`, want: true},
		{name: "unrelated failure", payload: `{"type":"response.failed","response":{"error":{"code":"server_error"}}}`},
		{name: "malformed input", payload: `{`},
		{name: "non terminal input", payload: `{"type":"response.created","code":"previous_response_not_found"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCodexPreviousResponseNotFoundEvent([]byte(tt.payload)); got != tt.want {
				t.Fatalf("IsCodexPreviousResponseNotFoundEvent() = %v, want %v", got, tt.want)
			}
		})
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

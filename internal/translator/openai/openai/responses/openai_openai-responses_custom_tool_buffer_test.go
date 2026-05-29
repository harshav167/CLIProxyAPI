package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertResponses_ConsecutiveCustomToolCallsBufferIntoOneMessage is the
// regression test for the flush-guard bug: the pre-fix guard only exempted
// "function_call", so a "custom_tool_call" item triggered a premature
// flushPendingToolCalls(), splitting consecutive custom tool calls into
// separate assistant messages before their outputs — invalid Chat Completions
// tool-call ordering. Consecutive tool calls (function or custom) must collapse
// into ONE assistant message carrying multiple tool_calls.
func TestConvertResponses_ConsecutiveCustomToolCallsBufferIntoOneMessage(t *testing.T) {
	input := `{
		"model": "gpt-5.5",
		"input": [
			{"type":"custom_tool_call","call_id":"call_a","name":"ApplyPatch","input":"patch-a"},
			{"type":"custom_tool_call","call_id":"call_b","name":"ApplyPatch","input":"patch-b"},
			{"type":"custom_tool_call_output","call_id":"call_a","output":"ok-a"},
			{"type":"custom_tool_call_output","call_id":"call_b","output":"ok-b"}
		]
	}`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.5", []byte(input), false)

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.IsArray() {
		t.Fatalf("messages not array: %s", string(out))
	}

	// Find the assistant message(s) carrying tool_calls.
	var assistantToolMsgs []gjson.Result
	msgs.ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() && len(m.Get("tool_calls").Array()) > 0 {
			assistantToolMsgs = append(assistantToolMsgs, m)
		}
		return true
	})

	if len(assistantToolMsgs) != 1 {
		t.Fatalf("expected exactly 1 assistant message with tool_calls (consecutive custom tool calls must buffer together), got %d; body=%s", len(assistantToolMsgs), string(out))
	}
	calls := assistantToolMsgs[0].Get("tool_calls").Array()
	if len(calls) != 2 {
		t.Fatalf("expected 2 buffered tool_calls in the single assistant message, got %d; body=%s", len(calls), string(out))
	}
	if calls[0].Get("id").String() != "call_a" || calls[1].Get("id").String() != "call_b" {
		t.Fatalf("tool_call ids/order wrong: got %q,%q", calls[0].Get("id").String(), calls[1].Get("id").String())
	}
}

// TestConvertResponses_MixedFunctionAndCustomToolCallsBuffer confirms a
// function_call immediately followed by a custom_tool_call also stays in one
// assistant message (the guard must exempt both types).
func TestConvertResponses_MixedFunctionAndCustomToolCallsBuffer(t *testing.T) {
	input := `{
		"model": "gpt-5.5",
		"input": [
			{"type":"function_call","call_id":"call_fn","name":"ReadFile","arguments":"{}"},
			{"type":"custom_tool_call","call_id":"call_custom","name":"ApplyPatch","input":"patch"}
		]
	}`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.5", []byte(input), false)

	var assistantToolMsgs []gjson.Result
	gjson.GetBytes(out, "messages").ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() == "assistant" && len(m.Get("tool_calls").Array()) > 0 {
			assistantToolMsgs = append(assistantToolMsgs, m)
		}
		return true
	})
	if len(assistantToolMsgs) != 1 {
		t.Fatalf("function_call + custom_tool_call must buffer into 1 assistant message, got %d; body=%s", len(assistantToolMsgs), string(out))
	}
	if n := len(assistantToolMsgs[0].Get("tool_calls").Array()); n != 2 {
		t.Fatalf("expected 2 buffered tool_calls, got %d", n)
	}
}

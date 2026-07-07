package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertCodexResponseToOpenAI_ReasoningDeltaHasRole locks in the fix for
// the codex thinking-render regression. Cursor renders the thinking panel ONLY
// when role:"assistant" is present on EVERY reasoning delta. Verified on the
// wire (2026-07-07) against the two working thinking providers: GLM emits
// {"delta":{"role":"assistant","reasoning_content":"..."}} and Kimi emits
// {"delta":{"role":"assistant","reasoning":"..."}} — both tag role on every
// reasoning chunk. Codex previously emitted role-less reasoning deltas (plus a
// standalone bare-role opener), which Cursor dropped entirely (no panel).
func TestConvertCodexResponseToOpenAI_ReasoningDeltaHasRole(t *testing.T) {
	ctx := context.Background()
	var param any
	modelName := "gpt-5.5"

	feed := func(line string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(line), &param)
	}

	// response.created emits a role:"assistant" opener chunk to open the turn.
	opener := feed(`data: {"type":"response.created","response":{"id":"r1","created_at":1,"model":"gpt-5.5"}}`)
	if len(opener) != 1 {
		t.Fatalf("response.created must emit exactly 1 opener chunk, got %d", len(opener))
	}
	if got := gjson.GetBytes(opener[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("opener must carry delta.role=assistant, got %q (%s)", got, opener[0])
	}
	if gjson.GetBytes(opener[0], "choices.0.delta.content").Exists() {
		t.Fatalf("opener must NOT carry content; got %s", opener[0])
	}
	if gjson.GetBytes(opener[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("opener must NOT carry reasoning_content; got %s", opener[0])
	}

	// Reasoning delta: must carry reasoning_content AND role:"assistant" —
	// matching the GLM/Kimi wire shape that Cursor renders as the thinking panel.
	out := feed(`data: {"type":"response.reasoning_summary_text.delta","delta":"**Thinking**"}`)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk for reasoning delta, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").String(); got != "**Thinking**" {
		t.Fatalf("reasoning_content: want %q, got %q (%s)", "**Thinking**", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("reasoning delta MUST carry role:assistant (Cursor thinking render requires it); got %q (%s)", got, out[0])
	}

	// Reasoning done: also carries role:"assistant".
	outDone := feed(`data: {"type":"response.reasoning_summary_text.done"}`)
	if len(outDone) != 1 {
		t.Fatalf("expected 1 chunk for reasoning done, got %d", len(outDone))
	}
	if got := gjson.GetBytes(outDone[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("reasoning done must carry role:assistant; got %q (%s)", got, outDone[0])
	}

	// Content delta (the visible answer): role IS expected here — this is the
	// assistant's visible message, and Cursor keys the message on role.
	outContent := feed(`data: {"type":"response.output_text.delta","delta":"Hello"}`)
	if len(outContent) != 1 {
		t.Fatalf("expected 1 chunk for content delta, got %d", len(outContent))
	}
	if got := gjson.GetBytes(outContent[0], "choices.0.delta.content").String(); got != "Hello" {
		t.Fatalf("content: want %q, got %q (%s)", "Hello", got, outContent[0])
	}
	if got := gjson.GetBytes(outContent[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("content delta should carry role:assistant; got %q (%s)", got, outContent[0])
	}
}

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	// response.created now emits a bare-role opener chunk (establishes the
	// assistant turn so role-less reasoning deltas render as thinking in Cursor).
	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 opener chunk for response.created, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("opener must carry delta.role=assistant, got %q (%s)", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "model").String(); got != modelName {
		t.Fatalf("opener model: want %q, got %q", modelName, got)
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_InterleavedToolCallArgsDoneNoDuplicate(t *testing.T) {
	ctx := context.Background()
	var param any
	modelName := "gpt-5.3-codex"

	// Announce tool call A, stream A's args, announce tool call B (this resets
	// the OLD global HasReceivedArgumentsDelta flag), stream B's args, then
	// fire A.done. With per-item tracking, A.done must emit nothing because A's
	// args already streamed. With the old global flag, B.added reset it and
	// A.done wrongly re-emitted A's full arguments.
	feed := func(line string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(line), &param)
	}

	feed(`data: {"type":"response.created","response":{"id":"r1","created_at":1,"model":"gpt-5.3-codex"}}`)
	feed(`data: {"type":"response.output_item.added","item":{"id":"item_A","type":"function_call","call_id":"call_A","name":"toolA"}}`)
	feed(`data: {"type":"response.function_call_arguments.delta","item_id":"item_A","delta":"{\"a\":1}"}`)
	feed(`data: {"type":"response.output_item.added","item":{"id":"item_B","type":"function_call","call_id":"call_B","name":"toolB"}}`)
	feed(`data: {"type":"response.function_call_arguments.delta","item_id":"item_B","delta":"{\"b\":2}"}`)

	// A.done — args already streamed for item_A, so nothing should be emitted.
	outADone := feed(`data: {"type":"response.function_call_arguments.done","item_id":"item_A","arguments":"{\"a\":1}"}`)
	if len(outADone) != 0 {
		t.Fatalf("A.done should emit nothing (args already streamed); got %d chunks: %s", len(outADone), outADone)
	}

	// B.done — likewise nothing.
	outBDone := feed(`data: {"type":"response.function_call_arguments.done","item_id":"item_B","arguments":"{\"b\":2}"}`)
	if len(outBDone) != 0 {
		t.Fatalf("B.done should emit nothing (args already streamed); got %d chunks: %s", len(outBDone), outBDone)
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgsDoneFallbackWhenNoDelta(t *testing.T) {
	ctx := context.Background()
	var param any
	modelName := "gpt-5.3-codex"

	feed := func(line string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(line), &param)
	}

	feed(`data: {"type":"response.created","response":{"id":"r1","created_at":1,"model":"gpt-5.3-codex"}}`)
	feed(`data: {"type":"response.output_item.added","item":{"id":"item_A","type":"function_call","call_id":"call_A","name":"toolA"}}`)

	// No delta streamed for item_A: done must emit the full arguments fallback.
	out := feed(`data: {"type":"response.function_call_arguments.done","item_id":"item_A","arguments":"{\"a\":1}"}`)
	if len(out) != 1 {
		t.Fatalf("done without prior delta should emit fallback args chunk; got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"a":1}` {
		t.Fatalf("fallback args = %q, want %q", got, `{"a":1}`)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallChunkOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls").Exists() {
		t.Fatalf("expected tool_calls to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgumentsDeltaOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"query\":\"OpenAI\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").Exists() {
		t.Fatalf("expected tool call arguments delta to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallInputDelta(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_123","name":"apply_patch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "apply_patch" {
		t.Fatalf("expected custom tool name %q, got %q; chunk=%s", "apply_patch", got, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.custom_tool_call_input.delta","delta":"*** Begin Patch"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "*** Begin Patch" {
		t.Fatalf("expected custom tool input delta %q, got %q; chunk=%s", "*** Begin Patch", got, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallInputDoneFallsBackToInput(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_123","name":"apply_patch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.custom_tool_call_input.done","input":"*** End Patch"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "*** End Patch" {
		t.Fatalf("expected custom tool input done fallback %q, got %q; chunk=%s", "*** End Patch", got, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallDoneFallsBackToInput(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_123","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "apply_patch" {
		t.Fatalf("expected custom tool name %q, got %q; chunk=%s", "apply_patch", got, string(out[0]))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("expected custom tool done fallback args, got %q; chunk=%s", got, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_StreamPartialImageEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamImageGenerationCallDoneEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/jpeg;base64,Ymll" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/jpeg;base64,Ymll", gotURL, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_StreamUsageTopLevelCachedTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.5","usage":{"input_tokens":10,"cached_tokens":4,"output_tokens":2,"total_tokens":12}}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 usage chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_tokens").Int(); got != 4 {
		t.Fatalf("cached_tokens = %d, want 4; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamImageGenerationCallAddsMessageImages(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	gotURL := gjson.GetBytes(out, "choices.0.message.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out))
	}
}

func TestConvertCodexResponseToOpenAINonStream_UsageTopLevelCachedTokens(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":10,"cached_tokens":4,"output_tokens":2,"total_tokens":12},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); got != 4 {
		t.Fatalf("cached_tokens = %d, want 4; response=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAINonStream_CustomToolCallUsesInput(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"custom_tool_call","call_id":"call_123","name":"apply_patch","input":"*** Begin Patch"}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String(); got != "apply_patch" {
		t.Fatalf("expected custom tool name %q, got %q; chunk=%s", "apply_patch", got, string(out))
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != "*** Begin Patch" {
		t.Fatalf("expected custom tool input fallback %q, got %q; chunk=%s", "*** Begin Patch", got, string(out))
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("expected finish reason %q, got %q; chunk=%s", "tool_calls", got, string(out))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamMultiMessageEmptyTrailingKeepsContent(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":"the real answer"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking again"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":""}]}` +
		`]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, raw, nil)

	got := gjson.GetBytes(out, "choices.0.message.content")
	if !got.Exists() || got.Type == gjson.Null {
		t.Fatalf("content was dropped to null by trailing empty message; resp=%s", string(out))
	}
	if got.String() != "the real answer" {
		t.Fatalf("expected content %q, got %q; resp=%s", "the real answer", got.String(), string(out))
	}
}

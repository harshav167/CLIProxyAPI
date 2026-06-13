package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiResponseToOpenAIMovesSentinelThoughtTextToReasoning(t *testing.T) {
	var param any
	firstChunk := []byte(`{"responseId":"gemini-response","modelVersion":"gemini-3.1-pro-preview","createTime":"2026-05-15T06:21:08Z","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"...94>thought\nCRITICAL"}]}}]}`)

	first := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-3.1-pro", nil, nil, firstChunk, &param)
	if len(first) != 1 {
		t.Fatalf("expected one converted chunk, got %d", len(first))
	}
	firstDelta := gjson.GetBytes(first[0], "choices.0.delta")
	if got := firstDelta.Get("reasoning_content").String(); got != "CRITICAL" {
		t.Fatalf("expected sentinel thought text to become reasoning_content, got %q in %s", got, string(first[0]))
	}
	if got := firstDelta.Get("content").Raw; got != "null" {
		t.Fatalf("expected sentinel thought text not to become visible content, got %s in %s", got, string(first[0]))
	}

	secondChunk := []byte(`{"responseId":"gemini-response","modelVersion":"gemini-3.1-pro-preview","createTime":"2026-05-15T06:21:08Z","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":" INSTRUCTION"}]}}]}`)
	second := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-3.1-pro", nil, nil, secondChunk, &param)
	if len(second) != 1 {
		t.Fatalf("expected one converted continuation chunk, got %d", len(second))
	}
	secondDelta := gjson.GetBytes(second[0], "choices.0.delta")
	if got := secondDelta.Get("reasoning_content").String(); got != " INSTRUCTION" {
		t.Fatalf("expected sentinel thought continuation to stay reasoning_content, got %q in %s", got, string(second[0]))
	}
	if got := secondDelta.Get("content").Raw; got != "null" {
		t.Fatalf("expected sentinel thought continuation not to become visible content, got %s in %s", got, string(second[0]))
	}
}

func TestConvertGeminiResponseToOpenAINativeThoughtDoesNotStickToNextText(t *testing.T) {
	var param any

	// Native thought part (thought:true) -> reasoning_content.
	thoughtChunk := []byte(`{"responseId":"r","modelVersion":"gemini-3.1-pro-preview","createTime":"2026-05-15T06:21:08Z","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"let me think","thought":true}]}}]}`)
	first := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-3.1-pro", nil, nil, thoughtChunk, &param)
	if len(first) != 1 {
		t.Fatalf("expected one chunk for thought, got %d", len(first))
	}
	if got := gjson.GetBytes(first[0], "choices.0.delta.reasoning_content").String(); got != "let me think" {
		t.Fatalf("native thought should be reasoning_content, got %q in %s", got, string(first[0]))
	}

	// Following NORMAL text part (no thought flag, no sentinel) must be visible
	// content — the native-thought flag must NOT stick to it and hide the answer.
	answerChunk := []byte(`{"responseId":"r","modelVersion":"gemini-3.1-pro-preview","createTime":"2026-05-15T06:21:08Z","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"the visible answer"}]}}]}`)
	second := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-3.1-pro", nil, nil, answerChunk, &param)
	if len(second) != 1 {
		t.Fatalf("expected one chunk for answer, got %d", len(second))
	}
	delta := gjson.GetBytes(second[0], "choices.0.delta")
	if got := delta.Get("content").String(); got != "the visible answer" {
		t.Fatalf("answer after native thought should be visible content, got content=%q reasoning=%q in %s",
			got, delta.Get("reasoning_content").String(), string(second[0]))
	}
	// reasoning_content may exist as a default null template field; it must not
	// carry the answer text.
	if got := delta.Get("reasoning_content"); got.Raw != "null" && got.String() != "" {
		t.Fatalf("answer after native thought must NOT be reasoning_content, got %s; body=%s", got.Raw, string(second[0]))
	}
}

func TestConvertGeminiResponseToOpenAILeavesNormalTextAsContent(t *testing.T) {
	var param any
	chunk := []byte(`{"responseId":"gemini-response","modelVersion":"gemini-3.1-pro-preview","createTime":"2026-05-15T06:21:08Z","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Final answer"}]}}]}`)

	out := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-3.1-pro", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected one converted chunk, got %d", len(out))
	}
	delta := gjson.GetBytes(out[0], "choices.0.delta")
	if got := delta.Get("content").String(); got != "Final answer" {
		t.Fatalf("expected normal Gemini text to remain visible content, got %q in %s", got, string(out[0]))
	}
	if got := delta.Get("reasoning_content").Raw; got != "null" {
		t.Fatalf("expected normal Gemini text not to become reasoning_content, got %s in %s", got, string(out[0]))
	}
}

func TestConvertGeminiResponseToOpenAINonStreamMovesSentinelThoughtTextToReasoning(t *testing.T) {
	raw := []byte(`{"responseId":"gemini-response","modelVersion":"gemini-3.1-pro-preview","createTime":"2026-05-15T06:21:08Z","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"...94>thought\nCRITICAL"},{"text":" INSTRUCTION"}]}}]}`)

	out := ConvertGeminiResponseToOpenAINonStream(context.Background(), "gemini-3.1-pro", nil, nil, raw, nil)
	message := gjson.GetBytes(out, "choices.0.message")
	if got := message.Get("reasoning_content").String(); got != "CRITICAL INSTRUCTION" {
		t.Fatalf("expected sentinel thought text to become non-stream reasoning_content, got %q in %s", got, string(out))
	}
	if got := message.Get("content").Raw; got != "null" {
		t.Fatalf("expected sentinel thought text not to become non-stream visible content, got %s in %s", got, string(out))
	}
}

func TestGeminiFinishReasonOnlyOnFinalChunk(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk1 := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_dir","args":{"path":"C:/"}}}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"}}`)
	result1 := ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)
	if len(result1) != 1 {
		t.Fatalf("expected 1 result from chunk1, got %d", len(result1))
	}
	fr1 := gjson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Fatalf("expected null finish_reason on tool chunk, got %v", fr1.String())
	}

	chunk2 := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_dir","args":{"path":"D:/"}}}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"}}`)
	ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	chunk3 := []byte(`{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)
	result3 := ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk3, &param)
	if len(result3) != 1 {
		t.Fatalf("expected 1 result from chunk3, got %d", len(result3))
	}
	fr3 := gjson.GetBytes(result3[0], "choices.0.finish_reason").String()
	if fr3 != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %s", fr3)
	}
	nfr3 := gjson.GetBytes(result3[0], "choices.0.native_finish_reason").String()
	if nfr3 != "stop" {
		t.Fatalf("expected native_finish_reason stop, got %s", nfr3)
	}
}

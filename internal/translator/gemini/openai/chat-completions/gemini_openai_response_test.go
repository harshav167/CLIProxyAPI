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

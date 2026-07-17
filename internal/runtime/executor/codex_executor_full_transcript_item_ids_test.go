package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// codexFullTranscriptItemIDsFixture returns a full-transcript Responses request
// with real server msg_* ids, a msg_synth_0, reasoning encrypted_content, and
// tool items. No previous_response_id.
func codexFullTranscriptItemIDsFixture() []byte {
	validEnc := validCodexReasoningEncryptedContentForTest()
	return []byte(`{"model":"gpt-5-codex","stream":true,"store":false,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","encrypted_content":"` + validEnc + `","summary":[{"type":"summary_text","text":"think"}]},` +
		`{"type":"message","id":"msg_0b17aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","role":"assistant","content":[{"type":"output_text","text":"a"}]},` +
		`{"type":"message","id":"msg_synth_0","role":"assistant","content":[{"type":"output_text","text":"synth"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"TodoWrite","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ok"},` +
		`{"type":"message","id":"msg_0b17bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","role":"assistant","content":[{"type":"output_text","text":"b"}]}` +
		`]}`)
}

// codexFullTranscriptItemIDsChainedFixture returns the same shape with
// previous_response_id set; helper must NOT strip real msg ids.
func codexFullTranscriptItemIDsChainedFixture() []byte {
	return []byte(`{"model":"gpt-5-codex","stream":true,"previous_response_id":"resp_keep","store":false,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","id":"msg_0b17keepthisid","role":"assistant","content":[{"type":"output_text","text":"a"}]},` +
		`{"type":"message","id":"msg_synth_0","role":"assistant","content":[{"type":"output_text","text":"synth"}]}` +
		`]}`)
}

// assertNoRealMsgIDs asserts every input item with an id starting "msg_" also
// starts with "msg_synth_" (i.e. real server ids stripped). Also checks tool
// call_ids and reasoning encrypted_content are preserved.
func assertNoRealMsgIDs(t *testing.T, body []byte) {
	t.Helper()
	input := gjson.GetBytes(body, "input").Array()
	hasSynth := false
	hasFunctionCall := false
	hasFunctionCallOutput := false
	validEnc := validCodexReasoningEncryptedContentForTest()
	hasEnc := false
	for i, item := range input {
		if id := item.Get("id"); id.Exists() && id.Type == gjson.String {
			idStr := id.String()
			if strings.HasPrefix(idStr, "msg_") && !strings.HasPrefix(idStr, "msg_synth_") {
				t.Fatalf("input.%d real msg id %q not stripped; body=%s", i, idStr, string(body))
			}
			if idStr == "msg_synth_0" {
				hasSynth = true
			}
		}
		if item.Get("type").String() == "function_call" && item.Get("call_id").String() == "call_1" && item.Get("name").String() == "TodoWrite" {
			hasFunctionCall = true
		}
		if item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == "call_1" && item.Get("output").String() == "ok" {
			hasFunctionCallOutput = true
		}
		if enc := item.Get("encrypted_content"); enc.Exists() && enc.Type == gjson.String && enc.String() == validEnc {
			hasEnc = true
		}
	}
	if !hasSynth {
		t.Fatalf("msg_synth_0 lost; body=%s", string(body))
	}
	if !hasFunctionCall {
		t.Fatalf("function_call item with call_id=call_1 name=TodoWrite lost; body=%s", string(body))
	}
	if !hasFunctionCallOutput {
		t.Fatalf("function_call_output item with call_id=call_1 output=ok lost; body=%s", string(body))
	}
	if !hasEnc {
		t.Fatalf("reasoning encrypted_content (exact fixture token) lost; body=%s", string(body))
	}
}

func assertKeepsRealMsgID(t *testing.T, body []byte, wantID string) {
	t.Helper()
	if !bytes.Contains(body, []byte(`"id":"`+wantID+`"`)) {
		t.Fatalf("chained body lost real msg id %q; body=%s", wantID, string(body))
	}
}

func TestCodexExecutorExecute_StripsRealMsgIDsOnFullTranscript(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test"},
	}, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexFullTranscriptItemIDsFixture(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	assertNoRealMsgIDs(t, gotBody)
}

func TestCodexExecutorExecuteStream_StripsRealMsgIDsOnFullTranscript(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test"},
	}, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexFullTranscriptItemIDsFixture(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}
	assertNoRealMsgIDs(t, gotBody)
}

func TestCodexExecutorCompact_StripsRealMsgIDsOnFullTranscript(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test"},
	}, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexFullTranscriptItemIDsFixture(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	assertNoRealMsgIDs(t, gotBody)
}

func TestCodexExecutorExecute_KeepsRealMsgIDsWhenPreviousResponseIDSet(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test"},
	}, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexFullTranscriptItemIDsChainedFixture(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	assertKeepsRealMsgID(t, gotBody, "msg_0b17keepthisid")
}

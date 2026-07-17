package helps

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStripCodexFullTranscriptServerMessageIDs_NoopWhenPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp_keep","input":[{"type":"message","id":"msg_abc","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
	got := StripCodexFullTranscriptServerMessageIDs(body)
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed when previous_response_id set:\n got=%s\nwant=%s", got, body)
	}
}

func TestStripCodexFullTranscriptServerMessageIDs_NoopWhenNoInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex"}`)
	got := StripCodexFullTranscriptServerMessageIDs(body)
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed when input absent:\n got=%s\nwant=%s", got, body)
	}
}

func TestStripCodexFullTranscriptServerMessageIDs_StripsRealMsgIDs(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","stream":true,"input":[` +
		`{"type":"reasoning","encrypted_content":"ENC","summary":[{"type":"summary_text","text":"think"}]},` +
		`{"type":"message","id":"msg_0b17deadbeef","role":"assistant","content":[{"type":"output_text","text":"a"}]},` +
		`{"type":"message","id":"msg_synth_0","role":"assistant","content":[{"type":"output_text","text":"synth"}]},` +
		`{"type":"message","id":"msg_0b17cafecafe","role":"assistant","content":[{"type":"output_text","text":"b"}]}` +
		`]}`)
	got := StripCodexFullTranscriptServerMessageIDs(body)

	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 4 {
		t.Fatalf("input length = %d, want 4; body=%s", len(input), string(got))
	}
	for i, item := range input {
		id := item.Get("id")
		if !id.Exists() {
			continue
		}
		idStr := id.String()
		if idStr != "msg_synth_0" {
			t.Fatalf("input.%d.id = %q, want stripped (only msg_synth_* kept); body=%s", i, idStr, string(got))
		}
	}
	if enc := gjson.GetBytes(got, "input.0.encrypted_content").String(); enc != "ENC" {
		t.Fatalf("reasoning encrypted_content = %q, want ENC; body=%s", enc, string(got))
	}
}

func TestStripCodexFullTranscriptServerMessageIDs_PreservesToolItems(t *testing.T) {
	toolIn := []byte(`{"type":"function_call","call_id":"call_1","name":"TodoWrite","arguments":"{}"}`)
	toolOut := []byte(`{"type":"function_call_output","call_id":"call_1","output":"ok"}`)
	body := []byte(`{"model":"gpt-5-codex","input":[` +
		string(toolIn) + `,` +
		string(toolOut) + `,` +
		`{"type":"message","id":"msg_0b17toolcheck","role":"assistant","content":[{"type":"output_text","text":"x"}]}` +
		`]}`)
	got := StripCodexFullTranscriptServerMessageIDs(body)

	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input length = %d, want 3; body=%s", len(input), string(got))
	}
	if fcRaw := input[0].Raw; fcRaw != string(toolIn) {
		t.Fatalf("function_call changed:\n got=%s\nwant=%s\nbody=%s", fcRaw, toolIn, string(got))
	}
	if fcoRaw := input[1].Raw; fcoRaw != string(toolOut) {
		t.Fatalf("function_call_output changed:\n got=%s\nwant=%s\nbody=%s", fcoRaw, toolOut, string(got))
	}
	if gjson.GetBytes(got, "input.2.id").Exists() {
		t.Fatalf("real msg id not stripped from input.2; body=%s", string(got))
	}
}

func TestStripCodexFullTranscriptServerMessageIDs_ProdPairShape(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","stream":true,"store":false,"input":[` +
		`{"type":"user","role":"user","content":"hi"},` +
		`{"type":"reasoning","encrypted_content":"ENC","summary":[{"type":"summary_text","text":"plan"}]},` +
		`{"type":"message","id":"msg_0b17e9e40c62b5ee006a538023cf7c8191b6d61eb59715d089","role":"assistant","content":[{"type":"output_text","text":"old"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"Shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"done"},` +
		`{"type":"reasoning","encrypted_content":"ENC2","summary":[{"type":"summary_text","text":"plan2"}]},` +
		`{"type":"message","id":"msg_0b17e9e40c62b5ee006a53804423208191837cf4e2ea14b9a2","role":"assistant","content":[{"type":"output_text","text":"pair"}]}` +
		`]}`)
	got := StripCodexFullTranscriptServerMessageIDs(body)

	input := gjson.GetBytes(got, "input").Array()
	for i, item := range input {
		id := item.Get("id")
		if !id.Exists() {
			continue
		}
		idStr := id.String()
		if idStr != "" && !bytes.HasPrefix([]byte(idStr), []byte("msg_synth_")) {
			t.Fatalf("input.%d real msg id %q not stripped; body=%s", i, idStr, string(got))
		}
	}
	if enc := gjson.GetBytes(got, "input.1.encrypted_content").String(); enc != "ENC" {
		t.Fatalf("first reasoning encrypted_content changed: %q; body=%s", enc, string(got))
	}
	if enc := gjson.GetBytes(got, "input.5.encrypted_content").String(); enc != "ENC2" {
		t.Fatalf("second reasoning encrypted_content changed: %q; body=%s", enc, string(got))
	}
	if gjson.GetBytes(got, "input.3.call_id").String() != "call_1" {
		t.Fatalf("tool call_id lost; body=%s", string(got))
	}
	if bytes.Contains(got, []byte(`"rs_`)) {
		t.Fatalf("helper invented rs_ id; body=%s", string(got))
	}
}

package helps

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestPrepareClaudeCacheDiagnosticsChainsPreviousMessageID(t *testing.T) {
	resetClaudeCacheDiagnosticsStore()
	t.Cleanup(resetClaudeCacheDiagnosticsStore)

	payload := []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`)
	first := PrepareClaudeCacheDiagnostics(payload, "session-1")
	if got := gjson.GetBytes(first, "diagnostics.previous_message_id"); !got.Exists() || got.Type != gjson.Null {
		t.Fatalf("first previous_message_id = %s, want explicit null; body=%s", got.Raw, first)
	}

	RecordClaudeCacheDiagnosticsMessageID("session-1", "msg_previous")
	second := PrepareClaudeCacheDiagnostics(payload, "session-1")
	if got := gjson.GetBytes(second, "diagnostics.previous_message_id").String(); got != "msg_previous" {
		t.Fatalf("second previous_message_id = %q, want msg_previous; body=%s", got, second)
	}
}

func TestPrepareClaudeCacheDiagnosticsPreservesClientOwnedPreviousMessageID(t *testing.T) {
	resetClaudeCacheDiagnosticsStore()
	t.Cleanup(resetClaudeCacheDiagnosticsStore)

	payload := []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"},"diagnostics":{"previous_message_id":"msg_client_owned"}}`)
	RecordClaudeCacheDiagnosticsMessageID("session-1", "msg_proxy_owned")

	out := PrepareClaudeCacheDiagnostics(payload, "session-1")
	if got := gjson.GetBytes(out, "diagnostics.previous_message_id").String(); got != "msg_client_owned" {
		t.Fatalf("previous_message_id = %q, want client-owned value", got)
	}
}

func TestPrepareClaudeCacheDiagnosticsSkipsIneligibleRequests(t *testing.T) {
	for name, payload := range map[string][]byte{
		"non_opus":         []byte(`{"model":"claude-sonnet-5","thinking":{"type":"adaptive"}}`),
		"manual_thinking":  []byte(`{"model":"claude-opus-5","thinking":{"type":"enabled"}}`),
		"future_lookalike": []byte(`{"model":"claude-opus-50","thinking":{"type":"adaptive"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			out := PrepareClaudeCacheDiagnostics(payload, "session-1")
			if gjson.GetBytes(out, "diagnostics").Exists() {
				t.Fatalf("ineligible request gained diagnostics: %s", out)
			}
		})
	}
}

func TestClaudeMessageIDFromResponse(t *testing.T) {
	if got := ClaudeMessageIDFromResponse([]byte(`{"id":"msg_nonstream","type":"message"}`)); got != "msg_nonstream" {
		t.Fatalf("nonstream id = %q, want msg_nonstream", got)
	}
	if got := ClaudeMessageIDFromResponse([]byte(`data: {"type":"message_start","message":{"id":"msg_stream"}}`)); got != "msg_stream" {
		t.Fatalf("stream id = %q, want msg_stream", got)
	}
	if got := ClaudeMessageIDFromResponse([]byte(`data: {"type":"content_block_delta"}`)); got != "" {
		t.Fatalf("non-start stream id = %q, want empty", got)
	}
	for name, payload := range map[string][]byte{
		"message_delta_top_level_id": []byte(`data: {"type":"message_delta","id":"msg_wrong","delta":{}}`),
		"error_top_level_id":         []byte(`{"type":"error","id":"resp_wrong","error":{"message":"no"}}`),
		"tool_use_id":                []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_wrong"}}`),
		"response_id":                []byte(`{"id":"resp_wrong","type":"response"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if got := ClaudeMessageIDFromResponse(payload); got != "" {
				t.Fatalf("id = %q, want empty", got)
			}
		})
	}
}

func TestClaudeMessageIDFromSSEFindsMessageStart(t *testing.T) {
	payload := []byte("data: {\"type\":\"message_delta\",\"id\":\"msg_wrong\"}\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_right\"}}\n" +
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"id\":\"toolu_wrong\"}}\n")

	if got := ClaudeMessageIDFromSSE(payload); got != "msg_right" {
		t.Fatalf("id = %q, want msg_right", got)
	}
}

func TestPurgeExpiredClaudeCacheDiagnostics(t *testing.T) {
	resetClaudeCacheDiagnosticsStore()
	t.Cleanup(resetClaudeCacheDiagnosticsStore)

	now := time.Now()
	claudeCacheDiagnosticsMu.Lock()
	claudeCacheDiagnosticsStore["expired"] = claudeCacheDiagnosticsEntry{messageID: "msg_old", expire: now.Add(-time.Second)}
	claudeCacheDiagnosticsStore["active"] = claudeCacheDiagnosticsEntry{messageID: "msg_new", expire: now.Add(time.Second)}
	claudeCacheDiagnosticsMu.Unlock()

	purgeExpiredClaudeCacheDiagnostics(now)

	claudeCacheDiagnosticsMu.Lock()
	defer claudeCacheDiagnosticsMu.Unlock()
	if _, ok := claudeCacheDiagnosticsStore["expired"]; ok {
		t.Fatal("expired diagnostics entry was not removed")
	}
	if _, ok := claudeCacheDiagnosticsStore["active"]; !ok {
		t.Fatal("active diagnostics entry was removed")
	}
}

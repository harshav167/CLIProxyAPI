package api

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexTransportRoutesResponsesNonStreamAggregatesHTTP(t *testing.T) {
	fixture := newCodexTransportRouteFixture(t, "{}\n", codexRouteUpstreamSuccess)
	body := []byte(`{"model":"gpt-5-codex","stream":false,"input":[{"type":"message","role":"user","content":"hello"}]}`)

	response := fixture.request(t, "/v1/responses", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := fixture.wsRequests.Load(); got != 0 {
		t.Fatalf("non-stream websocket requests = %d, want 0", got)
	}
	if got := fixture.httpRequests.Load(); got != 1 {
		t.Fatalf("non-stream HTTP requests = %d, want 1", got)
	}
	if got := gjson.GetBytes(response.Body.Bytes(), "id").String(); got != "resp-route" {
		t.Fatalf("response id = %q, want resp-route; body=%s", got, response.Body.String())
	}
	if got := gjson.GetBytes(response.Body.Bytes(), "usage.total_tokens").Int(); got != 8 {
		t.Fatalf("total tokens = %d, want 8; body=%s", got, response.Body.String())
	}
	output := gjson.GetBytes(response.Body.Bytes(), "output").Array()
	if len(output) != 3 {
		t.Fatalf("output item count = %d, want 3; body=%s", len(output), response.Body.String())
	}
	if got := output[0].Get("type").String(); got != "reasoning" {
		t.Fatalf("output[0].type = %q, want reasoning; body=%s", got, response.Body.String())
	}
	if got := output[0].Get("summary.0.text").String(); got != "route reasoning" {
		t.Fatalf("reasoning summary = %q, want route reasoning; body=%s", got, response.Body.String())
	}
	if got := output[1].Get("name").String(); got != "lookup" {
		t.Fatalf("tool name = %q, want lookup; body=%s", got, response.Body.String())
	}
	if got := output[1].Get("arguments").String(); got != "{}" {
		t.Fatalf("tool arguments = %q, want {}; body=%s", got, response.Body.String())
	}
	if got := output[2].Get("content.0.text").String(); got != "route answer" {
		t.Fatalf("answer = %q, want route answer; body=%s", got, response.Body.String())
	}
}

func TestCodexTransportRoutesResponsesNonStreamReturnsTargetTerminalSchema(t *testing.T) {
	tests := []struct {
		name       string
		mode       codexRouteUpstreamMode
		wantID     string
		wantStatus string
		wantReason string
		wantAnswer string
	}{
		{name: "response done", mode: codexRouteUpstreamDone, wantID: "resp-done", wantStatus: "completed", wantAnswer: "done answer"},
		{name: "response incomplete", mode: codexRouteUpstreamIncompleteMaxOutputTokens, wantID: "resp-incomplete", wantStatus: "incomplete", wantReason: "max_output_tokens"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCodexTransportRouteFixture(t, "{}\n", tt.mode)
			body := []byte(`{"model":"gpt-5-codex","stream":false,"input":[{"type":"message","role":"user","content":"hello"}]}`)

			response := fixture.request(t, "/v1/responses", body)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			wire := response.Body.Bytes()
			if got := gjson.GetBytes(wire, "id").String(); got != tt.wantID {
				t.Fatalf("response id = %q, want %q; body=%s", got, tt.wantID, wire)
			}
			if got := gjson.GetBytes(wire, "status").String(); got != tt.wantStatus {
				t.Fatalf("response status = %q, want %q; body=%s", got, tt.wantStatus, wire)
			}
			if got := gjson.GetBytes(wire, "incomplete_details.reason").String(); got != tt.wantReason {
				t.Fatalf("incomplete reason = %q, want %q; body=%s", got, tt.wantReason, wire)
			}
			if got := gjson.GetBytes(wire, "output.0.content.0.text").String(); got != tt.wantAnswer {
				t.Fatalf("answer = %q, want %q; body=%s", got, tt.wantAnswer, wire)
			}
			if gjson.GetBytes(wire, "type").Exists() || gjson.GetBytes(wire, "response").Exists() {
				t.Fatalf("raw event wrapper leaked into target response: %s", wire)
			}
		})
	}
}

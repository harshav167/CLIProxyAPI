package api

import (
	"bytes"
	"net/http"
	"testing"
)

func TestCodexTransportRoutesUseInternalWebsocketByDefault(t *testing.T) {
	t.Run("responses stream", func(t *testing.T) {
		fixture := newCodexTransportRouteFixture(t, "{}\n", codexRouteUpstreamSuccess)
		body := []byte(`{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"responses-route","input":[{"type":"message","role":"user","content":"hello"}]}`)

		response := fixture.request(t, "/v1/responses", body)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
		}
		if got := fixture.wsRequests.Load(); got != 1 {
			t.Fatalf("websocket requests = %d, want 1", got)
		}
		if got := fixture.httpRequests.Load(); got != 0 {
			t.Fatalf("HTTP requests = %d, want 0", got)
		}
		wire := response.Body.Bytes()
		for _, marker := range [][]byte{[]byte("route reasoning"), []byte("function_call"), []byte("route answer"), []byte(`"total_tokens":8`), []byte("response.completed")} {
			if !bytes.Contains(wire, marker) {
				t.Fatalf("responses SSE missing %q: %s", marker, wire)
			}
		}
	})

	t.Run("chat completions stream", func(t *testing.T) {
		fixture := newCodexTransportRouteFixture(t, "{}\n", codexRouteUpstreamSuccess)
		body := []byte(`{"model":"gpt-5-codex","stream":true,"user":"route-user","prompt_cache_key":"chat-route","messages":[{"role":"user","content":"hello"}]}`)

		response := fixture.request(t, "/v1/chat/completions", body)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
		}
		if got := fixture.wsRequests.Load(); got != 1 {
			t.Fatalf("websocket requests = %d, want 1", got)
		}
		if got := fixture.httpRequests.Load(); got != 0 {
			t.Fatalf("HTTP requests = %d, want 0", got)
		}
		wire := response.Body.Bytes()
		for _, marker := range [][]byte{
			[]byte(`"reasoning_content":"route reasoning"`),
			[]byte(`"name":"lookup"`),
			[]byte(`"arguments":"{}"`),
			[]byte(`"content":"route answer"`),
		} {
			if !bytes.Contains(wire, marker) {
				t.Fatalf("chat SSE missing %q: %s", marker, wire)
			}
		}
		if got := bytes.Count(wire, []byte("data: [DONE]")); got != 1 {
			t.Fatalf("done marker count = %d, want 1: %s", got, wire)
		}
		for _, marker := range [][]byte{[]byte(`"name":"lookup"`), []byte(`"arguments":"{}"`)} {
			if got := bytes.Count(wire, marker); got != 1 {
				t.Fatalf("logical tool field %q count = %d, want 1: %s", marker, got, wire)
			}
		}
		if bytes.Contains(wire, []byte("response.output_")) {
			t.Fatalf("raw Responses output event leaked into chat SSE: %s", wire)
		}
		if bytes.Contains(bytes.Join(fixture.upstreamBody, nil), []byte(`"type":"response.append"`)) {
			t.Fatalf("client-facing response.append leaked upstream: %q", fixture.upstreamBody)
		}
	})
}

func TestCodexTransportRoutesExplicitOptOutUsesHTTP(t *testing.T) {
	fixture := newCodexTransportRouteFixture(t, "codex-response-chaining:\n  enabled: false\n", codexRouteUpstreamSuccess)
	body := []byte(`{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"http-opt-out","input":[{"type":"message","role":"user","content":"hello"}]}`)

	response := fixture.request(t, "/v1/responses", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := fixture.wsRequests.Load(); got != 0 {
		t.Fatalf("websocket requests = %d, want 0", got)
	}
	if got := fixture.httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("route answer")) {
		t.Fatalf("HTTP opt-out response missing answer: %s", response.Body.String())
	}
}

func TestCodexTransportRoutesWebsocketBootstrapFallbackIsDuplicateFree(t *testing.T) {
	fixture := newCodexTransportRouteFixture(t, "{}\n", codexRouteUpstreamCloseBeforeOutput)
	body := []byte(`{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"route-fallback","input":[{"type":"message","role":"user","content":"hello"}]}`)

	response := fixture.request(t, "/v1/responses", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := fixture.wsRequests.Load(); got != 1 {
		t.Fatalf("websocket requests = %d, want 1", got)
	}
	if got := fixture.httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
	for eventType, want := range map[string]int{
		`"type":"response.created"`:           1,
		`"type":"response.output_text.delta"`: 1,
		`"type":"response.completed"`:         1,
	} {
		if got := bytes.Count(response.Body.Bytes(), []byte(eventType)); got != want {
			t.Fatalf("event %s count = %d, want %d without duplicate transport replay: %s", eventType, got, want, response.Body.String())
		}
	}
}

func TestCodexTransportRoutesResponsesTerminalFailureForwardedOnce(t *testing.T) {
	fixture := newCodexTransportRouteFixture(t, "{}\n", codexRouteUpstreamIncompleteMaxOutputTokens)
	body := []byte(`{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"route-incomplete","input":[{"type":"message","role":"user","content":"hello"}]}`)

	response := fixture.request(t, "/v1/responses", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := bytes.Count(response.Body.Bytes(), []byte(`"type":"response.incomplete"`)); got != 1 {
		t.Fatalf("response.incomplete count = %d, want 1: %s", got, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte("stream closed before response.completed")) {
		t.Fatalf("terminal failure produced synthetic timeout: %s", response.Body.String())
	}
}

func TestCodexTransportRoutesChatTerminalContract(t *testing.T) {
	tests := []struct {
		name         string
		configYAML   string
		mode         codexRouteUpstreamMode
		finishReason string
		errorMessage string
	}{
		{name: "websocket max output tokens", configYAML: "{}\n", mode: codexRouteUpstreamIncompleteMaxOutputTokens, finishReason: "length"},
		{name: "http max output tokens", configYAML: "codex-response-chaining:\n  enabled: false\n", mode: codexRouteUpstreamIncompleteMaxOutputTokens, finishReason: "length"},
		{name: "websocket content filter", configYAML: "{}\n", mode: codexRouteUpstreamIncompleteContentFilter, finishReason: "content_filter"},
		{name: "http content filter", configYAML: "codex-response-chaining:\n  enabled: false\n", mode: codexRouteUpstreamIncompleteContentFilter, finishReason: "content_filter"},
		{name: "websocket failed", configYAML: "{}\n", mode: codexRouteUpstreamFailed, errorMessage: "failed turn"},
		{name: "http failed", configYAML: "codex-response-chaining:\n  enabled: false\n", mode: codexRouteUpstreamFailed, errorMessage: "failed turn"},
		{name: "websocket response error", configYAML: "{}\n", mode: codexRouteUpstreamResponseError, errorMessage: "response error"},
		{name: "http response error", configYAML: "codex-response-chaining:\n  enabled: false\n", mode: codexRouteUpstreamResponseError, errorMessage: "response error"},
		{name: "websocket top-level error", configYAML: "{}\n", mode: codexRouteUpstreamTopLevelError, errorMessage: "top-level error"},
		{name: "http top-level error", configYAML: "codex-response-chaining:\n  enabled: false\n", mode: codexRouteUpstreamTopLevelError, errorMessage: "top-level error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCodexTransportRouteFixture(t, tt.configYAML, tt.mode)
			body := []byte(`{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"chat-terminal","messages":[{"role":"user","content":"hello"}]}`)

			response := fixture.request(t, "/v1/chat/completions", body)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			wire := response.Body.Bytes()
			if got := bytes.Count(wire, []byte("data: [DONE]")); got != 1 {
				t.Fatalf("done marker count = %d, want 1: %s", got, wire)
			}
			if tt.finishReason != "" {
				if got := bytes.Count(wire, []byte(`"finish_reason":"`+tt.finishReason+`"`)); got != 1 {
					t.Fatalf("finish_reason %q count = %d, want 1: %s", tt.finishReason, got, wire)
				}
				if bytes.Contains(wire, []byte(`"error":`)) {
					t.Fatalf("incomplete terminal became an error envelope: %s", wire)
				}
				return
			}
			if got := bytes.Count(wire, []byte(`"error":`)); got != 1 {
				t.Fatalf("error envelope count = %d, want 1: %s", got, wire)
			}
			if !bytes.Contains(wire, []byte(tt.errorMessage)) {
				t.Fatalf("error envelope missing %q: %s", tt.errorMessage, wire)
			}
		})
	}
}

package observability

import (
	"net/http"
	"testing"
)

func TestParseQuotaSamplesAnthropic(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.57")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.54")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")

	samples := parseQuotaSamples("claude", h)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d: %+v", len(samples), samples)
	}
	byWindow := map[string]quotaSample{}
	for _, s := range samples {
		byWindow[s.window] = s
	}
	if got := byWindow["5h"]; got.utilization != 0.57 || got.status != "allowed" {
		t.Fatalf("5h sample = %+v, want util=0.57 status=allowed", got)
	}
	if got := byWindow["7d"]; got.utilization != 0.54 {
		t.Fatalf("7d sample util = %v, want 0.54", got.utilization)
	}
}

func TestParseQuotaSamplesCodexNormalizesPercent(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42")
	h.Set("x-codex-secondary-primary-used-percent", "73.5")

	samples := parseQuotaSamples("codex", h)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d: %+v", len(samples), samples)
	}
	byWindow := map[string]quotaSample{}
	for _, s := range samples {
		byWindow[s.window] = s
	}
	// 0-100 scale must be normalized to 0.0-1.0.
	if got := byWindow["primary"].utilization; got != 0.42 {
		t.Fatalf("primary util = %v, want 0.42", got)
	}
	if got := byWindow["weekly"].utilization; got != 0.735 {
		t.Fatalf("weekly util = %v, want 0.735", got)
	}
}

func TestParseQuotaSamplesIgnoresGarbageAndUnknownProviders(t *testing.T) {
	// Out-of-range / unparseable Anthropic values yield no samples.
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "12.0") // >1, invalid
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "abc")  // unparseable
	if got := parseQuotaSamples("claude", h); len(got) != 0 {
		t.Fatalf("expected 0 samples for garbage, got %+v", got)
	}

	// Codex out-of-range (>100) is dropped.
	hc := http.Header{}
	hc.Set("x-codex-primary-used-percent", "250")
	if got := parseQuotaSamples("codex", hc); len(got) != 0 {
		t.Fatalf("expected 0 samples for >100 codex, got %+v", got)
	}

	// Providers without a continuous utilization header yield nothing.
	hx := http.Header{}
	hx.Set("x-ratelimit-remaining-requests", "100")
	for _, prov := range []string{"xai", "antigravity", "gemini", ""} {
		if got := parseQuotaSamples(prov, hx); len(got) != 0 {
			t.Fatalf("provider %q expected 0 samples, got %+v", prov, got)
		}
	}

	// Nil headers are safe.
	if got := parseQuotaSamples("claude", nil); got != nil {
		t.Fatalf("nil headers should yield nil, got %+v", got)
	}
}

package observability

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestRedactBodyForLogRemovesSecretsAndBoundsBody(t *testing.T) {
	body := []byte(`{"authorization":"Bearer sk-secret","x-api-key":"sk-api","token":"sk-abc def,ghi\"quoted","nested":{"password":"secret value"},"prompt":"keep this","note":"Bearer abc.def sk-proj-verysecret"}`)

	got := RedactBodyForLog(body, 512)
	for _, leaked := range []string{"sk-secret", "sk-api", "sk-abc", "def,ghi", "quoted", "secret value", "abc.def", "sk-proj-verysecret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted body leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, `"prompt":"keep this"`) {
		t.Fatalf("non-secret field unexpectedly removed: %q", got)
	}

	textBody := []byte(`token="sk-abc def,ghi" safe=ok Authorization: Bearer abc.def sk-proj-verysecret`)
	textGot := RedactBodyForLog(textBody, 200)
	for _, leaked := range []string{"sk-abc", "def,ghi", "abc.def", "sk-proj-verysecret"} {
		if strings.Contains(textGot, leaked) {
			t.Fatalf("redacted text body leaked %q in %q", leaked, textGot)
		}
	}
	if !strings.Contains(textGot, "safe=ok") {
		t.Fatalf("safe text unexpectedly removed: %q", textGot)
	}

	bounded := RedactBodyForLog([]byte(strings.Repeat("a", 200)), 32)
	if len(bounded) > 35 {
		t.Fatalf("bounded body length = %d, want <= 35", len(bounded))
	}
	if !strings.HasSuffix(bounded, "...") {
		t.Fatalf("bounded body %q missing ellipsis", bounded)
	}
}

func TestSummaryFromUsageIncludesCacheIdentityAndRatio(t *testing.T) {
	ctx := logging.WithCacheIdentity(logging.WithResponseStatusHolder(context.Background()), "conv-1", "pck-1")
	logging.SetResponseStatus(ctx, 502)

	summary := SummaryFromUsage(ctx, coreusage.Record{
		Provider: "openai",
		Model:    "gpt-5.5",
		Alias:    "gpt-5.5-extra",
		Latency:  1500 * time.Millisecond,
		TTFT:     250 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:         10,
			OutputTokens:        5,
			CacheReadTokens:     30,
			CacheCreationTokens: 10,
		},
	}, logging.RequestSummary{
		EndpointFamily:      "responses",
		RequestBytes:        123,
		ResponseBytes:       456,
		CacheControlSummary: "system.2{ttl=1h,scope=global}",
	})

	if summary.ConversationID != "conv-1" || summary.PromptCacheKey != "pck-1" {
		t.Fatalf("cache identity = (%q, %q), want conv-1/pck-1", summary.ConversationID, summary.PromptCacheKey)
	}
	if summary.Status != 502 || !summary.Failed || summary.ErrorStatus != 502 {
		t.Fatalf("status/failure = %d/%t/%d, want 502/true/502", summary.Status, summary.Failed, summary.ErrorStatus)
	}
	if summary.CacheHitRatio != 0.6 {
		t.Fatalf("cache hit ratio = %v, want 0.6", summary.CacheHitRatio)
	}
	if summary.CacheReadTokens != 30 || summary.CacheCreationTokens != 10 {
		t.Fatalf("cache split = read %d creation %d, want 30/10", summary.CacheReadTokens, summary.CacheCreationTokens)
	}
}

func TestSummaryFromUsagePropagatesTransportFailureWithClientStatus200(t *testing.T) {
	ctx := logging.WithRequestOutcomeHolder(logging.WithResponseStatusHolder(context.Background()))
	logging.SetResponseStatus(ctx, 200)
	logging.SetRequestOutcome(ctx, true, 0, "context canceled")

	summary := SummaryFromUsage(ctx, coreusage.Record{
		Provider: "openai",
		Model:    "gpt-5.5-extra",
		Failed:   true,
		Fail:     coreusage.Failure{Body: "context canceled"},
	}, logging.RequestSummary{
		Provider:       "openai",
		Model:          "gpt-5.5-extra",
		EndpointFamily: "responses",
	})

	if !summary.Failed {
		t.Fatal("expected failed summary")
	}
	if summary.ErrorStatus != http.StatusBadGateway {
		t.Fatalf("error status = %d, want %d", summary.ErrorStatus, http.StatusBadGateway)
	}
	if summary.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want propagated upstream failure status %d", summary.Status, http.StatusBadGateway)
	}
	if summary.ErrorMessage != "context canceled" {
		t.Fatalf("error message = %q, want context canceled", summary.ErrorMessage)
	}
}

func TestCacheHitRatioOpenAIStyleNormalizedInput(t *testing.T) {
	// After OpenAI parsing normalization: 100 total prompt with 80 cached =>
	// InputTokens=20, CacheReadTokens=80 => ratio 80/100 = 0.8
	ratio := cacheHitRatio(coreusage.Detail{
		InputTokens:     20,
		CacheReadTokens: 80,
	})
	if ratio != 0.8 {
		t.Fatalf("cache hit ratio = %v, want 0.8", ratio)
	}
}

func TestRecordRequestSummaryDisabledNoop(t *testing.T) {
	previous := active()
	SetActive(nil)
	RecordRequestSummary(context.Background(), RequestSummary{Model: "gpt-5.5"})

	SetActive(&State{settings: Settings{TransportLogs: false, TransportLogsFullBody: true}})
	RecordRequestSummary(context.Background(), RequestSummary{
		Model:        "gpt-5.5",
		RequestBody:  `{"authorization":"Bearer secret"}`,
		ResponseBody: `{"token":"secret"}`,
	})

	SetActive(previous)
}

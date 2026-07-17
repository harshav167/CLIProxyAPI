package auth

import (
	"net/http"
	"testing"
	"time"
)

type kimiOverloadStatusError struct {
	message string
}

func (e kimiOverloadStatusError) Error() string {
	return e.message
}

func (e kimiOverloadStatusError) StatusCode() int {
	return http.StatusTooManyRequests
}

func TestManagerShouldRetryKimiEngineOverloadWithoutRetryAfter(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(5, 5*time.Second, 5)
	auth := &Auth{
		ID:       "kimi-overload",
		Provider: "kimi",
		Metadata: map[string]any{"disable_cooling": true},
	}
	if _, err := m.Register(t.Context(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	err := kimiOverloadStatusError{
		message: `{"error":{"type":"engine_overloaded_error","message":"The engine is currently overloaded, please try again later"}}`,
	}

	wait, ok := m.shouldRetryAfterError(err, 0, []string{"kimi"}, "kimi-for-coding", 5*time.Second)
	if !ok {
		t.Fatal("shouldRetryAfterError() = false, want Kimi capacity retry")
	}
	if wait <= 0 || wait > 5*time.Second {
		t.Fatalf("retry wait = %v, want bounded positive delay", wait)
	}

	nextWait, ok := m.shouldRetryAfterError(err, 1, []string{"kimi"}, "kimi-for-coding", 5*time.Second)
	if !ok {
		t.Fatal("second shouldRetryAfterError() = false, want Kimi capacity retry")
	}
	if nextWait <= wait {
		t.Fatalf("second retry wait = %v, want exponential increase over %v", nextWait, wait)
	}
	if nextWait > 5*time.Second {
		t.Fatalf("second retry wait = %v, want max 5s", nextWait)
	}
}

func TestManagerDoesNotSynthesizeRetryForOtherKimi429(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(5, 5*time.Second, 5)
	auth := &Auth{ID: "kimi-quota", Provider: "kimi"}
	if _, err := m.Register(t.Context(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	err := kimiOverloadStatusError{
		message: `{"error":{"type":"rate_limit_error","message":"quota exhausted"}}`,
	}

	if wait, ok := m.shouldRetryAfterError(err, 0, []string{"kimi"}, "kimi-for-coding", 5*time.Second); ok {
		t.Fatalf("shouldRetryAfterError() = (%v, true), want no synthetic retry", wait)
	}
}

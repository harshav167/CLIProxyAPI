package codebuddy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestAuth(serverURL string) *CodeBuddyAuth {
	return &CodeBuddyAuth{
		httpClient: http.DefaultClient,
		baseURL:    serverURL,
	}
}

func fakeJWT(sub string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{"sub": sub, "iat": 1234567890})
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + encodedPayload + ".sig"
}

func TestFetchAuthState_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.URL.Path; got != codeBuddyStatePath {
			t.Errorf("expected path %s, got %s", codeBuddyStatePath, got)
		}
		if got := r.URL.Query().Get("platform"); got != "CLI" {
			t.Errorf("expected platform=CLI, got %s", got)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("expected User-Agent %s, got %s", UserAgent, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{
				"state":   "test-state-abc",
				"authUrl": "https://example.com/login?state=test-state-abc",
			},
		})
	}))
	defer srv.Close()

	auth := newTestAuth(srv.URL)
	result, err := auth.FetchAuthState(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.State != "test-state-abc" {
		t.Errorf("expected state 'test-state-abc', got '%s'", result.State)
	}
	if result.AuthURL != "https://example.com/login?state=test-state-abc" {
		t.Errorf("unexpected authURL: %s", result.AuthURL)
	}
}

func TestRefreshToken_Success(t *testing.T) {
	newAccessToken := fakeJWT("refreshed-user-456")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Refresh-Token"); got != "old-refresh-token" {
			t.Errorf("expected X-Refresh-Token 'old-refresh-token', got '%s'", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer old-access-token" {
			t.Errorf("expected Authorization 'Bearer old-access-token', got '%s'", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{
				"accessToken":      newAccessToken,
				"refreshToken":     "new-refresh-token",
				"expiresIn":        3600,
				"refreshExpiresIn": 86400,
				"tokenType":        "bearer",
				"domain":           "custom.domain.com",
			},
		})
	}))
	defer srv.Close()

	auth := newTestAuth(srv.URL)
	storage, err := auth.RefreshToken(context.Background(), "old-access-token", "old-refresh-token", "user-123", "custom.domain.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storage.AccessToken != newAccessToken {
		t.Errorf("expected new access token, got '%s'", storage.AccessToken)
	}
	if storage.RefreshToken != "new-refresh-token" {
		t.Errorf("expected 'new-refresh-token', got '%s'", storage.RefreshToken)
	}
	if storage.UserID != "refreshed-user-456" {
		t.Errorf("expected userID 'refreshed-user-456', got '%s'", storage.UserID)
	}
}

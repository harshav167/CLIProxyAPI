package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req2 := req.Clone(req.Context())
			req2.URL.Scheme = "http"
			req2.URL.Host = strings.TrimPrefix(srv.URL, "http://")
			return srv.Client().Transport.RoundTrip(req2)
		}),
	}
}

func TestFetchUserInfo_FullProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"login": "octocat",
			"email": "octocat@github.com",
			"name":  "The Octocat",
		})
	}))
	defer srv.Close()

	client := &DeviceFlowClient{httpClient: newTestClient(srv)}
	info, err := client.FetchUserInfo(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Login != "octocat" {
		t.Errorf("Login: got %q, want %q", info.Login, "octocat")
	}
	if info.Email != "octocat@github.com" {
		t.Errorf("Email: got %q, want %q", info.Email, "octocat@github.com")
	}
	if info.Name != "The Octocat" {
		t.Errorf("Name: got %q, want %q", info.Name, "The Octocat")
	}
}

func TestFetchUserInfo_EmptyToken(t *testing.T) {
	client := &DeviceFlowClient{httpClient: http.DefaultClient}
	_, err := client.FetchUserInfo(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}

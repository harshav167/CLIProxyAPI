package helps

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestProxyClient_ReusesConnectionsAcrossRequests proves that sequential
// requests built via NewProxyAwareHTTPClient (no proxy configured — the GLM /
// openai-compat hot path) reuse the same TCP connection instead of dialing a
// fresh one per request. It counts distinct server-side connections over N
// sequential requests: with pooling, that count should be 1; without a shared
// transport it grows with N.
func TestProxyClient_ReusesConnectionsAcrossRequests(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{}
	const n = 10
	for i := 0; i < n; i++ {
		client := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	dialCount := dials.Load()
	t.Logf("distinct server connections for %d sequential requests: %d", n, dialCount)
	if dialCount > 1 {
		t.Errorf("expected connection reuse (1 dial) across %d requests, got %d new connections", n, dialCount)
	}
}

// TestProxyClient_ReusesConnectionsWithProxyConfigured checks the proxy-configured
// path, where buildProxyTransport currently builds a FRESH *http.Transport on
// every call. A fresh transport = a fresh (empty) connection pool, so sequential
// requests cannot reuse connections. We emulate "proxy configured" via a direct
// setting (auth.ProxyURL="direct"), which still routes through buildProxyTransport
// and returns a per-call transport.
func TestProxyClient_ReusesConnectionsWithProxyConfigured(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	// Guard the test's own premise: "direct" must resolve to a real (non-nil)
	// transport so this path provably exercises the proxy-configured branch of
	// NewProxyAwareHTTPClient. If proxyutil ever stopped treating "direct" as a
	// buildable setting, buildProxyTransport would return nil, the no-proxy
	// branch would silently take over, and this test would no longer cover what
	// it claims to.
	if buildProxyTransport("direct") == nil {
		t.Fatal("buildProxyTransport(\"direct\") returned nil; test would not exercise the proxy-configured path")
	}

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct"} // routes through buildProxyTransport
	const n = 10
	for i := 0; i < n; i++ {
		client := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	dialCount := dials.Load()
	t.Logf("[proxy=direct] distinct server connections for %d sequential requests: %d", n, dialCount)
	if dialCount > 1 {
		t.Errorf("expected connection reuse (1 dial) across %d requests, got %d new connections (fresh transport per call)", n, dialCount)
	}
}

package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type codexWebsocketTestServer struct {
	server      *httptest.Server
	connections atomic.Int64
	closed      chan struct{}
}

func newCodexWebsocketTestServer(t *testing.T) *codexWebsocketTestServer {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	fixture := &codexWebsocketTestServer{closed: make(chan struct{}, 8)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		fixture.connections.Add(1)
		defer func() {
			_ = conn.Close()
			fixture.closed <- struct{}{}
		}()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *codexWebsocketTestServer) websocketURL() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http") + "/responses"
}

func (s *codexWebsocketTestServer) waitForClose(t *testing.T) {
	t.Helper()
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket close")
	}
}

func TestCodexWebsocketConnectionReuse(t *testing.T) {
	server := newCodexWebsocketTestServer(t)
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sess := exec.getOrCreateSession("reuse")
	t.Cleanup(func() { exec.closeExecutionSession(sess, "test_cleanup") })

	first, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", server.websocketURL(), nil)
	if err != nil {
		t.Fatalf("first ensureUpstreamConn() error = %v", err)
	}
	second, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", server.websocketURL(), nil)
	if err != nil {
		t.Fatalf("second ensureUpstreamConn() error = %v", err)
	}

	if second != first {
		t.Fatal("matching fresh connection was not reused")
	}
	if got := server.connections.Load(); got != 1 {
		t.Fatalf("connections = %d, want 1", got)
	}
}

func TestCodexWebsocketConnectionAuthChangeRedials(t *testing.T) {
	server := newCodexWebsocketTestServer(t)
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sess := exec.getOrCreateSession("auth-change")
	t.Cleanup(func() { exec.closeExecutionSession(sess, "test_cleanup") })

	first, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", server.websocketURL(), nil)
	if err != nil {
		t.Fatalf("first ensureUpstreamConn() error = %v", err)
	}
	second, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-2", server.websocketURL(), nil)
	if err != nil {
		t.Fatalf("second ensureUpstreamConn() error = %v", err)
	}

	if second == first {
		t.Fatal("auth change reused the previous connection")
	}
	if got := server.connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2", got)
	}
}

func TestCodexWebsocketConnectionURLChangeRedials(t *testing.T) {
	firstServer := newCodexWebsocketTestServer(t)
	secondServer := newCodexWebsocketTestServer(t)
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sess := exec.getOrCreateSession("url-change")
	t.Cleanup(func() { exec.closeExecutionSession(sess, "test_cleanup") })

	first, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", firstServer.websocketURL(), nil)
	if err != nil {
		t.Fatalf("first ensureUpstreamConn() error = %v", err)
	}
	second, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", secondServer.websocketURL(), nil)
	if err != nil {
		t.Fatalf("second ensureUpstreamConn() error = %v", err)
	}

	if second == first {
		t.Fatal("URL change reused the previous connection")
	}
	if got := firstServer.connections.Load(); got != 1 {
		t.Fatalf("first server connections = %d, want 1", got)
	}
	if got := secondServer.connections.Load(); got != 1 {
		t.Fatalf("second server connections = %d, want 1", got)
	}
}

func TestCodexWebsocketConnectionAgeRedials(t *testing.T) {
	server := newCodexWebsocketTestServer(t)
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sess := exec.getOrCreateSession("aged")
	t.Cleanup(func() { exec.closeExecutionSession(sess, "test_cleanup") })

	first, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", server.websocketURL(), nil)
	if err != nil {
		t.Fatalf("first ensureUpstreamConn() error = %v", err)
	}
	sess.connMu.Lock()
	firstGeneration := sess.connGeneration
	firstWindowGeneration := sess.windowGen
	sess.connCreatedAt = time.Now().Add(-55*time.Minute - time.Second)
	sess.turnState = "stale-turn-state"
	sess.warmedUp = true
	sess.warmedUpGen = firstGeneration
	sess.connMu.Unlock()

	second, _, err := exec.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", server.websocketURL(), nil)
	if err != nil {
		t.Fatalf("second ensureUpstreamConn() error = %v", err)
	}

	if second == first {
		t.Fatal("aged connection was reused")
	}
	server.waitForClose(t)
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	if sess.connGeneration != firstGeneration+1 {
		t.Fatalf("connGeneration = %d, want %d", sess.connGeneration, firstGeneration+1)
	}
	if sess.windowGen != firstWindowGeneration+1 {
		t.Fatalf("windowGen = %d, want %d", sess.windowGen, firstWindowGeneration+1)
	}
	if sess.turnState != "" {
		t.Fatalf("turnState = %q, want empty", sess.turnState)
	}
	if sess.warmedUpGen == sess.connGeneration {
		t.Fatalf("warmedUpGen = %d, must not match new generation %d", sess.warmedUpGen, sess.connGeneration)
	}
}

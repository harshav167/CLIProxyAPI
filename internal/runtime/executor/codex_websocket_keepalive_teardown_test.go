package executor

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexWebsocketKeepaliveStopsBeforeOutputClose(t *testing.T) {
	out := make(chan cliproxyexecutor.StreamChunk)
	stop := make(chan struct{})
	done := make(chan struct{})
	go runCursorKeepalive(
		context.Background(),
		out,
		[][]byte{[]byte(`{"type":"response.in_progress"}`)},
		time.Millisecond,
		stop,
		done,
		"test-session",
	)

	cleanupCalled := false
	teardown := &codexWebsocketStreamTeardown{
		stopKeepalive:    func() { close(stop) },
		keepaliveDone:    done,
		keepaliveStarted: true,
		cleanup: func() {
			select {
			case <-done:
			default:
				t.Fatal("cleanup ran before keepalive stopped")
			}
			select {
			case _, open := <-out:
				if !open {
					t.Fatal("output closed before cleanup")
				}
			default:
			}
			cleanupCalled = true
		},
		out: out,
	}

	teardown.finish()

	if !cleanupCalled {
		t.Fatal("stream cleanup did not run")
	}
	if _, open := <-out; open {
		t.Fatal("output remained open after teardown")
	}
}

package executor

import cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"

type codexWebsocketStreamTeardown struct {
	stopKeepalive    func()
	keepaliveDone    <-chan struct{}
	keepaliveStarted bool
	cleanup          func()
	out              chan cliproxyexecutor.StreamChunk
}

func (t *codexWebsocketStreamTeardown) finish() {
	t.stopKeepalive()
	if t.keepaliveStarted {
		<-t.keepaliveDone
	}
	t.cleanup()
	close(t.out)
}

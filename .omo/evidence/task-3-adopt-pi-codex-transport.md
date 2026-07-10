# Task 3 Evidence: Upstream WebSocket Maximum Age

## Impact

- `ensureUpstreamConn`: LOW, 3 upstream symbols and the Codex `ExecuteStream` process.
- `codexWebsocketSession`: LOW, 7 upstream symbols across Codex and xAI executor processes.

## Red

Command:

```text
go test -run 'TestCodexWebsocketConnection(Reuse|Age|Auth|URL)' ./internal/runtime/executor
```

Result: build failed because `codexWebsocketSession.connCreatedAt` did not exist.

## Green

Commands:

```text
go test -run 'TestCodexWebsocketConnection(Reuse|Age|Auth|URL)' ./internal/runtime/executor
go test -race -run 'TestCodexWebsocketConnection(Reuse|Age|Auth|URL)' ./internal/runtime/executor
go test ./internal/runtime/executor
```

Results: focused suite 4 passed; focused race suite 4 passed; package suite 521 passed.

## Assertions

- A fresh socket with matching auth and URL is reused.
- Auth or URL changes close the old socket and redial.
- A socket older than 55 minutes is closed and redialed before reuse.
- Redial increments connection and window generations, clears turn state, and invalidates the previous warmup generation.
- No timer goroutine or new configuration surface was added.

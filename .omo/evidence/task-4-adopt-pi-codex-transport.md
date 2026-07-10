# Task 4 Evidence: Recoverable Recycle vs Terminal Shutdown

## Impact

- `invalidateUpstreamConn`: MEDIUM, 7 upstream symbols in the Codex execution path.
- `closeCodexWebsocketSession`: LOW, 8 upstream symbols across executor and SDK lifecycle paths.
- `CloseCodexWebsocketSessionsForAuthID`: LOW, 5 upstream SDK lifecycle symbols.

## Red

Command:

```text
go test -run 'TestCodexWebsocket.*(Recycle|Terminate|Disconnect|AuthRemoval)' ./internal/runtime/executor
```

Result: build failed because `recycleUpstreamConn` and `terminateUpstreamSession` did not exist.

## Green

Commands:

```text
go test -run 'TestCodexWebsocket.*(Recycle|Terminate|Disconnect|AuthRemoval|CloseExecutionSession)|TestCloseCodexWebsocketSessionsForAuthIDNotifiesOnce' ./internal/runtime/executor
go test -race -run 'TestCodexWebsocket.*(Recycle|Terminate|Disconnect|AuthRemoval|CloseExecutionSession)|TestCloseCodexWebsocketSessionsForAuthIDNotifiesOnce' ./internal/runtime/executor
go test ./internal/runtime/executor
```

Results: focused suite 10 passed; focused race suite 10 passed; package suite 530 passed.

## Assertions

- Send, auth/URL, age, ping, read, cancellation, binary, and upstream-error paths recycle only the affected connection.
- Recoverable recycling does not send or close the downstream disconnect channel.
- Auth removal routes through terminal shutdown, notifies once, and closes the channel.
- Downstream-initiated `CloseExecutionSession` closes silently.
- Old Codex `invalidateUpstreamConn` and `closeCodexWebsocketSession` call sites were removed.
- A stale read/ping loop cannot notify a later connection generation.

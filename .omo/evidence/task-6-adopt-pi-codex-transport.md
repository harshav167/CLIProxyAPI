# Task 6 Evidence: Bounded Pre-Output WebSocket Fallback

## Impact

- `CodexAutoExecutor.ExecuteStream`: LOW, two indexed upstream symbols.
- `CodexAutoExecutor.bridgedExecuteStream`: LOW, two indexed upstream symbols.
- Change detection found one touched production symbol and no affected indexed processes.

## Red

Command:

```text
go test ./internal/runtime/executor -run 'TestCodexAutoExecuteStream.*(ConnectionLimit|HTTPFallback|ReadClose|PostStart)' -count=1
```

Result: one post-start guard passed; three pre-output recovery cases failed. Connection-limit stopped after one WebSocket request, a second connection-limit never reached HTTP, and an upstream read close produced no HTTP request.

## Green

Commands:

```text
go test ./internal/runtime/executor -run 'Test(CodexAutoExecuteStream.*(ConnectionLimit|HTTPFallback|ReadClose|PostStart)|CodexWebsocketTransportBootstrapErrorClassification|BootstrapCodexStreamReplaysFirstPayloadAndHeaders)$' -count=1
go test -race ./internal/runtime/executor -run 'Test(CodexAutoExecuteStream.*(ConnectionLimit|HTTPFallback|ReadClose|PostStart)|CodexWebsocketTransportBootstrapErrorClassification|BootstrapCodexStreamReplaysFirstPayloadAndHeaders)$' -count=1
go test ./internal/runtime/executor -count=1
```

Results: focused suite 11 passed; focused race suite 11 passed; executor package suite 551 passed.

## Assertions

- A connection-limit error before visible output retries exactly once on a fresh upstream WebSocket.
- A second pre-output transport failure disables WebSocket only for the existing recovery window and falls back to HTTP.
- Generic WebSocket close/read failure before output falls back directly to HTTP.
- The first non-empty downstream payload commits the transport choice; later errors are forwarded without retry or fallback.
- Buffered bootstrap chunks and original response headers are replayed exactly once.
- Quota, context-length, invalid-request, caller cancellation, and caller deadline errors are not classified as transport fallback signals.
- Bridge state is reset before retry and fallback, so a reconnect uses a full request rather than a stale delta.

# Task 2 Evidence: Codex Responses Terminal Classifier

## Impact

- `ParseCodexWebsocketWarmupEvent`: MEDIUM, 10 upstream symbols and the Codex `ExecuteStream` process.
- `runCodexWebsocketWarmup`: LOW, 2 upstream symbols in the executor path.

## Red

Command:

```text
go test -run 'Test.*Codex.*Event' ./internal/runtime/executor/helps
```

Result: build failed because `ClassifyCodexResponsesEvent` did not exist.

## Green

Commands:

```text
go test -run 'Test.*Codex.*Event' ./internal/runtime/executor/helps
go test ./internal/runtime/executor/helps ./internal/runtime/executor
```

Results: focused suite 11 passed; package suites 691 passed.

## Assertions

- `response.completed` and compatibility `response.done` are successful terminals.
- `response.failed`, `response.incomplete`, `response.error`, and top-level `error` are failure terminals.
- SSE-framed and raw JSON payloads share one classifier.
- Successful terminals extract nested or top-level response IDs.
- Unknown and malformed payloads remain non-terminal.
- The warmup-only parser and result type were removed; `runCodexWebsocketWarmup` uses the shared classifier.

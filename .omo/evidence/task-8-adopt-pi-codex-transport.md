# Task 8 Evidence: Public Codex Transport Route Contracts

## Impact

- `/v1/chat/completions` and `/v1/responses` are registered by `Server.setupRoutes` and exercise the real OpenAI handlers, auth manager, `CodexAutoExecutor`, and in-process upstream HTTP/WebSocket transports.
- GitNexus route lookup did not index either route, so direct route registration inspection and real `httptest` requests were used.
- Task 8 changes test files only; no route, handler, executor, translator, or public schema changed.

## Green

Commands:

```text
go test ./internal/api -run 'TestCodexTransportRoutes' -count=1
go test -race ./internal/api -run 'TestCodexTransportRoutes' -count=1
go test -race ./sdk/api/handlers/openai -run 'Test(ForwardResponsesWebsocket(FailureTerminalPreservedOnce|PreservesCompletedEvent|TreatsResponseDoneAsTerminalWithoutRewriting)|ResponsesWebsocketPassthroughReplaysTranscriptAfterPreviousResponseNotFound|RepairResponsesWebsocketToolCalls.*Custom|RecordResponsesWebsocketCustomToolCalls.*)$' -count=1
```

Results: route suite 7 passed; route race suite 7 passed; raw downstream WebSocket/custom-tool race suite 9 passed.

## Package Baseline Resolution

The first `go test ./internal/api -count=1` run exposed a stale catalog-priority expectation (129 versus the existing 43 + 100 contract). The final verification wave corrected that test-only expectation without changing model metadata or runtime behavior. `go test -race ./internal/api -count=1` now passes all 40 tests.

## Assertions

- Streaming `/v1/responses` and `/v1/chat/completions` select internal upstream WebSocket when response chaining is enabled.
- Explicit response-chaining opt-out keeps streaming `/v1/responses` on HTTP.
- Responses SSE preserves reasoning, function-call, text, usage, and terminal event shapes.
- Chat Completions SSE preserves answer chunks and the `[DONE]` marker.
- Pre-output WebSocket close falls back to valid HTTP/SSE with exactly one created, text-delta, and completed event.
- `response.incomplete` is forwarded once without a synthetic completion timeout.
- Non-stream Responses remains HTTP and aggregates response ID and usage.
- Captured upstream requests never use client-facing `response.append`.
- Existing raw downstream Responses WebSocket tests cover completed/done/failure preservation and custom-tool repair/dedupe behavior.

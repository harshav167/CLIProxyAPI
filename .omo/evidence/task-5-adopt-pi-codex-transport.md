# Task 5 Evidence: Shared Terminal Contract Consumers

## Impact

- `CodexWebsocketsExecutor.ExecuteStream`: LOW, no indexed upstream symbol dependents.
- `OpenAIResponsesAPIHandler.forwardResponsesWebsocket`: LOW, one direct caller (`ResponsesWebsocket`).
- `runFoldLoop`: LOW, four upstream symbols across the HTTP and WebSocket Codex execution paths.
- Route-level impact lookup found no indexed `/v1/responses` route, so symbol impact was used instead.
- The index did not yet expose the newly split `scanOneRound` symbol; package tests cover its callers.

## Red

Focused executor and handler tests initially showed that `response.failed`, `response.incomplete`, and `response.error` did not close the executor turn. The downstream handler forwarded the failure event and then emitted a synthetic `stream closed before response.completed` timeout, producing two payloads instead of one.

## Green

Commands:

```text
go test ./internal/runtime/executor ./sdk/api/handlers/openai -run 'Test(CodexWebsocketFailureTerminalClosesTurn|ForwardResponsesWebsocketFailureTerminalPreservedOnce|CodexContinueFoldTreatsResponse(DoneAsSuccessfulTerminal|ErrorAsTerminal))$' -count=1
go test -race ./internal/runtime/executor ./sdk/api/handlers/openai -run 'Test(CodexWebsocketFailureTerminalClosesTurn|ForwardResponsesWebsocketFailureTerminalPreservedOnce|CodexContinueFoldTreatsResponse(DoneAsSuccessfulTerminal|ErrorAsTerminal))$' -count=1
go test -race ./internal/runtime/executor ./sdk/api/handlers/openai -run 'Test(CodexWebsocketFailureTerminalClosesTurn|ForwardResponsesWebsocketFailureTerminalPreservedOnce|CodexContinueFoldTreatsResponse(DoneAsSuccessfulTerminal|ErrorAsTerminal)|CodexContinueFoldReconstructsTerminalIdentityOutputMetadataAndUsage|CodexExecutorExecute(Stream)?SurfacesTerminalStreamError)$' -count=1
go test ./internal/runtime/executor ./sdk/api/handlers/openai -count=1
```

Results: targeted suite 10 passed; targeted race suite 10 passed; expanded terminal race suite 13 passed; affected package suite 705 passed.

## Assertions

- `response.completed` and `response.done` end the turn successfully and retain output, response ID, usage, and original event type.
- `response.failed`, `response.incomplete`, `response.error`, and top-level `error` end the turn without waiting for idle timeout.
- Failure terminal payloads are forwarded exactly once and do not trigger a synthetic completion timeout.
- Fold and non-fold scanners, raw downstream WebSocket passthrough, bridge observation, keepalive shutdown, and handler forwarding use `ClassifyCodexResponsesEvent` for terminal decisions.
- Failure events are not rewritten into successful completion events.

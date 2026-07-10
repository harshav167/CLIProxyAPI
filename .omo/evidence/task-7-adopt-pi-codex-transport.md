# Task 7 Evidence: Passthrough Transcript Recovery

## Impact

- `OpenAIResponsesAPIHandler.ResponsesWebsocket`: LOW direct symbol impact.
- `normalizeResponsesWebsocketRequestWithIncrementalState`: LOW, 11 indexed upstream symbols and one Responses WebSocket process.
- Change detection found four affected WebSocket handler processes; focused and package tests cover the shared handler path.

## Red

Command:

```text
go test ./sdk/api/handlers/openai -run 'TestResponsesWebsocketPassthroughReplaysTranscriptAfterPreviousResponseNotFound$' -count=1
```

Result: after `previous_response_not_found`, passthrough had no shadow `lastRequest` and cleared its remembered model. The next request failed locally with `missing model in response.create request` instead of reaching the executor as a full replay.

## Green

Commands:

```text
go test ./sdk/api/handlers/openai -run 'TestResponsesWebsocket.*(Passthrough|Replay|PreviousResponseNotFound)' -count=1
go test -race ./sdk/api/handlers/openai -run 'TestResponsesWebsocket.*(Passthrough|Replay|PreviousResponseNotFound)' -count=1
go test ./sdk/api/handlers/openai -count=1
```

Results: focused suite 5 passed; focused race suite 5 passed; handler package suite 170 passed.

## Assertions

- Normal passthrough requests retain their upstream WebSocket request type, previous response ID, and incremental input shape.
- A separate shadow transcript is normalized through the existing merge, repair, and dedupe owners while passthrough bytes remain unchanged.
- Cache loss rolls shadow state back to the last successful turn and schedules exactly one mediated full replay.
- The recovery replay omits the stale `previous_response_id` and includes prior user, function call, function output, and follow-up user items exactly once.
- A successful replay updates response state and the following request resumes byte-preserving passthrough.

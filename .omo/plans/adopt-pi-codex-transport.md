# adopt-pi-codex-transport - Work Plan

## TL;DR (For humans)
**What you'll get:** Codex requests will prefer a robust, reusable WebSocket from the proxy to OpenAI, with automatic recovery to HTTP before output starts. Existing clients keep using the same HTTP/SSE and Responses interfaces, and lost connection-local context is rebuilt safely instead of hanging or dropping turns.

**Why this approach:** The proxy already implements nearly all of Pi's useful transport behavior, so the smallest reliable change is to harden that existing path. One owner chooses the transport, one event contract ends turns, and one transcript path handles recovery.

**What it will NOT do:** It will not port Pi's architecture, add dependencies, change client-facing WebSocket preference, rewrite legacy completions, touch local credentials/config, or retry after output has begun.

**Effort:** Large
**Risk:** High - connection reuse, fallback, and replay are concurrent stateful paths where a duplicate turn or premature close is worse than a visible failure.
**Decisions to sanity-check:** Internal WebSocket becomes the omitted-config default; sockets recycle at 55 minutes; client-facing WebSocket preference remains off; implementation and review use no subagents.

Your next move: commit this approved planning checkpoint, then implement it directly without delegation. Full execution detail follows below.

---

> TL;DR (machine): Large/high-risk TDD hardening of the existing Codex auto HTTP/WS selector, terminal contract, connection lifecycle, pre-output fallback, transcript recovery, and route contracts; no downstream WS rollout or Pi code port.

## Scope
### Must have
- Keep `CodexAutoExecutor` as the single Codex transport selector.
- Make internal response chaining default-on when omitted, with explicit `enabled: false` preserved.
- Keep downstream model metadata at `prefer_websockets: false`.
- Use `response.create` plus `previous_response_id` for upstream continuation; never emit upstream `response.append`.
- Use one shared terminal-event classifier across warmup, Codex WebSocket execution, and downstream Responses WebSocket forwarding.
- Treat `response.completed` and compatibility `response.done` as successful terminals; treat `response.failed`, `response.incomplete`, `response.error`, and top-level `error` as terminal failures.
- Recycle upstream sockets before OpenAI's 60-minute limit using a 55-minute maximum age.
- Preserve auth-ID and upstream-URL affinity on every reused socket.
- Separate recoverable upstream socket recycling from terminal execution-session notification.
- Retry a fresh upstream WebSocket at most once for a connection-limit failure before output; otherwise fall back to HTTP only before the first downstream payload.
- Recover `store=false` sessions from cache loss with the existing full-transcript replay path, including passthrough sessions.
- Preserve Chat Completions and Responses output contracts for reasoning, tools, usage, terminal errors, and non-stream aggregation.
- Use TDD and execute all implementation/review directly without subagents.
### Must NOT have (guardrails, anti-slop, scope boundaries)
- Do not port Pi's TypeScript class structure, process-global session cache, or assistant-message diagnostics.
- Do not add a dependency, a new transport package, a second executor hierarchy, or a new retry framework.
- Do not modify local `config.yaml`, credentials, auth files, production config, or deployment state.
- Do not advertise downstream WebSocket preference or alter `prefer_websockets` model metadata.
- Do not rewrite legacy `/v1/completions`; only protect it from incidental regressions if shared code changes.
- Do not broaden translator refactors beyond behavior required by these transport invariants.
- Do not replay, retry, switch auth, or switch transport after a downstream payload has been emitted.
- Do not add lint/test suppressions, skip flags, weakened assertions, or compatibility shims for unshipped branch code.
- Do not use subagents for implementation, review, or verification.

## Verification strategy
> Zero human intervention - all verification is agent-executed.
- Test decision: TDD with Go `testing`, `httptest`, Gorilla WebSocket test servers, and existing handler/auth-manager fakes.
- Every behavior todo starts with a focused failing test, records the red result, implements the minimum change, then records the green result.
- Targeted checks: use the exact focused `go test -run` command listed in each todo.
- Concurrency checks: `go test -race -run 'Test(Codex|HTTPToWSBridge|ForwardResponsesWebsocket)' ./internal/runtime/executor ./sdk/api/handlers/openai`.
- Package gate: `go test ./internal/config ./internal/runtime/executor ./sdk/cliproxy/auth ./sdk/api/handlers/openai`.
- Repository gate: run `gofmt -w` on every changed Go file reported by `git diff --name-only`, then `go build -o test-output ./cmd/server && rm test-output`, then `go test ./...`.
- Structural gate: GitNexus impact before each edited symbol and `gitnexus_detect_changes(scope="all")` before commits.
- Evidence: `.omo/evidence/task-{1..8}-adopt-pi-codex-transport.md` records red/green commands, key assertions, and final output for each todo.

## Execution strategy
### Parallel execution waves
> Target 5-8 todos per wave. Fewer than 3 (except the final) means you under-split.
- User constraint: one direct implementer, zero subagents. Code edits execute sequentially even when the dependency graph permits parallel work; independent test commands may run in parallel.
- Wave 1, foundations: todos 1-4 establish defaults, terminal classification, absolute connection age, and explicit connection-close semantics.
- Wave 2, recovery behavior: todos 5-7 implement safe pre-output fallback, passthrough replay recovery, and consume the shared terminal contract end to end.
- Wave 3, public contracts: todo 8 proves `/v1/chat/completions` and `/v1/responses` retain their wire behavior across internal transport selection.
- After each todo, re-read edited files to verify formatter/hooks did not mutate the intended change.

### Dependency matrix
| Todo | Depends on | Blocks | Can parallelize with |
| --- | --- | --- | --- |
| 1 | none | 8 | 2, 3, 4 |
| 2 | none | 5, 7, 8 | 1, 3, 4 |
| 3 | none | 5, 6, 8 | 1, 2, 4 |
| 4 | none | 5, 6 | 1, 2, 3 |
| 5 | 2, 3, 4 | 8 | 6 after shared executor edits settle |
| 6 | 3, 4 | 8 | 5 after shared executor edits settle |
| 7 | 2 | 8 | none; shares handler state with todo 8 |
| 8 | 1, 2, 3, 4, 5, 6, 7 | final verification | none |

## Todos
> Implementation + Test = ONE todo. Never separate.
- [x] 1. Default internal Codex response chaining when config omits the key
  What to do / Must NOT do: First add failing config tests proving both `LoadConfigOptional` and `ParseConfigBytes` default `CodexResponseChaining.Enabled` to true when `codex-response-chaining` is absent and preserve explicit `enabled: false`. Add the smallest shared default initializer needed to keep all config parse/optional-empty paths aligned. Do not touch `config.yaml`, do not remove the existing public config field, and do not change downstream `prefer_websockets` metadata.
  Parallelization: Wave 1 | Blocked by: none | Blocks: 8
  References: `internal/config/config.go` (`Config.CodexResponseChaining`, `LoadConfigOptional`); `internal/config/parse.go` (`ParseConfigBytes`); `sdk/api/handlers/openai/codex_client_models.go` (`prefer_websockets`).
  Acceptance criteria: absent key yields true in both parsers; explicit false yields false; explicit true yields true; optional missing/empty config uses the same default; `codex_client_models.go` remains unchanged.
  QA scenarios: happy - run `go test -run 'Test.*CodexResponseChaining.*Default' ./internal/config`; failure - run the explicit-false test and prove YAML does not get overwritten by the default. Record `.omo/evidence/task-1-adopt-pi-codex-transport.md`.
  Commit: Y | `feat(config): default codex response chaining on`

- [x] 2. Make one Codex Responses terminal-event classifier authoritative
  What to do / Must NOT do: Start with table-driven failing tests for completed, done, failed, incomplete, response.error, top-level error, non-terminal events, SSE-framed payloads, malformed JSON, and response-ID extraction. Replace `ParseCodexWebsocketWarmupEvent` and `CodexWebsocketWarmupEvent` with a generic `ClassifyCodexResponsesEvent` result that exposes terminal/success/failure and response ID; update the sole production caller and existing tests in the same todo. Do not retain a compatibility wrapper because the symbol is internal and this branch behavior is unshipped. Do not normalize failure events into success.
  Parallelization: Wave 1 | Blocked by: none | Blocks: 5, 7, 8
  References: `internal/runtime/executor/helps/codex_websocket_warmup.go` (`ParseCodexWebsocketWarmupEvent`); `internal/runtime/executor/helps/codex_websocket_warmup_test.go`; `.omo/drafts/codex-transport-adoption-research.md` (`Event Semantics`).
  Acceptance criteria: one helper owns the full terminal taxonomy; successful terminals expose response ID; failure terminals are terminal but not successful; non-terminals remain open; no duplicate terminal list remains in `helps`.
  QA scenarios: happy - run `go test -run 'Test.*Codex.*Event' ./internal/runtime/executor/helps`; failure - include malformed/unknown events and prove they cannot terminate a turn. Record `.omo/evidence/task-2-adopt-pi-codex-transport.md`.
  Commit: Y | `refactor(codex): centralize responses terminal classification`

- [x] 3. Recycle upstream sockets before the 60-minute service limit
  What to do / Must NOT do: Add failing tests around `ensureUpstreamConn` proving a fresh socket with matching auth/URL is reused, auth or URL changes force redial, and a connection created 55 minutes ago is closed and redialed before reuse. Add connection creation time to `codexWebsocketSession` and enforce the maximum age inside `ensureUpstreamConn`, where every natural caller passes. Reconnect must increment `connGeneration`, clear connection-local `turnState`, advance `windowGen`, and require warmup for the new generation. Do not add a timer goroutine or configurable duration unless tests prove an existing config owner already exists.
  Parallelization: Wave 1 | Blocked by: none | Blocks: 5, 6, 8
  References: `internal/runtime/executor/codex_websockets_executor.go` (`codexWebsocketSession`, `ensureUpstreamConn`, `connGeneration`, `warmedUpGen`); Pi reference `packages/ai/src/api/openai-codex-responses.ts` (`SESSION_WEBSOCKET_MAX_AGE_MS`); OpenAI WebSocket guide cited in the research note.
  Acceptance criteria: matching fresh connection reuses; auth, URL, or age mismatch redials; old connection is closed; connection-local state resets once; no in-flight request is interrupted because `reqMu` remains the serialization owner.
  QA scenarios: happy - run `go test -run 'TestCodexWebsocket.*(Reuse|Age|Auth|URL)' ./internal/runtime/executor`; failure - set creation time beyond 55 minutes and prove the stale socket cannot be returned. Record `.omo/evidence/task-3-adopt-pi-codex-transport.md`.
  Commit: Y | `fix(codex): recycle aged websocket connections`

- [x] 4. Separate recoverable connection recycle from terminal session shutdown
  What to do / Must NOT do: Add failing tests proving send-error redial, auth/URL change, age rollover, ping failure, and idle read failure clear/close only the affected upstream connection without closing `upstreamDisconnectCh`; auth removal/session termination must notify exactly once and close the downstream subscription. Replace `invalidateUpstreamConn` with `recycleUpstreamConn` for recoverable connection loss and add `terminateUpstreamSession` for notify-once terminal shutdown; do not use a boolean `notify` argument. Route `CloseCodexWebsocketSessionsForAuthID` through `terminateUpstreamSession`, while ordinary `CloseExecutionSession` closes silently because the downstream handler initiated it.
  Parallelization: Wave 1 | Blocked by: none | Blocks: 5, 6
  References: `internal/runtime/executor/codex_websockets_executor.go` (`notifyUpstreamDisconnect`, `invalidateUpstreamConn`, `closeCodexWebsocketSession`, `CloseCodexWebsocketSessionsForAuthID`); `sdk/api/handlers/openai/openai_responses_websocket.go` (`UpstreamDisconnectChan` subscription).
  Acceptance criteria: recoverable recycle never closes downstream notification; terminal auth removal notifies once; closed/replaced connections cannot notify a later generation; all old ambiguous call sites are removed.
  QA scenarios: happy - run `go test -run 'TestCodexWebsocket.*(Recycle|Disconnect|AuthRemoval)' ./internal/runtime/executor`; failure - simulate a first send failure followed by successful redial and prove the downstream disconnect channel remains open. Record `.omo/evidence/task-4-adopt-pi-codex-transport.md`.
  Commit: Y | `fix(codex): separate websocket recycle from shutdown`

- [x] 5. Apply the shared terminal contract through executor and downstream forwarding
  What to do / Must NOT do: Write failing executor and handler tests for successful completed/done and terminal failed/incomplete/response.error/error events. Update warmup, fold/non-fold executor branches, raw downstream-WebSocket passthrough, and `forwardResponsesWebsocket` to consume the shared classifier. Rename local `completed` state to terminal/success state where needed so a failure terminal does not trigger a second synthetic timeout. Preserve each original event payload on the downstream wire. Do not rewrite `response.incomplete` into completed in the proxy.
  Parallelization: Wave 2 | Blocked by: 2, 3, 4 | Blocks: 8
  References: `internal/runtime/executor/codex_websockets_executor.go` (`runCodexWebsocketWarmup`, `ExecuteStream`); `internal/runtime/executor/codex_continue_scan.go`; `sdk/api/handlers/openai/openai_responses_websocket.go` (`forwardResponsesWebsocket`, `isResponsesWebsocketCompletionEvent`); corresponding test files.
  Acceptance criteria: every terminal closes one turn promptly; failure terminals are forwarded once and do not produce `stream closed before response.completed`; successful terminals still capture output/response ID/usage; literal duplicate terminal switches are removed in favor of the shared helper.
  QA scenarios: happy - run focused executor and handler terminal tests; failure - send `response.incomplete` and `response.failed` followed by an intentionally open upstream socket and prove the handler returns without waiting for idle timeout or emitting a second error. Record `.omo/evidence/task-5-adopt-pi-codex-transport.md`.
  Commit: Y | `fix(codex): unify websocket terminal handling`

- [x] 6. Make pre-output WebSocket retry and HTTP fallback explicit at `CodexAutoExecutor`
  What to do / Must NOT do: Begin with failing tests for four cases: connection-limit before any client-visible payload retries once on a fresh WebSocket; second pre-output transport failure falls back to `CodexExecutor.ExecuteStream`; generic dial/read close before output falls back to HTTP; any error after a non-empty downstream payload is forwarded without retry or fallback. Add package-private `bootstrapCodexStream(ctx, result)` to synchronously read until the first non-empty visible chunk, error, or close and to return a wrapped result that replays buffered chunks with original headers. Add `isCodexWebsocketTransportBootstrapError(err)` that returns true only for connection-limit, WebSocket close, EOF/read/network failure, handshake failure, and non-caller-cancellation deadline; it returns false for quota, context-length, invalid-request, policy, and other semantic API failures. Reset bridge state before full retry/fallback and disable WS only for the existing recovery window.
  Parallelization: Wave 2 | Blocked by: 3, 4 | Blocks: 8
  References: `internal/runtime/executor/codex_websockets_executor.go` (`CodexAutoExecutor.ExecuteStream`, `bridgedExecuteStream`, `disableWSSession`, `parseCodexWebsocketError`); `sdk/cliproxy/auth/conductor.go` (`readStreamBootstrap`) as the outer no-first-payload contract; Pi reference `openai-codex-responses.ts` pre-start retry/fallback behavior.
  Acceptance criteria: one fresh-WS retry maximum for connection limit; HTTP fallback only before output; no duplicate downstream bytes; bridge/session state is full-send safe after reconnect; non-transport failures are returned unchanged; the auth manager can still perform credential rotation outside this seam.
  QA scenarios: happy - `go test -run 'TestCodexAutoExecuteStream.*(ConnectionLimit|HTTPFallback|PostStart)' ./internal/runtime/executor`; failure - emit one text delta then a connection-limit error and prove the HTTP fake receives zero requests. Record `.omo/evidence/task-6-adopt-pi-codex-transport.md`.
  Commit: Y | `fix(codex): bound websocket fallback before output`

- [x] 7. Recover passthrough sessions with the existing full-transcript replay path
  What to do / Must NOT do: Add a failing downstream WebSocket handler test with a full first turn, an incremental second turn that receives `previous_response_not_found`, and a next request that must be rebuilt as a full transcript without the stale previous ID. Keep a shadow copy of request/response transcript state even while forwarding passthrough payloads unchanged. On a releasable cache/session error, force exactly the next request through existing mediated normalization/full replay, then allow passthrough again after success. Reuse tool-call repair and dedupe owners; do not create a second transcript builder.
  Parallelization: Wave 2 | Blocked by: 2 | Blocks: 8
  References: `sdk/api/handlers/openai/openai_responses_websocket.go` (`ResponsesWebsocket`, `forceTranscriptReplayNextRequest`, `normalizeResponsesWebsocketRequestWithIncrementalState`); `openai_responses_websocket_toolcall_repair.go`; `.omo/drafts/codex-transport-adoption-research.md` (`Conversation State and Storage Rules`).
  Acceptance criteria: passthrough happy path remains byte-preserving; cache loss releases pinned auth and schedules one full replay; replay includes prior user/assistant/tool state exactly once; stale `previous_response_id` is absent; successful recovery updates shadow state and can resume passthrough.
  QA scenarios: happy - run `go test -run 'TestResponsesWebsocket.*(Passthrough|Replay|PreviousResponseNotFound)' ./sdk/api/handlers/openai`; failure - include paired tool call/output and prove replay neither orphans nor duplicates them. Record `.omo/evidence/task-7-adopt-pi-codex-transport.md`.
  Commit: Y | `fix(responses): replay passthrough transcript after cache loss`

- [x] 8. Lock public Chat Completions and Responses contracts across internal transport selection
  What to do / Must NOT do: Add route/handler-level tests using existing fakes and `httptest` for Codex requests through `/v1/chat/completions` and `/v1/responses`. Cover HTTP/SSE downstream with internal upstream WS default, explicit chaining opt-out using HTTP, raw downstream Responses WebSocket passthrough, terminal failures, reasoning deltas, function/custom tool calls, usage, `[DONE]` behavior, and non-stream aggregation. Add a legacy `/v1/completions` smoke test only if shared code changed its path. Do not duplicate translator fixture suites or broaden translator changes.
  Parallelization: Wave 3 | Blocked by: 1-7 | Blocks: final verification
  References: `internal/api/server.go` route registration; `sdk/api/handlers/openai/openai_handlers.go`; `openai_responses_handlers.go`; `openai_chat_responses_stream_test.go`; `openai_responses_handlers_stream_test.go`; `openai_responses_handlers_stream_error_test.go`; existing Codex translator tests.
  Acceptance criteria: transport choice is invisible at both HTTP endpoints; response shapes and terminal framing match existing contracts; config opt-out proves HTTP remains available; no client-facing `response.append` is sent upstream; no new route or public schema is introduced.
  QA scenarios: happy - run focused handler tests for Chat Completions and Responses over internal WS; failure - force WS handshake/read failure and prove the downstream HTTP response is valid HTTP/SSE fallback with no duplicate event. Record `.omo/evidence/task-8-adopt-pi-codex-transport.md`.
  Commit: Y | `test(openai): lock codex transport route contracts`

## Final verification wave
> Runs in parallel after ALL todos. ALL must APPROVE. Surface results and wait for the user's explicit okay before declaring complete.
- [x] F1. Plan compliance audit - direct implementer checks every Must have/Must NOT have and todo acceptance item against the current diff; write `.omo/evidence/final-plan-compliance.md` with PASS/FAIL and exact evidence commands. No subagent.
- [x] F2. Code quality review - inspect changed code for duplicate terminal taxonomies, hidden caller conventions, boolean mode flags, retry-after-output, data races, leaked goroutines/channels, and unnecessary abstractions; run `gofmt` and targeted `go vet` if the repository baseline supports it; write `.omo/evidence/final-code-quality.md`. No subagent.
- [x] F3. Real manual QA - run targeted tests, race tests, package tests, `go build -o test-output ./cmd/server && rm test-output`, and `go test ./...`; verify a local `httptest`/integration path exercises WS success, connection-age rollover, connection-limit fallback, passthrough cache recovery, and post-output non-retry; write `.omo/evidence/final-manual-qa.md`. No production calls and no credentials.
- [x] F4. Scope fidelity - inspect `git diff`, confirm local `config.yaml`, credentials, model metadata, legacy completions, and unrelated fork features are untouched; run `gitnexus_detect_changes(scope="all")`; write `.omo/evidence/final-scope-fidelity.md`. No subagent.

## Commit strategy
- Before product implementation, commit only the approved research/draft/plan artifacts as the user-requested checkpoint; leave unrelated `.logs/`, `.omo/run-continuation/`, and `omp-session-SpecReview.html` untouched.
- During implementation, pair each behavior with its test in the same commit, following the commit listed on each todo.
- Keep config defaults, executor lifecycle, handler replay, and route-contract tests independently revertible.
- Before every implementation commit: inspect `git status`, staged diff, unstaged diff, and recent log; run the todo's focused tests and relevant package gate; run `gitnexus_detect_changes(scope="all")`.
- Never amend, force-push, skip hooks, or stage unrelated user files.

## Success criteria
- A configuration that omits `codex-response-chaining` uses internal upstream WS for websocket-capable Codex auths; explicit false uses HTTP.
- Downstream model metadata still reports `prefer_websockets: false`.
- Reused upstream sockets are bound to the selected auth and URL and are younger than 55 minutes.
- Recoverable redial cannot close the downstream WebSocket; terminal auth/session removal notifies exactly once.
- Completed/done/failed/incomplete/response.error/error events terminate one turn consistently without hangs or duplicate synthetic errors.
- `websocket_connection_limit_reached` gets at most one fresh WS retry before pre-output HTTP fallback.
- No transport/auth retry occurs after the first downstream payload.
- Passthrough `store=false` sessions recover from cache loss with one full, deduplicated transcript replay.
- `/v1/chat/completions` and `/v1/responses` preserve reasoning, tool, usage, terminal, SSE, and non-stream output contracts.
- Focused tests, race tests, package tests, build, and full `go test ./...` pass after the last code edit.
- GitNexus change detection and direct scope review show only planned modules and no config/credential/production changes.
- All implementation and verification were performed directly with zero subagents.

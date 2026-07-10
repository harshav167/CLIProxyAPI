---
slug: adopt-pi-codex-transport
status: approved
intent: clear
pending-action: commit the approved planning checkpoint before product implementation
approach: Deepen the existing CodexAutoExecutor path so websocket-capable Codex auths prefer a bounded, recoverable upstream WebSocket while preserving downstream HTTP/SSE compatibility and HTTP fallback.
---

# Draft: adopt-pi-codex-transport

## Components (topology ledger)
| id | outcome | status | evidence path |
| --- | --- | --- | --- |
| terminal-contract | Every Responses terminal event ends exactly one turn without hanging or synthetic double-errors | active | `internal/runtime/executor/helps/codex_websocket_warmup.go`, `internal/runtime/executor/codex_websockets_executor.go`, `sdk/api/handlers/openai/openai_responses_websocket.go` |
| connection-lifecycle | Reuse remains auth/URL-bound and proactively recycles before OpenAI's 60-minute limit | active | `internal/runtime/executor/codex_websockets_executor.go` |
| fallback-policy | WebSocket retry/HTTP fallback is allowed only before the first downstream payload | active | `internal/runtime/executor/codex_websockets_executor.go`, `sdk/cliproxy/auth/conductor.go` |
| replay-recovery | `store=false` sessions recover from connection/cache loss by replaying full local context | active | `internal/runtime/executor/codex_http_ws_bridge.go`, `sdk/api/handlers/openai/openai_responses_websocket.go` |
| internal-default | Websocket-capable Codex auths use internal upstream WS by default while clients retain HTTP/SSE preference | active | `internal/config/config.go`, `internal/config/parse.go`, `sdk/api/handlers/openai/codex_client_models.go` |
| route-contracts | Chat Completions and Responses keep their current output/tool/reasoning/usage contracts across transport choice | active | `sdk/api/handlers/openai/openai_handlers.go`, `sdk/api/handlers/openai/openai_responses_handlers.go` |

## Open assumptions (announced defaults)
| assumption | adopted default | rationale | reversible? |
| --- | --- | --- | --- |
| WebSocket rollout boundary | Change only proxy-to-OpenAI transport; keep `prefer_websockets: false` downstream | Avoid expanding the client-facing protocol surface while still gaining upstream robustness | yes |
| Default gate | `codex-response-chaining.enabled` defaults true when omitted; explicit false remains opt-out | The selected auth's websocket capability is the primary safety gate; a hidden second opt-in is a footgun | yes |
| Connection age | Recycle at 55 minutes | Leaves a five-minute margin before OpenAI's documented 60-minute connection cap | yes |
| Compatibility terminal | Continue accepting `response.done` as successful compatibility terminal | Existing proxy/Pi behavior supports it even though public docs emphasize `response.completed` | yes |
| Testing | TDD | User selected TDD; transport replay bugs need failing contract tests first | yes |
| Execution | No subagents; one direct implementer | Explicit user constraint | yes |

## Findings (cited - path:lines)
- OpenAI documents one in-flight response per WebSocket and a 60-minute connection limit; use repeated `response.create` with `previous_response_id`, not upstream `response.append`. See `.omo/drafts/codex-transport-adoption-research.md`.
- `CodexAutoExecutor.ExecuteStream` already owns HTTP, upstream WS, response chaining, delta/full retry, and fallback selection. A parallel Pi-style transport would duplicate the existing owner.
- `CodexWebsocketsExecutor.ensureUpstreamConn` already binds reuse to both `authID` and `wsURL`; Pi's session-ID-only socket cache would weaken proxy isolation.
- `HTTPToWSBridge.ComputeDelta` already resets on model, auth, request-shape, or prefix mismatch and has focused concurrency tests.
- Terminal semantics are duplicated and inconsistent: warmup recognizes completed/done/failed/incomplete/error, the executor's downstream raw branch omits failed/incomplete/response.error, and the handler completion helper only recognizes completed/done.
- `codexWebsocketSession` has idle/session TTLs but no absolute connection age, so an active conversation can reach the upstream 60-minute cap.
- `invalidateUpstreamConn` always notifies `upstreamDisconnectCh`, including send-error paths that immediately redial; `ResponsesWebsocket` closes the downstream socket on that notification, creating a recoverable-retry race.
- `forceTranscriptReplayNextRequest` restores mediated replay state but is effectively a no-op for upstream passthrough sessions, so `previous_response_not_found` can repeat stale incremental state.
- `auth.Manager.readStreamBootstrap` already owns the generic pre-first-payload boundary. New Codex logic must compose with it, not create an independent global retry loop.
- Handler-level coverage exists for framing and WebSocket forwarding, but direct route-contract coverage for Codex branching through `/v1/chat/completions` and `/v1/responses` is thin.

## Decisions (with rationale)
- Keep `CodexAutoExecutor` as the only transport selector and deepen its guarantees.
- Generalize the existing Codex warmup event classifier into the shared terminal-event owner used by warmup, executor, and downstream forwarding; remove local terminal lists.
- Split recoverable connection recycle from terminal session notification with explicit method names; do not add a boolean `notify` parameter.
- Store connection creation time on `codexWebsocketSession`; recycle stale connections inside `ensureUpstreamConn` before reuse so every natural caller gets the invariant.
- Preserve generic auth-manager bootstrap retry. Codex-specific fallback may select HTTP only when no downstream payload has been emitted; after output begins, forward the terminal error without replay.
- Keep shadow transcript/response state for passthrough turns and reuse the existing full-transcript normalization path on cache loss.
- Set the response-chaining default in both config parsing paths while retaining explicit `enabled: false`.
- Do not change model metadata to advertise downstream WebSockets.
- Execute implementation directly with no subagents, as requested.

## Scope IN
- Codex upstream HTTP/SSE/WebSocket selection and lifecycle.
- Responses terminal-event classification shared across relevant owners.
- Upstream WebSocket auth/URL/age affinity and recovery behavior.
- `store=false` full-context recovery after reconnect or cache miss.
- Config defaulting for internal response chaining.
- Focused executor, handler, config, and route contract tests.
- Existing request logs/telemetry labels needed to prove transport choice and recovery.

## Scope OUT (Must NOT have)
- No literal port of Pi's TypeScript classes, global session cache, or debug-stat APIs.
- No new dependency, new transport package, or parallel executor hierarchy.
- No changes to local `config.yaml` or credential files.
- No downstream `prefer_websockets: true` rollout.
- No rewrite of legacy `/v1/completions` beyond a regression smoke check if touched indirectly.
- No broad translator refactor or unrelated cleanup.
- No retries after any downstream payload has been emitted.
- No subagents during implementation or review.

## Open questions
None. The user approved the internal-upstream-WS boundary and selected TDD.

## Approval gate
status: approved
approved decision: write `.omo/plans/adopt-pi-codex-transport.md` with internal upstream WS default, TDD, and no subagent execution.

# Codex Responses Transport Adoption Research

Last updated: 2026-07-10

This note summarizes first-party evidence for adopting OpenAI/Codex Responses HTTP, SSE, and WebSocket transports in CLIProxyAPI. Evidence is labeled as public contract when it comes from OpenAI docs, and as first-party client detail when it comes from OpenAI SDK or Codex source.

## Executive Summary

The stable baseline remains HTTP `POST /v1/responses`: the API reference defines `POST /responses` for creating a response, and the streaming guide says HTTP streaming is enabled by setting `stream=true` on the Responses endpoint ([Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create/), [Streaming guide](https://platform.openai.com/docs/guides/streaming-responses)).

The documented WebSocket mode is a persistent connection to `wss://api.openai.com/v1/responses`; each turn starts by sending a client `response.create` event, and continuation is done by sending another `response.create` with only new `input` items plus `previous_response_id` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Do not design around `response.append` for this adoption: the public WebSocket guide documents repeated `response.create`, the OpenAI Node example sends `ResponsesClientEvent` with `type: 'response.create'`, and the current first-party Codex `ResponsesWsRequest` enum contains only `response.create` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L413), [Codex common.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs)).

WebSocket mode is documented as compatible with Zero Data Retention and `store=false` because the fast continuation path keeps only the most recent previous-response state in a connection-local in-memory cache; with `store=false`, there is no persisted fallback if the prior response ID is not in that cache ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

The public WebSocket guide documents one in-flight response per connection, no multiplexing, existing Responses streaming event ordering, and a 60 minute connection duration limit ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

The `OpenAI-Beta: responses_websockets=2026-02-06` header is a first-party client/source detail, not a clearly stated public-doc requirement in the WebSocket guide: OpenAI's Node example exposes that beta value, and Codex source defines the same value as `RESPONSES_WEBSOCKETS_V2_BETA_HEADER_VALUE` ([openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L89), [Codex client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs)).

## Transport Surfaces

### HTTP Responses Create

Public contract: create a response with `POST /v1/responses`, using the normal Responses create body ([Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create/)).

Public streaming contract: request HTTP SSE streaming by setting `stream=true`; the streaming guide says Responses streams use semantic typed events such as `response.created`, `response.output_text.delta`, `response.completed`, and `error` ([Streaming guide](https://platform.openai.com/docs/guides/streaming-responses)).

Public conversation-state contract: a client can manually carry history by resending prior input/output items, use Conversations API persistence, or chain responses by passing `previous_response_id` ([Conversation state](https://platform.openai.com/docs/guides/conversation-state)).

Public stateful chaining contract: the conversation-state guide shows `previous_response_id` only when the original and follow-up responses use `store: true` in the example, while the WebSocket guide documents `store=false` compatibility specifically through the active socket's in-memory cache ([Conversation state](https://platform.openai.com/docs/guides/conversation-state), [WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

### HTTP Background Mode

Public contract: background mode starts an asynchronous response with `background: true` and polling uses `GET /v1/responses/{id}` until the response status leaves `queued` or `in_progress` ([Background mode](https://platform.openai.com/docs/guides/background)).

Public storage caveat: background mode stores response data for roughly 10 minutes to enable polling and is not ZDR compatible ([Background mode](https://platform.openai.com/docs/guides/background)).

Adoption implication: do not use background mode as the fallback for ZDR or `store=false` WebSocket continuation; use full-context replay or compacted-window replay when the cached `previous_response_id` cannot be continued ([Background mode](https://platform.openai.com/docs/guides/background), [WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

### WebSocket Responses

Public contract: connect to `wss://api.openai.com/v1/responses` and send JSON client events over the socket ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public client event contract: begin each turn with a `response.create` event whose payload mirrors the normal Responses create body, except transport-specific fields such as `stream` and `background` are not used ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

First-party client detail: OpenAI's Node example still includes `stream: true` in the `ResponsesClientEvent`, and Codex's `ResponseCreateWsRequest` struct contains a `stream` field, so CLIProxyAPI should tolerate or forward `stream` for compatibility but should not depend on it as a WebSocket-mode contract ([openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L413), [Codex common.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs), [WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public continuation contract: continue by sending another `response.create` with `previous_response_id` set to the prior response ID and `input` containing only new items such as tool outputs or the next user message ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public warmup contract: a client can send `response.create` with `generate: false`, receive a response ID, and chain the next generated turn with `previous_response_id` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

First-party Codex detail: Codex uses WebSocket prewarm as a best-effort `response.create` with `generate=false` and lets normal stream retry or fallback handle prewarm failure ([Codex client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs)).

Public concurrency contract: a single WebSocket connection can receive multiple `response.create` messages but runs them sequentially with one in-flight response at a time; the public guide says there is no multiplexing support today ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public lifetime contract: connection duration is limited to 60 minutes and clients should reconnect when the limit is reached ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

First-party Codex detail: Codex maps the `websocket_connection_limit_reached` error code to a retryable API error with the message `Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue.` ([Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

## Event Semantics

Public event model: Responses streaming uses typed semantic events, and the streaming guide lists lifecycle and output events including `ResponseCreatedEvent`, `ResponseInProgressEvent`, `ResponseFailedEvent`, `ResponseCompletedEvent`, output item events, text delta events, tool-call argument events, and `Error` ([Streaming guide](https://platform.openai.com/docs/guides/streaming-responses), [Streaming events reference](https://developers.openai.com/api/reference/resources/responses/streaming-events/)).

Public WebSocket event-ordering contract: WebSocket server events and ordering match the existing Responses streaming event model ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Recommended success terminal: treat `response.completed` as the only successful terminal event for streaming transports, because Codex's SSE and WebSocket processors stop the response stream only after mapping `response.completed` to `ResponseEvent::Completed` ([Codex SSE responses.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs), [Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

Recommended failure terminals: treat `response.failed`, `response.incomplete`, and top-level `error` as terminal failures; Codex maps `response.failed` into classified API errors, maps `response.incomplete` to a stream error, and maps wrapped WebSocket `error` payloads with non-success status into transport errors ([Codex SSE responses.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs), [Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

Recommended connection-loss rule: if an SSE stream or WebSocket closes before `response.completed`, treat the turn as not successfully completed; Codex's SSE processor emits `stream closed before response.completed`, and its WebSocket processor returns `websocket closed by server before response.completed` or `stream closed before response.completed` ([Codex SSE responses.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs), [Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

## Conversation State and Storage Rules

Public WebSocket state rule: the active WebSocket connection keeps one previous-response state, the most recent response, in a connection-local in-memory cache ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public storage rule: because the WebSocket previous-response state is retained only in memory and is not written to disk, WebSocket mode can be used with `store=false` and ZDR ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public cache-miss rule: with `store=true`, the service may hydrate older response IDs from persisted state when available, but that usually loses the in-memory latency benefit ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public ZDR cache-miss rule: with `store=false`, including ZDR, there is no persisted fallback; if the requested previous response ID is uncached, the request returns `previous_response_not_found` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public error rule: if a turn fails with a `4xx` or `5xx`, the service evicts the referenced `previous_response_id` from the connection-local cache to prevent reusing stale cached state ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Public compaction rule: server-side compaction via `context_management` continues the normal WebSocket pattern with the latest `previous_response_id`; standalone `/responses/compact` returns a compacted input window rather than a response ID, so the next WebSocket request starts a new chain by omitting or nulling `previous_response_id` and passing the compacted output as input ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

## Beta Headers

Public-doc state: the WebSocket mode guide shows a WebSocket connection authenticated with `Authorization: Bearer ...` and does not make the beta header part of the visible example ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

First-party SDK detail: the OpenAI Node WebSocket example defines `BETA_HEADER_VALUE = 'responses_websockets=2026-02-06'` and conditionally sends it as `OpenAI-Beta` when `--use-beta-header` is provided ([openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L89), [openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L413)).

First-party Codex detail: Codex source defines `OPENAI_BETA_HEADER` and `RESPONSES_WEBSOCKETS_V2_BETA_HEADER_VALUE = "responses_websockets=2026-02-06"` ([Codex client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs)).

Adoption recommendation: make `OpenAI-Beta: responses_websockets=2026-02-06` configurable and enabled for first-party-compatible WebSocket mode, but isolate it as a feature-gated client-compatibility header rather than a hard-coded public API invariant ([openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L89), [Codex client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs), [WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

## `response.append` Assessment

No first-party evidence reviewed here establishes `response.append` as a public or SDK client event.

The public WebSocket continuation flow uses another `response.create`, not `response.append` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

The OpenAI Node example creates a `ResponsesClientEvent` whose type is `response.create` for each generated request, including follow-up turns with `previous_response_id` ([openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L413)).

The OpenAI Node generated type describes `ResponsesClientEvent` as having type `response.create`, and the current Codex `ResponsesWsRequest` enum serializes only `ResponseCreate` as `response.create` ([openai-node responses.ts](https://github.com/openai/openai-node/blob/main/src/resources/responses/responses.ts#L6675), [Codex common.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs)).

Adoption recommendation: treat any `response.append` mention as undocumented until a first-party public reference or SDK type appears; implement continuation using `response.create` plus `previous_response_id` and incremental `input` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

## Fallback-Safe Retry Boundaries

Safe pre-send retry: if WebSocket connection establishment fails before a request frame is accepted by the socket, retry by opening a new WebSocket connection or falling back to HTTP/SSE with the same logical request, because no server-side response state should have been created before the request frame is sent; Codex treats WebSocket prewarm failure as a best-effort failure handled by normal stream retry or fallback logic ([Codex client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs)).

Safe connection-limit recovery: when `websocket_connection_limit_reached` occurs, open a new WebSocket connection; if the prior response is persisted with `store=true`, continue with `previous_response_id`, and if it is not continuable because of `store=false` or `previous_response_not_found`, start a new response with full input context or a compacted window ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

Safe cache-miss recovery: on `previous_response_not_found`, do not retry the same incremental request against the same missing previous ID; reconstruct the full context, use a compacted window, or continue from a persisted response ID that is known to exist ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Unsafe replay boundary: after a request frame has been sent and any server event may have been produced, do not blindly replay the same incremental `input` as a new generation unless the client can prove the prior attempt failed before creation; otherwise the replay can duplicate tool outputs or user turns because WebSocket continuation is stateful through `previous_response_id` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [Streaming guide](https://platform.openai.com/docs/guides/streaming-responses)).

Fatal-not-retry-as-same-request boundary: context-window errors, insufficient quota, usage-not-included, cyber-policy, invalid prompt, and similar classified failures should not be retried as identical requests; Codex maps those `response.failed` payloads to fatal or invalid-request classifications rather than generic retry loops ([Codex SSE responses.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs)).

Retryable throttling boundary: rate-limit style failures can be retried after the parsed delay when the upstream error supplies one; Codex parses `rate_limit_exceeded` messages for retry-after timing and maps other nonfatal failed responses to retryable errors ([Codex SSE responses.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs)).

## CLIProxyAPI Adoption Shape

Implement WebSocket as an alternate upstream transport for Codex Responses, not as a new protocol surface for downstream clients, because the public WebSocket contract mirrors Responses create semantics and server events match Responses streaming events ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [Streaming guide](https://platform.openai.com/docs/guides/streaming-responses)).

Preserve the HTTP/SSE path as the fallback transport because the stable documented create endpoint is `POST /v1/responses` and the documented streaming mechanism is HTTP SSE with `stream=true` ([Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create/), [Streaming guide](https://platform.openai.com/docs/guides/streaming-responses)).

Represent each upstream WebSocket turn as `response.create`, carry `previous_response_id` only when continuing from a known prior response, and send only new input items on fast-path continuation ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Keep one active in-flight response per upstream WebSocket connection and allocate separate connections for parallel upstream runs, because the public guide says there is no multiplexing and one connection runs responses sequentially ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Track connection age and reconnect before or at 60 minutes, because the public guide documents a 60 minute connection duration limit and Codex treats the corresponding limit error as retryable ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

For ZDR or `store=false`, retain enough local transcript state to reconstruct a full-context request after reconnect or cache miss, because the public guide says there is no persisted fallback for uncached previous response IDs under `store=false` ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

For stored mode, allow `previous_response_id` recovery across a new connection but expect the latency benefit to drop because the public guide says persisted hydration may work and usually loses the in-memory latency benefit ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

Normalize terminal handling across SSE and WebSocket: success only on `response.completed`; failure on `response.failed`, `response.incomplete`, top-level `error`, idle timeout, or transport close before completion ([Streaming guide](https://platform.openai.com/docs/guides/streaming-responses), [Codex SSE responses.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs), [Codex responses_websocket.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs)).

Do not expose or depend on undocumented `response.append`; keep the implementation aligned with repeated `response.create` plus `previous_response_id` until first-party docs or SDK types say otherwise ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [openai-node responses.ts](https://github.com/openai/openai-node/blob/main/src/resources/responses/responses.ts#L6675), [Codex common.rs](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs)).

## Open Questions

The public WebSocket guide documents a 60 minute connection duration limit but does not publish a smaller numeric TTL for the connection-local previous-response cache in the material reviewed here ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

The public WebSocket guide documents no multiplexing and one in-flight response per connection, but the material reviewed here does not publish account-level or process-level maximum WebSocket connection counts ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode)).

The beta header value is present in first-party client source, but the public WebSocket guide's visible example does not present it as required, so deployment should keep this behind config or provider capability detection ([WebSocket mode](https://platform.openai.com/docs/guides/websocket-mode), [openai-node websocket example](https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts#L89), [Codex client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs)).

## Planning Gate

- Status: approved
- Pending action: execute `.omo/plans/adopt-pi-codex-transport.md` directly without subagents
- Test strategy: TDD
- Downstream boundary: retain existing HTTP/SSE preference (`prefer_websockets: false`); improve proxy-to-OpenAI WebSocket selection internally.
- Primary seam: deepen `CodexAutoExecutor.ExecuteStream`; do not add a parallel transport stack.
- Components: terminal-event semantics; connection age and affinity; safe pre-output fallback; internal WebSocket defaulting; route-level contract tests.
- Must not include: literal Pi cache port, new dependencies, legacy `/v1/completions` rewrite, downstream WebSocket rollout, or changes to local `config.yaml`.

## Source Index

- OpenAI Responses create reference: https://developers.openai.com/api/reference/resources/responses/methods/create/
- OpenAI Responses streaming events reference: https://developers.openai.com/api/reference/resources/responses/streaming-events/
- OpenAI WebSocket mode guide: https://platform.openai.com/docs/guides/websocket-mode
- OpenAI conversation state guide: https://platform.openai.com/docs/guides/conversation-state
- OpenAI streaming guide: https://platform.openai.com/docs/guides/streaming-responses
- OpenAI background mode guide: https://platform.openai.com/docs/guides/background
- OpenAI Node WebSocket example: https://github.com/openai/openai-node/blob/main/examples/responses/websocket.ts
- OpenAI Node `ResponsesWS`: https://github.com/openai/openai-node/blob/main/src/resources/responses/ws.ts
- OpenAI Node generated Responses types: https://github.com/openai/openai-node/blob/main/src/resources/responses/responses.ts
- OpenAI Codex client transport source: https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs
- OpenAI Codex Responses common types: https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs
- OpenAI Codex WebSocket endpoint handling: https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/responses_websocket.rs
- OpenAI Codex SSE event handling: https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/sse/responses.rs

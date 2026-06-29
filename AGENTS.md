# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
- GitHub: https://github.com/router-for-me/CLIProxyAPI

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Cursor Request Shape

- Cursor does **not** send Anthropic Messages format to the proxy on ingress, even when the user picked a native Claude/Fable model in Cursor. Cursor sends an **OpenAI chat-completions style** request body to cliproxy, and cliproxy's Claude path translates it later.
- If you need the exact raw Cursor `system` prompt or native tool array, inspect the proxy's **request/response logs**, not ProxyMan captures from Claude Code:
  - local container: `docker/logs/cliproxy/`
  - prod VM: `~/cliproxy/logs/`
- For prompt-shape questions, `=== REQUEST BODY ===` is the raw Cursor payload and `=== API REQUEST ===` is what cliproxy finally sent upstream.

## Production Observability — Use SigNoz First (MANDATORY)

The prod proxy ships full traces/metrics/logs to SigNoz at `kaecilius.ecorp.cc`. For ANY question about prod behaviour — errors, latency, throughput, which auth was selected, what upstream returned, why a request failed — query SigNoz first. The MCP server is `project-2-CLIProxyAPI-signoz` (tools: `signoz_search_logs`, `signoz_search_traces`, `signoz_aggregate_logs`, `signoz_aggregate_traces`, `signoz_get_trace_details`, `signoz_query_metrics`, `signoz_get_field_values`, `signoz_list_metrics`).

DO NOT default to `gcloud compute ssh` for log inspection. SSH is the fallback only when SigNoz is genuinely unreachable AND the user asked for SSH explicitly. If SigNoz returns a 502 / non-200, the FIRST action is to diagnose and fix the SigNoz pipeline — not to silently sidestep into SSH. Tell the user SigNoz is down, fix it, then resume the original investigation.

### SigNoz infrastructure map (verified 2026-06-13)

This is the actual path a query takes. Memorise it; do not guess at it.

```
Cursor MCP / agent
       │   (calls `project-2-CLIProxyAPI-signoz`)
       ▼
http://127.0.0.1:57000  ── signoz-mcp-server container on local Mac
       │   (HTTP/JSON over MCP)
       ▼
https://kaecilius.ecorp.cc  ── Cloudflare ingress
       │   (Cloudflare Tunnel)
       ▼
cloudflared paper-tunnel (UUID d4683d61-5597-4985-b71f-372f2679c293)
       │   - macOS LaunchAgent: cc.ecorp.cloudflared-paper
       │   - plist: ~/Library/LaunchAgents/cc.ecorp.cloudflared-paper.plist
       │   - config: ~/.cloudflared/config.yml
       │   - routes for kaecilius.ecorp.cc are:
       │       path ^/v1/(traces|logs|metrics)$ → http://192.168.1.202:57018  (OTLP/HTTP ingest)
       │       all other paths                  → http://192.168.1.202:57080  (SigNoz UI / API)
       ▼
host 192.168.1.202 on LAN ── runs the SigNoz docker-compose stack
       │   - root@192.168.1.202, ssh key auth (no password)
       │   - hostname: NOT "ava" — `ava` is 192.168.1.201 which runs the
       │     macro / clairvoyant / s3.macro / opik / codex LXCs and DOES NOT
       │     run SigNoz. Confusing the two was the root cause of the
       │     2026-06-13 outage where kaecilius returned 502 for hours.
       │   - SigNoz UI:        http://192.168.1.202:57080
       │   - OTLP/HTTP ingest: http://192.168.1.202:57018
       │   - OTLP/gRPC ingest: 192.168.1.202:57017 (NOT tunnelled — LAN only)
       │   - collector health: http://192.168.1.202:57133
       ▼
ClickHouse + query-service inside SigNoz stack
```

The prod proxy on the GCP VM also runs its OWN otel-collector (via `docker/docker-compose.local.yml` style stack) and exports through this same tunnel. Sources of telemetry on SigNoz:
- `service.namespace = "cliproxy"` / `cliproxy.instance = "clanker"` → GCP prod proxy
- `cliproxy.instance = "local-docker"` → developer Mac local container
Both push through `https://kaecilius.ecorp.cc/v1/{traces,logs,metrics}`.

### Known SigNoz failure modes + fix-first checklist

1. **cloudflared paper-tunnel is dead on the Mac.** Symptom: `curl https://kaecilius.ecorp.cc` returns 502 with a sub-second `time_total`. Fix: `launchctl kickstart -k gui/$(id -u)/cc.ecorp.cloudflared-paper`, wait 5s, retry. The plist owns the lifecycle; do NOT `kill`-and-restart by hand.
2. **cloudflared config points at the wrong host IP.** Historic cause of outages: someone migrates SigNoz off the Mac to a LAN host and forgets to update `~/.cloudflared/config.yml`. Verify against the map above. Both `kaecilius.ecorp.cc` ingress rules must point at the host that actually runs SigNoz (currently `192.168.1.202`).
3. **192.168.1.202 is up on LAN but SigNoz container is down.** Symptom: `curl http://192.168.1.202:57080` returns connection refused or 5xx, but ping works. Fix: `ssh root@192.168.1.202`, find the SigNoz `docker-compose` stack, `docker compose up -d`. The compose file location on .202 is not yet inventoried here — once located, write the absolute path into this section.
4. **signoz-mcp-server container on the Mac is stale.** Symptom: SigNoz UI loads in browser but MCP tools return 502 / empty. Fix: `docker restart signoz-mcp-server`. The MCP container is a thin HTTP proxy that talks to `kaecilius.ecorp.cc` — if the tunnel is down, MCP returns the same upstream error.
5. **GCP prod proxy stopped exporting.** Symptom: SigNoz UI works locally but the `cliproxy.instance = "clanker"` series stops. The prod proxy's compose stack on the GCP VM owns its own otel-collector. SSH into the prod VM (`gcloud compute ssh --zone "asia-southeast1-a" wade@production --project "mediprepai"`) and check `docker ps`. This is the ONE case where SSH is legitimate — not for reading logs but for fixing the collector that produces them.

If the user ever has to re-explain that SigNoz is the source of truth, that is a process failure. Update this section the moment any infrastructure detail changes.

## Cursor rendering contract: `delta.reasoning_content` = visible thinking (verified 2026-06-27)

Cursor renders an OpenAI chat-completions stream's **`choices[].delta.reasoning_content`** as the native thinking block. This is THE field. Confirmed on the wire from two independent GLM backends (z.ai coding endpoint: 258 deltas; GMI general endpoint: 518 deltas) — both stream `{"choices":[{"delta":{"reasoning_content":"..."}}]}` and Cursor shows thinking for both. The visible answer then arrives as the normal `delta.content`. No request-side `thinking`/`reasoning_effort` field is required for Cursor to render it — the model/provider emits `reasoning_content` and Cursor picks it up.

- Our chat-completions translators ALREADY emit this correctly:
  - `internal/translator/claude/openai/chat-completions/claude_openai_response.go` — `thinking_delta` → `choices.0.delta.reasoning_content` (streaming) and `choices.0.message.reasoning_content` (non-stream).
  - `internal/translator/codex/openai/chat-completions/codex_openai_response.go` — `response.reasoning_summary_text.delta` → `choices.0.delta.reasoning_content`.
- Opus/GPT thinking visibility caveat: the Opus prod aliases use `thinking.type: adaptive` + `output_config.effort` (the model DECIDES whether to think). An easy turn (e.g. a single tool call) can legitimately return `output_tokens_details.thinking_tokens: 0` and therefore no `reasoning_content` — that is NOT a bug, it's adaptive thinking declining to think. To force thinking every turn, switch the override to `thinking.type: enabled` + `thinking.budget_tokens` (manual) — at higher token cost.

### Image/vision through Cursor (verified 2026-06-27)
- Cursor pre-processes images itself (needs a real OpenAI API key configured) and DOES send `image_url` + `data:image` base64 in the downstream chat-completions body. Proven on prod: downstream had the JPEG, upstream to z.ai had the identical bytes, our proxy passes it through untouched (`NormalizeGLMRequestBody` never touches image content).
- Whether a given model "sees" the image depends on the BACKEND deployment, not our proxy: z.ai's coding-plan GLM-5.2 accepts the image (200, ~48K prompt tokens) but the model replies "I can't see it" (text-only deployment); GMI's `zai-org/GLM-5.2-FP8` build is multimodal and describes it. z.ai's real vision model is `glm-5v-turbo` on the GENERAL endpoint (`/api/paas/v4`), not the coding endpoint.

## GLM / Z.AI provider config (prod `openai-compatibility`)

Prod `glm` provider uses the GLM Coding Plan endpoint `https://api.z.ai/api/coding/paas/v4` (subscription; only GLM-5.2 / GLM-5-Turbo / GLM-4.7 callable, no vision). The `gmi` provider (`https://api.gmi-serving.com/v1`, general pay-as-you-go) is a throwaway promo endpoint.

GLM effort aliases use `protocol: openai` payload overrides (matched on the client-facing alias name). Pattern (added 2026-06-27, verified on the wire):

```yaml
# under openai-compatibility: glm: models:
  - name: GLM-5.2
    alias: "glm-5.2-max"     # one model entry per alias
  - name: GLM-5.2
    alias: "glm-5.2-high"
# under override:
  - models: [{ name: glm-5.2-max, protocol: openai, ... }]
    params: { stream: true, thinking.type: enabled, reasoning_effort: max,  clear_thinking: false, tool_stream: true }
  - models: [{ name: glm-5.2-high, protocol: openai, ... }]
    params: { stream: true, thinking.type: enabled, reasoning_effort: high, clear_thinking: false, tool_stream: true }
```

Per z.ai spec: `reasoning_effort` only takes effect when `thinking.type: enabled`; `low`/`medium`→`high`, `xhigh`→`max`, `none`/`minimal` skip thinking. `clear_thinking: false` preserves prior-turn `reasoning_content` across turns. `tool_stream: true` streams tool_call deltas (GLM-4.6+). Always back up the hand-edited prod config (`config.yaml.bak-*`) before editing; restart `cli-proxy-api-test` to load.

## Fork features to preserve across upstream merges

This fork (`harshav167/CLIProxyAPI`) adds behaviour the upstream (`router-for-me/CLIProxyAPI`) does not have. Every upstream sync MUST keep these intact. List updated 2026-06-13; refresh whenever a fork-only feature is added.

### Cursor system-prompt rewrite (Claude path)
- `internal/runtime/executor/helps/cursor_system_prompt.go` — identity-line + integrity-contract rewrite shared by GPT-5.4 / Codex / Opus / Fable. Touch carefully; the integrity contract has been redlined by the user.
- `internal/runtime/executor/helps/claude_cursor_system_prompt.go` — Cursor → Claude system-block rebuild: billing header + identity block + cache_control anchoring. Mirrors the canonical Claude Code 2.1.156 Opus 4.8 layout (verified 2026-05-29).
- `internal/runtime/executor/helps/cursor_system_prompt_test.go` / `claude_cursor_system_prompt_test.go` — assertions for the canonical layout. Update tests when prompt redline changes.

### xAI / Grok Composer fixes (NOT upstream)
- `internal/runtime/executor/xai_executor.go::sanitizeXAIResponsesBody` runs three normalisers in order:
  1. `rewriteOrphanXAICustomToolCalls` — fixes 422 "untagged enum ModelInput" when `apply_patch` (or any other custom tool) was stripped from `tools` but its `custom_tool_call` / `custom_tool_call_output` history items remain. Rewrites them to `function_call` / `function_call_output` with arguments wrapped as `{"input": "..."}` JSON-object strings.
  2. `normalizeXAIFunctionCallArguments` — fixes 400 "expected JSON object for tool arguments" when prior turn's interrupted streaming left `function_call.arguments` as `""` or non-JSON. Coerces missing / empty / non-object args to `"{}"`.
  3. `stampXAIInputMessageType` — adds `type: "message"` to bare role-bearing items that droid emits without a type (xAI's `ModelInput` enum rejects untyped variants; OpenAI tolerates them).
- `internal/runtime/executor/xai_executor.go::normalizeXAITool` — drops `apply_patch` from `tools` entirely (xAI refuses to register it). Pairs with the orphan rewrite above.
- `internal/runtime/executor/xai_executor_test.go` — covers all three normalisers with the actual prod failure repros.

### Cursor Fable 5 alias (`f5-*` family, bypasses Cursor ZDR routing gate)
- `internal/runtime/executor/helps/cursor_fable_alias.go` — `ApplyCursorFableAliasSnapshot` swaps `system` and `tools` transactionally when inbound model matches `f5-*`. Both `sjson` writes succeed-or-rollback; if either fails the original payload is returned and the failure is logged.
- `internal/runtime/executor/helps/cursor_fable_snapshot/` — embedded Go asset (`//go:embed`) containing the captured Cursor → Anthropic Opus 4.7 thinking-max system blocks (identity swapped to "Claude Fable 5") + the 19 native Cursor tools (`Shell`, `Read`, `Grep`, `StrReplace`, `Task`, `TodoWrite`, …). DO NOT replace this with a live capture; the embedded version is the contract.
- `internal/runtime/executor/claude_executor.go` — hook point: the line immediately after `thinking.ApplyThinking` and before `applyCloaking`. Calls `helps.ApplyCursorFableAliasSnapshot(body, req.Model)`. Hook MUST run before `applyCloaking` so the rest of the Cursor pipeline sees the swapped body.
- `docker/config/cliproxy-config.yaml` — five `overrides:` (each `protocol: claude`) and five `oauth-model-alias: claude:` entries map `f5-low` / `f5-medium` / `f5-high` / `f5-xhigh` / `f5-max` to `claude-fable-5` with the matching effort level. Same config is mirrored on the prod VM at `/home/wade/cliproxy/config/config.yaml` (back up before editing — file is hand-edited in place).
- If a user asks whether `f5-*` is sending the "right" prompt/tools, verify in the request logs above. The expected behavior is: inbound model `f5-*` → snapshot injects Cursor-native Anthropic-model `system` + 19 native tools → normal Claude rewrite pipeline continues.

### Codex workspace / billing reality
- ChatGPT web workspaces and OpenAI API/Codex orgs are **different systems**. The UUID shown in `chatgpt.com` workspace pickers is **not** the same identifier as the `org-...` value on `platform.openai.com`.
- If Codex OAuth returns `chatgpt_plan_type: "free"` and `https://platform.openai.com/settings/organization/billing/overview` shows **Free trial** / **Add payment details**, there is **no proxy-side fix**. The OpenAI API org itself lacks paid Codex/API billing.
- Do **not** hardcode a ChatGPT workspace UUID into cliproxy config to try to force a business workspace. We tested that path and it did not change the minted API token.
- To diagnose Codex "why am I still free-tier?" issues, check:
  1. `https://platform.openai.com/settings/organization/general` for the real API org id (`org-...`)
  2. `https://platform.openai.com/settings/organization/billing/overview` for actual API/Codex billing state
  3. the stored auth JSON / decoded `id_token` claims for `chatgpt_account_id`, `chatgpt_plan_type`, and `organizations[]`
- If the API org is free, the correct fix is on OpenAI's side (billing / org enrollment), not in cliproxy.

### Observability (deeper than upstream)
- `internal/observability/` — fork-customised: `transport_logs.go` records error-only request bodies when enabled, `metrics.go` exposes the proxy-level metrics enumerated in `docs/signoz-observability.md`, `config.go` adds `TransportLogsErrorBody` + `TransportLogsBodyLimit` settings.
- Quota metrics (`cliproxy.quota.utilization` gauge + `cliproxy.quota.burned` counter) parse provider-specific headers (Anthropic `Anthropic-Ratelimit-Unified-*`, Codex `x-codex-primary-used-percent`) and feed the cash-burn dashboard panels.
- `internal/runtime/executor/claude_executor.go` / `codex_executor.go` / `codex_websockets_executor.go` — each calls `reporter.RecordQuota(ctx, headers)` after `RecordAPIResponseMetadata` on every response path.

### Billing / cache behaviour
- `internal/config/config.go` — `ClaudeCursorGlobalCacheScope` config flag (feature-gates `scope:"global"` on the last system block, defaults on in prod via `cliproxy-config.yaml`).
- `internal/runtime/executor/helps/claude_cache_control.go` — `EnsureClaudeCacheControl`, `EnsureClaudeUserPromptCacheAnchor`, `CountClaudeCacheControls`. Anchor strategy mirrors Claude Code 2.1.156 Opus 4.8.

### GLM / Z.AI request normalisation (NOT upstream)
- `internal/runtime/executor/helps/glm_normalizer.go::NormalizeGLMRequestBody` runs on the openai-compat path (both `Execute` and `ExecuteStream` in `openai_compat_executor.go`, right after `ApplyPayloadConfigWithRequest`), gated on `provider == "glm"`. Idempotent. Five behaviours:
  1. Couples `reasoning_effort` with `thinking.type=enabled` (GLM ignores effort without it) — EXCEPT `none`/`minimal`, which mean skip-thinking and must NOT enable thinking (`glmEffortEnablesThinking`).
  2. Maps effort aliases (`low`/`medium`→`high`, `xhigh`→`max`) per z.ai spec.
  3. Strips OpenAI-only top-level fields (`service_tier`, `parallel_tool_calls`, `prompt_cache_key`, `prompt_cache_retention`, `store`, `metadata`, `logprobs`, `top_logprobs`). These vary per turn and break z.ai's **implicit prefix-based prompt cache**.
  4. Enables `tool_stream: true` for GLM-4.6+ when tools present (per-chunk tool_call deltas; default false buffers them to stream end).
  5. Sorts `tools[]` by `function.name` for a byte-stable cacheable prefix (Cursor doesn't guarantee tool order across turns). Bails on non-function tools (web_search/retrieval) to avoid partial sort.
- z.ai prompt cache is IMPLICIT (no `cache_control` markers, no `prompt_cache_key`). Hit reporting via `usage.prompt_tokens_details.cached_tokens` — already parsed by `helps.ParseOpenAIUsage`, flows to SigNoz unchanged. Verified in prod 2026-06-21: warm turn hit 64/96 prompt tokens.
- `internal/runtime/executor/helps/glm_normalizer_test.go` — 13 tests incl. the `none`/`minimal` skip-thinking guard and idempotency.
- Prod config (`/home/wade/cliproxy/config/config.yaml`, lines ~730) has the `glm` openai-compatibility provider: base-url `https://api.z.ai/api/coding/paas/v4` (GLM Coding Plan — do NOT downgrade to `/paas/v4`), models `GLM-5.2` / `glm-5.1` / `glm-5-turbo`.

### Build / packaging
- `Dockerfile` — `ENV TZ=Australia/Sydney`. Reverted upstream's `debian:bookworm + CGO_ENABLED=1` to `alpine + CGO_ENABLED=0` with `BUILDPLATFORM` / `TARGETARCH` for fast native cross-compilation. Trade-off: no runtime `.so` plugin loading; acceptable for our deployment.
- Image tags on `ghcr.io/harshav167/cliproxyapi`: `:prod` is the rolling production tag (manually retagged after each deploy), `:f5-and-grok-fixes-<sha>`, `:upstream-sync-<sha>` and `:glm-cache-<sha>` are content-addressed pins. DO NOT push `:latest`. Current `:prod` digest as of 2026-06-21: `sha256:caabeb57c6bd93ebb2b72f0169743c6459641caeb083d60702520654e736bb43` (tag `:glm-cache-70205e6b`, GLM normalizer + xAI content[] merge fix), deployed to the `cli-proxy-api-test` clanker container. Prior: `sha256:bdf02e241c362003ac54f0e91c70ea7139d8d75f008ad33b7623c8b93f9ca76d` from `2ee2bf64`.
- Prod deploy target: the `cli-proxy-api-test` service in `/home/wade/cliproxy/compose.yaml` runs `ghcr.io/harshav167/cliproxyapi:prod` (`pull_policy: always`) on port 8312, labeled `cliproxy.instance=clanker`. The OTHER container `cli-proxy-api-plus` runs a DIFFERENT binary (`cliproxyplus:latest`, `./CLIProxyAPIPlus`) on 8317 — not built from this repo. Deploy: `docker compose pull cli-proxy-api-test && docker compose up -d cli-proxy-api-test`.

### Docs / governance
- `AGENTS.md` (this file) — fork-only. Upstream doesn't have it.
- `MIGRATION-LEDGER.md` — fork-only, tracks each upstream merge.
- `docs/upstream-sync.md` — fork-only. Full mental model + conflict-resolution playbook for the recurring upstream sync.
- `docs/signoz-observability.md` — fork-only. (Note: the SigNoz infra map above is the source of truth; the doc file is the dashboards/metrics reference.)
- `docs/security-backlog.md` — fork-only.

### Upstream sync workflow
Full playbook (conflict table, CGO trap, deploy gate): **`.agents/skills/upstream-sync/SKILL.md`** and **`docs/upstream-sync.md`**. Quick reference:

1. `git fetch upstream main`
2. `git checkout -b sync/upstream-<YYYY-MM-DD>`
3. `git merge --no-ff --no-commit upstream/main` — resolve conflicts manually, **preferring our fork's behaviour for every file in this section**. Standing rule: never revert a fork change to resolve a conflict; when both sides are additive, keep BOTH.
4. After merge, run `git status` and verify every file in the lists above is still present and unchanged (or expanded — never reduced). Most "deletions" in the diffstat are fork-only files upstream never had; `--no-ff --no-commit` preserves them.
5. `gofmt -w .` then `go build -o /tmp/build ./cmd/server && rm /tmp/build` then `go test ./...`.
6. Commit with `chore: sync upstream/main (<N> commits incl. <themes>)`.
7. Update `MIGRATION-LEDGER.md` with the sync date, commit count, and what each fork-only file required during merge (no change / re-applied / restructured).
8. Fast-forward `main` only after tests pass. Push to `origin/main`.
9. Build a new image tagged `:upstream-sync-<sha8>` and `:prod`. Rebuild local container on **8312** and get user sign-off in Cursor FIRST; do NOT pull on the prod VM until local smoke tests pass.

**CGO=0 Dockerfile trap:** upstream flips the Dockerfile to `bookworm + CGO_ENABLED=1` claiming the plugin host needs cgo. It does NOT for us — the cgo loader is build-tag gated (`internal/pluginhost/loader_unix.go` vs `loader_unsupported.go`). Keep our `alpine + CGO_ENABLED=0` cross-compile; verify with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/server`. Accepting upstream's change silently regresses build speed ~15s → ~3min and gains nothing we use.

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **CLIProxyAPI** (21912 symbols, 68823 relationships, 300 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> If any GitNexus tool warns the index is stale, run `npx gitnexus analyze` in terminal first.

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `gitnexus_impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `gitnexus_detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `gitnexus_query({query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `gitnexus_context({name: "symbolName"})`.

## Never Do

- NEVER edit a function, class, or method without first running `gitnexus_impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `gitnexus_rename` which understands the call graph.
- NEVER commit changes without running `gitnexus_detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/CLIProxyAPI/context` | Codebase overview, check index freshness |
| `gitnexus://repo/CLIProxyAPI/clusters` | All functional areas |
| `gitnexus://repo/CLIProxyAPI/processes` | All execution flows |
| `gitnexus://repo/CLIProxyAPI/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->

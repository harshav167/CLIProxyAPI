# Migration Ledger

This ledger records the execution baseline for migrating the product-bearing
current-state diff from `cpapi-plus` into this `CLIProxyAPI` checkout.

It is an execution artifact, not a historical narrative. Its job is to answer:

1. What exact source state are we migrating from?
2. What exact target state are we migrating into?
3. What was the target repo state before migration work started?
4. What verification signal existed before any migration bucket landed?

## Scope Rule

Migration scope is the **current end-state diff** of the product-bearing
customizations in `cpapi-plus`.

It is **not**:

- a replay of intermediary commits
- a preservation of original commit structure
- a port of local scaffolding/tooling like `.agents`, `.claude`, `.serena`,
  snapshots, or local-only operational artifacts

## Source Baseline

Repository:
- `cpapi-plus`
- Path: `/Users/harsha/Documents/GitHub/claude-tools/ccs/cliproxy/cpapi-plus`

Observed at baseline capture:
- Branch: `main`
- HEAD: `1d992f600a8e2aaac0da4b5b910b93c81a3e652a`
- Tag present: `pre-upstream-reconcile`

Relationship to upstream as observed during baseline capture:
- Upstream merge-base: `5dcca69e8cc3d36e66ec66c4e7925e2a7f57c90f`
- Divergence vs `upstream/main`: `712 ahead / 92 behind`

Source repo working tree notes:
- The repo contains unrelated local/untracked workspace artifacts and
  `.serena/project.yml` changes.
- Those local artifacts are **not** part of the migration scope.
- Migration source of truth is the committed product-bearing state plus
  verified current behavior, not the entire live workspace dirt.

## Target Baseline

Repository:
- `CLIProxyAPI`
- Path: `/Users/harsha/Documents/GitHub/claude-tools/ccs/cliproxy/CLIProxyAPI`

Observed at baseline capture:
- Branch: `port/cpapi-plus`
- HEAD: `79579c34bf9ea72f51ccaea53908741d84d05829`
- Remote configured at capture time:
  - `origin -> https://github.com/router-for-me/CLIProxyAPI`

## Target Working Tree State At Baseline

The target repo was **not perfectly clean** at the moment baseline was locked.

Observed modification:
- `internal/translator/openai/openai/responses/openai_openai-responses_request.go`

Interpretation:
- This is an earlier accidental partial port attempt in the exact translator
  area later identified as part of the migration scope.
- It means the migration does not start from a pristine target worktree.
- Before real bucket execution begins, this file must either:
  - be intentionally accepted as the first in-progress migration change, or
  - be reset and then re-ported deliberately.

This fact is recorded here so later history is not misread as if the target
baseline were totally clean.

## Pre-Migration Verification Snapshot

Focused test sweep run in target repo:

Command:

```bash
go test ./internal/translator/... ./internal/runtime/executor/... ./sdk/api/handlers/... ./sdk/cliproxy/auth/...
```

Observed result:
- Translator packages: passing
- `sdk/api/handlers`: passing
- `sdk/cliproxy/auth`: passing
- `internal/runtime/executor`: failing

Observed failing tests in target baseline:
- `TestEnsureAccessToken_WarmTokenLoadsCreditsHint`
- `TestUpdateAntigravityCreditsBalance_LoadCodeAssistUserAgent`

Interpretation:
- These failures appeared **before** the migration work started.
- They must be treated as baseline failures unless later migration changes
  expand or alter the failure set.

Build snapshot in target repo:

Command:

```bash
go build -o cli-proxy-api ./cmd/server
```

Observed result:
- Build passed successfully

## Baseline Artifacts

This ledger is one baseline artifact.

Additional baseline artifact to create immediately after this file:
- a target-repo baseline tag on current HEAD

Recommended purpose of the tag:
- capture the target commit that migration work starts from
- allow later comparison if bucket work needs rollback or branch recreation

## Migration Buckets

The migration will be executed by current-state feature bucket, not history
replay.

Planned buckets:

1. Core Codex/OpenAI transport and translators
2. Handler/API surface
3. Config surface required by migrated behavior
4. Auth/provider expansion
5. Claude cache and shared usage accounting
6. Model registry and metadata expansion
7. Management/API product-critical extras
8. Tests and verification in lockstep

## Immediate Next Step

Before further execution:

1. Create and verify the target baseline tag
2. Decide whether the dirty target translator file is retained as intentional
   in-progress migration work or reset before the first bucket
3. Return to planning with this ledger as the authoritative execution baseline

## Final Verification Addendum

Migration execution is now complete for the intended local `cpapi-plus` parity
scope. **`kiro`, `kilo`, and `iflow` are permanently dropped** — not ported to
the fork runtime. They remain only in the frozen `cpapi-plus` tag for archival
reference; no follow-up port is planned.

Completed migration scope:

- Core Codex/OpenAI transport and translators
- Handler/API surface
- Config surface required by migrated behavior
- Auth/provider expansion for GitLab, CodeBuddy, GitHub Copilot, and Cursor
- Claude cache and shared usage accounting
- Model registry and metadata expansion

Final verification outcome:

- Focused migrated package verification: passing
- `go build -o cli-proxy-api ./cmd/server`: passing
- `go build ./...`: passing

Baseline failures that remain unchanged from pre-migration state:

- `TestEnsureAccessToken_WarmTokenLoadsCreditsHint`
- `TestUpdateAntigravityCreditsBalance_LoadCodeAssistUserAgent`

Classification:

- The two Antigravity failures above remain classified as baseline failures that
  predated migration execution and do not indicate missing migration work.
- `TestOpenAIResponsesToOpenAI_IgnoresBuiltinTools` is classified as a stale
  expectation relative to the migrated target behavior, because the resulting
  implementation matches current `cpapi-plus` builtin tool passthrough behavior
  rather than indicating an incomplete migration.
- `kiro`, `kilo`, and `iflow` are **permanently dropped** from fork scope (user
  decision 2026-05-27). They are not part of the migrated runtime.

## Phase 2 Rebase Resume (2026-05-22)

The interactive rebase started by the original migration pass was paused with 9
unresolved conflict files and 2 commits remaining in the queue
(`b3f21f1b` Claude prompt-cache anchor + beta/thinking parity, `83a9426a`
redisqueue). This addendum records the resumption and resolution.

Observed pre-resume state:

- `interactive rebase in progress; onto 3a9fb378`
- last command done: `pick 577f7454 # feat(migration): port cpapi-plus
  product-bearing parity onto upstream main`
- 9 unresolved conflict files, all `both modified`:
  - `cmd/server/main.go`
  - `internal/api/handlers/management/auth_files.go`
  - `internal/api/handlers/management/handler.go`
  - `internal/api/handlers/management/usage.go`
  - `internal/api/server.go`
  - `internal/runtime/executor/helps/usage_helpers.go`
  - `internal/translator/openai/openai/responses/openai_openai-responses_request.go`
  - `sdk/api/handlers/openai/openai_handlers.go`
  - `sdk/api/handlers/openai/openai_responses_handlers.go`

Conflict resolution philosophy applied:

- Kept upstream's v7 module path (`router-for-me/CLIProxyAPI/v7`) and merged
  upstream's new imports (`internal/home`) with the migration commit's added
  imports (`internal/usage`, codex/cursor/copilot/gitlab auth packages, etc.).
- For semantic conflicts in `openai_openai-responses_request.go` (the
  `function_call` vs `custom_tool_call` case), kept BOTH sides: upstream's
  `pendingToolCalls` buffering correctness AND the migration's
  `custom_tool_call` case label + `arguments`/`input` dual-read for ApplyPatch
  support.
- Performed a bulk `v6 -> v7` import rewrite across the 24 migration-added
  files (`internal/auth/{codebuddy,copilot,cursor,gitlab}/*`,
  `internal/runtime/executor/{codebuddy,cursor,github_copilot,gitlab,compat}_executor.go`,
  `internal/cmd/{codebuddy,cursor,github_copilot,gitlab}_login.go`,
  `internal/usage/*`, `sdk/api/handlers/openai/endpoint_compat.go`,
  `sdk/auth/{codebuddy,cursor,github_copilot,gitlab}.go`,
  `internal/api/handlers/management/auth_files_gitlab_test.go`).
- Resolved `go.mod` / `go.sum` conflicts during the `83a9426a` (redisqueue)
  pick by accepting the redisqueue side and running `go mod tidy` to
  reconcile direct vs indirect placement.

Post-rebase verification:

- `gofmt -l .`: clean.
- `go build -o /tmp/clip-newfork ./cmd/server`: success.
- `go test ./internal/translator/... ./internal/runtime/executor/...
  ./sdk/api/handlers/... ./sdk/cliproxy/auth/...`:
  passing, modulo the two pre-existing Antigravity baseline failures already
  documented above.

Post-rebase HEAD:

- `f5f3353d feat(redisqueue): add optional external Redis/Valkey backend`
- `bd49018a feat(claude): port cpapi-plus prompt-cache anchor + beta/thinking parity`
- `4d9e4445 feat(migration): port cpapi-plus product-bearing parity onto upstream main`
- `3a9fb378 fix(home): implement home dispatch headers and enhance Gemini model handling` (upstream)

## Phase 2 Delta Check: cpapi-plus Commits Since Ledger Baseline

`cpapi-plus@1d992f60` was the original migration source baseline. cpapi-plus
HEAD is currently `b5cd8425` (`fix cursor gpt http response routing`),
representing 15 commits of additional product-bearing work since the baseline.
These commits' end-state diffs are not fully captured in the migration commit
`4d9e4445` — the new fork is dormant and a follow-up migration pass is needed.

Cpapi-plus commits between `1d992f60..b5cd8425` (oldest -> newest):

- `3a144e60` fix(codex): preserve custom_tool_call type tag through chat->codex translator
- `8160c638` feat(claude+cursor): Phase 25.5/26/27 — Cursor BYOK Claude path repair, Fast-mode service_tier, cache TTL/effort/diagnosis betas
- `ece25714` fix(cursor): preserve raw system prompt identity
- `479d929e` fix(cliproxy): honor proxy environment for transports
- `7a259a78` chore: snapshot current service changes
- `6e766df3` fix(codex): preserve cursor responses request semantics
- `6a94c8de` feat: add cursor gpt prompt upgrade flag
- `b0bbe381` chore: snapshot current service fixes
- `2591c258` fix: balance cursor prompt upgrade modes
- `dc3726e5` fix: require concrete edit authorization in cursor prompt
- `95298e25` checkpoint current go changes
- `7491fff2` checkpoint current new go files
- `31125531` add cursor execution integrity prompt contract
- `423ccde1` Promote Claude stream errors before translation
- `b5cd8425` fix cursor gpt http response routing

Code-level deltas observed via filesystem diff (cpapi-plus vs new fork) that
are clearly missing from the migration:

1. Files that exist in cpapi-plus but NOT in the new fork (pre-Phase-1
   product work):
   - `internal/runtime/executor/codex_http_ws_bridge.go` + test
   - `internal/runtime/executor/codex_remote_compact.go` + test
   - `internal/runtime/executor/helps/cursor_system_prompt.go` + test
     (contains the GPT-5.4 execution-integrity contract patches used by the
     openai handler family; `RewriteCursorSystemPromptIdentityAndIntegrity`
     and the persistence/integrity prompt patches.)

2. Cpapi-plus Phase 1 thermo-fixup additions (committed in cpapi-plus AFTER
   baseline; landing pending in cpapi-plus). These need to be re-applied
   in the new fork:
   - G2 fix in `internal/runtime/executor/xai_executor.go` + new test
     `xai_executor_test.go`.
   - C3 named constant + `claude_stream_errors.go` extraction.
   - G3 hasModelProvider helper collapse in `sdk/api/handlers/openai/`.
   - G7 `helps.ExecutionSessionIDFromOptions` move +
     `helps/session_id_cache.go` export.
   - C4 docstring audit in `helps/cursor_system_prompt.go`.
   - G1 session-id symmetry on non-stream chat in `openai_handlers.go` +
     regression test.
   - C1 Claude beta-header merge regression test in `claude_executor_test.go`.
   - C2 Opus 4.7 thinking-parity boundary test in `claude_executor_test.go`.
   - G5 Codex translator `service_tier`/`prompt_cache_key` regression test.
   - G4 `openai_handlers.go` decomposition into 4 sibling files:
     `openai_session_id.go`, `openai_provider_routing.go`,
     `openai_responses_bridge.go`, `openai_model_metadata.go`.
   - C5 `claude_executor.go` decomposition into 5 sibling files:
     `claude_stream_errors.go` (executor pkg),
     `helps/claude_oauth_tool_names.go`, `helps/claude_cache_control.go`,
     `helps/claude_billing.go`, `helps/claude_cursor_system_prompt.go`.

Explicit deferral:

- G6 (xai executor verbatim port from upstream's 940-LoC implementation) is
  not yet applied. The current xai executor in the new fork is the
  cpapi-plus 511-LoC custom version (with G2 still missing here). The plan
  designated this for a follow-up commit on top of the rebase, not as part
  of the rebase itself.

Recommended next action when the new fork is brought online:

1. Cherry-pick or hand-port the 3 missing product files
   (`codex_http_ws_bridge.go`, `codex_remote_compact.go`,
   `helps/cursor_system_prompt.go`) from cpapi-plus first — these are
   product behavior the migration commit missed.
2. Port the Phase 1 thermo-fixup work from cpapi-plus into the new fork
   (the same 11 items listed above, against the v7 module paths).
3. Then land G6 (xai verbatim from upstream) on top.

This addendum was authored after Phase 2 rebase resume on 2026-05-22 and
records the actual post-rebase state, not the pre-Phase-1 plan-anticipated
state. The new fork remains dormant (not in production); cpapi-plus@b5cd8425
plus its Phase 1 thermo-fixup working tree remains the production source of
truth until the deltas above are reconciled.

## Dev Utility Deferral

`cmd/mcpdebug/main.go` and `cmd/protocheck/main.go` are intentionally deferred.
They are dev-time debug utilities for `internal/auth/cursor/proto`, and that
proto package is already present in the fork.

These commands have no runtime dependency on anything else in `cpapi-plus`.
The migration bar is shipping runtime customizations, not scratch tools. If
they are needed later, port them verbatim with the same v6-to-v7 import rewrite
used for migrated tests.

## Removal: wrongly-ported provider stacks (2026-05-31)

The `f88a5eae` migration commit ported four upstream-only provider stacks that
were **never part of the user's customizations** and are **not present in
upstream `router-for-me/CLIProxyAPI` main**: GitLab Duo, CodeBuddy, GitHub
Copilot, and the Cursor *backend provider* (distinct from the Cursor IDE
client customizations, which are legitimate and retained).

The port was also incomplete: the implementation packages/executors/login
helpers (`internal/auth/{copilot,cursor,gitlab,codebuddy}`,
`internal/runtime/executor/{github_copilot,codebuddy,cursor,gitlab}_executor.go`,
`internal/cmd/*_login.go`, `sdk/auth/{...}.go`) were absent from the tree, but
the references to them survived in shared files. After the 2026-05-29 upstream
sync merge this produced a broken build (`go build ./...` failed on
`no required module provides package internal/auth/{copilot,cursor,gitlab}`).

Removed all residue (build green, `go vet` clean, 2122 tests pass):

- `internal/api/handlers/management/auth_files.go` — restored to upstream/main
  (was upstream + 601 lines of GitLab/Copilot/Cursor handlers, 0 legitimate
  fork changes).
- `internal/api/server.go` — dropped 4 route registrations
  (`gitlab-auth-url` GET/POST, `cursor-auth-url`, `github-auth-url`).
- `cmd/server/main.go` — dropped 5 login flags + dispatch branches
  (`gitlab-login`, `gitlab-token-login`, `cursor-login`,
  `github-copilot-login`, `codebuddy-login`).
- `internal/registry/model_definitions.go` — restored to upstream/main
  (dropped `GetCodeBuddyModels` / `GetCursorModels` / `GetGitHubCopilotModels`
  stubs + their channel/lookup wiring).
- `sdk/cliproxy/service.go` — dropped 4 authenticators, 4 executor
  registrations, and 4 model-fetch cases.
- `internal/api/handlers/management/oauth_sessions.go` — dropped the
  github-copilot/gitlab/cursor cases from `NormalizeOAuthProvider` (matches
  upstream).
- `internal/api/handlers/management/oauth_sessions_test.go` — deleted
  (only exercised the removed github-copilot normalization).

Retained (NOT a backend provider — these are the Cursor IDE client
customizations and stay): `helps/cursor_system_prompt.go`,
`RewriteCursorSystemPromptIdentityAndIntegrity`, the GPT prompt-upgrade flag,
and all Cursor BYOK Claude/Codex request handling.

Verification anchored with GitNexus impact analysis (index re-built at HEAD
`327b5fac`): `GetCursorModels` blast radius = LOW, 2 direct callers in the same
file, 0 affected execution flows — confirming the removal is self-contained.

## Upstream Sync — 2026-06-13 (34 commits)

Branch: `sync/upstream-2026-06-13`. Merge-base `21387f5c`, upstream tip
`94c5a7fd`, merge commit `7dad9253`. Divergence at start: 34 upstream / 53 fork.

Incoming themes: plugin store (install-from-latest-release, unload/config
preservation), htmlsanitize utilities, antigravity Claude-WebSearch →
native googleSearch bridge, executor-registration refactor, translator
stream-specific transforms + cache token aggregation + finish_reason
correctness, CORS exposed-header fixes.

Conflicts (6 files) and per-file decisions:

- `internal/runtime/executor/claude_executor.go` (5 hunks) — re-applied. Kept
  our `prepared.*` flow and the Fable alias hook
  (`ApplyCursorFableAliasSnapshot` still runs after `thinking.ApplyThinking`,
  before `applyCloaking`). Dropped upstream's re-inlined `from`/`to`/`body`
  translation vars (unused in our architecture).
- `internal/api/handlers/management/handler.go` — kept BOTH. Our `usageStats`
  field/import + upstream's `pluginstore` import and plugin-store fields.
- `internal/registry/model_definitions_test.go` — kept BOTH test funcs
  (our grok-composer 256k override test + upstream's antigravity websearch test).
- `internal/runtime/executor/codex_websockets_executor.go` — restructured.
  Adopted upstream's `responseFormat` arg rename on `TranslateStream`; kept our
  raw-fallback synth-error branch (`needsRawFallback` +
  `synthesizeChatCompletionsErrorChunk`).
- `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go`
  (4 hunks) — merged BOTH features. Our `ThoughtTextActive` sentinel-thought
  routing coexists with upstream's `UpstreamFinishReason` + `SawToolCall`
  final-chunk finish_reason logic. Streaming-func `hasFunctionCall` removed
  (superseded by `SawToolCall`); non-stream `hasFunctionCall` untouched.
- `gemini_openai_response_test.go` — kept BOTH (our 4 sentinel-thought tests +
  upstream's `TestGeminiFinishReasonOnlyOnFinalChunk`).

Upstream-intended deletions accepted (NOT fork features):
- `examples/plugin/jshandler/*` — upstream `b6c22f2d` removed the JS handler.
- `sdk/cliproxy/service_xai_executor_binding_test.go` — upstream `ca1f6271`
  replaced it with `service_executor_registration_test.go` (which still covers
  `xai`); our XAIExecutor registration in `service.go` and the 20
  `xai_executor_test.go` unit tests are intact.

Dockerfile: upstream switched to `debian:bookworm + CGO_ENABLED=1` with `zig cc` cross-compilation to support runtime `.so` plugin loading. Adopted upstream's build on this branch.

Verification: `gofmt -w .` clean; `go build ./cmd/server` clean; `go test ./...`
green (68 packages, 0 fail), including the two hand-merged packages
(`translator/gemini/openai/chat-completions`, `handlers/management`).

Pending: fast-forward `main`, push `origin/main`, build `:upstream-sync-7dad9253`
+ `:prod`, local 8312 smoke + user Cursor sign-off BEFORE prod pull.

## Upstream Sync — 2026-07-05 (35 commits)

Branch: `sync/upstream-2026-07-05`. Merge-base `4c0c6029`, upstream tip
`5afc0f1d`. Divergence at start: 35 upstream / 71 fork.

Incoming themes: plugin store auth providers + direct install type +
manifest fetching (11 commits), `disable-cooling` field in management
PatchOpenAICompat, Codex WS-to-SSE full transcript replay, force-mapped
Responses SSE framing fix, Gemini Responses reasoning two-part
signatures, Antigravity CLI User-Agent agy 1.0.13 short form, reasoning
content handling, snapshot test refactor, README sponsor additions
(VisionCoder/Code0/CyberPay/Claude API), Claude Sonnet 5 metadata, and
`fix(translator): remove temperature parameter handling in Claude
request transformations` (closes #4071).

Conflicts (4 files, all in the Sonnet-5 / Claude-temperature area we
already ported via `10c2f474` on 2026-07-01) and per-file decisions:

- `internal/registry/models/models.json` — kept OUR bounded thinking
  block for `claude-sonnet-5` (`min: 1024, max: 128000, zero_allowed:
  true`, no `dynamic_allowed`). Dropped upstream's
  `dynamic_allowed: true` shape. Per user decision: preserve the proven
  fable-5 shape; do not enable Anthropic's adaptive path for Sonnet 5.
- `internal/registry/model_registry_safety_test.go` — kept OUR assertion
  matching the bounded thinking block.
- `internal/runtime/executor/claude_executor.go` (3 hunks) — kept OUR
  `prepared.*` factored flow (the O(n²) perf fix from `9a57e522`,
  load-bearing for 2.2 MB Opus payloads). Dropped upstream's re-inlined
  body-manipulation sequence at both Execute and ExecuteStream call
  sites. Kept OUR `normalizeClaudeSamplingForThinking` (coerce
  temperature to 1 when thinking active) over upstream's
  `normalizeClaudeSamplingForUpstream` (delete temperature
  unconditionally). Per user "keep ours" stance for this Sonnet-5
  cluster + standing rule "don't revert our changes even if upstream
  conflicts". Residual: the auto-merged translator
  `claude_openai_request.go` now drops `temperature` from OpenAI inbound
  (upstream `5afc0f1d`); our function only acts on native Claude clients
  that send `temperature` directly — defensive, no break.
- `internal/runtime/executor/claude_executor_test.go` (4 hunks) — kept
  OUR 5 tests verbatim (assert coerce-to-1 semantics matching the
  function we kept). Dropped upstream's 6
  `TestNormalizeClaudeSamplingForUpstream_*` tests.

Fork-only files (12 in the AGENTS.md inventory) verified present and
unchanged-or-expanded: `AGENTS.md`, `MIGRATION-LEDGER.md`,
`docs/upstream-sync.md`, `docs/signoz-observability.md`,
`docs/security-backlog.md`, `internal/usage/logger_plugin.go`,
`internal/runtime/executor/helps/cursor_system_prompt.go`,
`internal/runtime/executor/helps/claude_cursor_system_prompt.go`,
`internal/runtime/executor/helps/cursor_fable_alias.go`,
`internal/runtime/executor/helps/glm_normalizer.go`,
`internal/runtime/executor/helps/proxy_helpers.go`,
`internal/registry/model_definitions.go`. The `f5-*` Fable-5 hook point
in `claude_executor.go` is preserved (the `prepared.*` flow survives).

Dockerfile: upstream switched to `debian:bookworm + CGO_ENABLED=1` with `zig cc`. Adopted on this branch for plugin support.

Pre-existing flake noted (NOT introduced by this merge): on clean
`main`, `TestAntigravityRefresh_DeduplicatesConcurrentRefresh` in
`internal/runtime/executor` hangs (httptest.Server accept loop never
returns). Reproduced on `main` HEAD `1adff3a3` before redoing the merge.
Skipped via `-skip` for the green re-run; the rest of
`internal/runtime/executor` passes in 2.5s.

Post-sync fork-only commits added on this branch:
- `cefdc6e5` — feat(codex): port CodexCont continue-thinking fold into the
  codex executor (`internal/runtime/executor/codex_continue_fold.go` +
  `internal/runtime/executor/helps/codex_continue_thinking.go`). Default-off,
  gated by `codex_continue_thinking.enabled` + reasoning model.
- `be27a21c` — feat(deploy): GLM thinking dialect guard + openai-compat stream
  normalizer (`internal/runtime/executor/helps/openai_compat_stream_normalizer.go`)
  + adopt upstream's `bookworm + CGO_ENABLED=1 + zig` Dockerfile for plugin
  host support.

Verification: `gofmt -w .` clean; `go build -o /tmp/cli-proxy-build
./cmd/server` clean; `go test -timeout 120s -skip
TestAntigravityRefresh_DeduplicatesConcurrentRefresh ./...` green (all
packages pass, 0 fail).

Pending: commit, fast-forward `main`, push `origin/main`, build
`:upstream-sync-<sha8>` + `:prod`, local 8312 smoke + user Cursor
sign-off BEFORE prod pull.

## Post-Sync Fork Commits

Two fork-only commits were added on top of the upstream sync to land new
features and fix review findings. They must be preserved in the next upstream
merge.

- `cefdc6e5` — feat(codex): port CodexCont continue-thinking fold into the
codex executor. New files: `internal/runtime/executor/codex_continue_fold.go`,
`internal/runtime/executor/helps/codex_continue_thinking.go`. Hook in
`internal/runtime/executor/codex_executor.go`. Default-off; gated by
`codex_continue_thinking.enabled` and a reasoning model.

- `be27a21c` — feat(deploy): GLM thinking dialect guard + openai-compat stream
normalizer + CGO=1 plugin host. New file:
`internal/runtime/executor/helps/openai_compat_stream_normalizer.go`, called
from `internal/runtime/executor/openai_compat_executor.go`. Dockerfile switched
to `debian:bookworm + CGO_ENABLED=1 + zig cc` for plugin support. GLM
normalizer updated with thinking guards.

- (review fixes) Dead-code removal, identity-state per-round fix, response-body
leak fix, per-request Claude user IDs, plugin-store safe HTTP client, bounded
plugin-version concurrency, and doc updates were applied in follow-up edits on
this branch.

## 2026-07-25 — Claude Opus 5 onboarding and Fable alias removal

- Added the `claude-opus-5` catalog entry and adaptive thinking-level aliases.
- Updated the Claude client fingerprint to the captured Opus 5 client shape.
- Added cache-diagnostics chaining keyed by the real Cursor execution session,
  including `metadata.user_id` fallback and strict upstream message-ID parsing.
- Restored the `extended-cache-ttl-2025-04-11` beta and explicit `ttl: "1h"` on
  proxy-owned cache anchors after production showed silent five-minute writes.
- Removed the unused `f5-*` config alias hook, embedded snapshot, tests, config
  guidance, and current architecture documentation. The direct
  `claude-fable-5` catalog model remains supported. Older historical ledger
  entries remain unchanged because they describe what prior upstream syncs
  preserved at the time.

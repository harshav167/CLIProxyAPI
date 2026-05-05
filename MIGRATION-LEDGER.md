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
scope, with `kiro` explicitly left out of this commit as a deferred follow-up.

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
- `kiro` remains intentionally deferred and is not part of the locally-ready
  migrated state recorded by this ledger.

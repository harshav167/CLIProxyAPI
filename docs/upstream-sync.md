# Upstream Sync Playbook

How to bring `router-for-me/CLIProxyAPI` (upstream) into the
`harshav167/CLIProxyAPI` fork without losing fork features.

This is fork-only governance. Upstream does not have this file.

- **Quick reference / agent trigger:** `.agents/skills/upstream-sync/SKILL.md`
- **Fork-feature inventory (source of truth):** `AGENTS.md` → "Fork features to
  preserve across upstream merges"
- **Per-merge audit trail:** `MIGRATION-LEDGER.md`

## Mental model

The fork is a **behavior superset** of upstream. Upstream is a moving base; our
customizations sit on top:

- Cursor system-prompt rewrite (identity + integrity contract) for Claude/GPT/Fable
- xAI / Grok Composer request normalizers (422 / 400 fixes)
- `f5-*` Cursor Fable 5 aliases (bypass the ZDR routing gate) + embedded snapshot
- Deeper observability (OTel → SigNoz, quota metrics, error-body transport logs)
- Billing / cache-control behavior tuned to Claude Code's canonical layout
- `alpine + CGO_ENABLED=0` Dockerfile with native cross-compile

A sync is successful only if **every** item above survives unchanged or
expanded. Reverting one to resolve a conflict is a failure.

### Standing rules (from the user)

- "when merging make sure we dont revert our changes even if upstream conflicts"
- Local container first, prod second. Never deploy to prod before the user
  validates the build in Cursor against `127.0.0.1:8312`.

## Procedure

```bash
git fetch upstream main
git checkout -b sync/upstream-$(date +%Y-%m-%d)
git merge --no-ff --no-commit upstream/main      # inspect before committing

# Triage scope
git rev-list --left-right --count upstream/main...HEAD   # ahead / behind
git log --oneline <merge-base>..upstream/main            # incoming themes
grep -rn '^<<<<<<<' --include='*.go' --include='go.mod' . # real conflicts
```

Resolve conflicts using the table below, then:

```bash
gofmt -w .
go build -o /tmp/build ./cmd/server && rm /tmp/build
go test ./...        # ~62 packages green
```

Commit (`chore: sync upstream/main (<N> commits incl. <themes>)`), update
`MIGRATION-LEDGER.md`, fast-forward `main`, push, then run the deploy gate.

## Conflict-resolution table

| File / area | Conflict shape | Resolution |
|---|---|---|
| `go.mod` / `go.sum` | upstream older minors vs our OTel + newer crypto/net/oauth2 | Keep ours; `go mod tidy` reconciles indirects |
| `internal/config/config.go` | our Redis/Observability/OTLP vs upstream Plugins/PluginInstance | Keep BOTH (additive) |
| `internal/api/server.go`, `handlers/management/handler.go` | observability vs pluginhost wiring/setters | Keep BOTH |
| `sdk/cliproxy/service.go` | our observability lifecycle vs upstream API-key/plugin lifecycle | Keep BOTH |
| `cmd/server/main.go` | our redis env overrides vs upstream plugin bootstrap | Keep BOTH |
| `internal/runtime/executor/*` | upstream executor/translator refactors vs our hooks | Re-apply our hook onto moved call site; verify `ApplyCursorFableAliasSnapshot` runs after `thinking.ApplyThinking`, before `applyCloaking` |
| `Dockerfile` | upstream bookworm+CGO=1 vs our alpine+CGO=0 | Keep ours (see CGO trap) |

When in doubt: prefer ours, or keep both if additive. Only take upstream's side
for a pure bugfix that does not touch fork behavior.

## The CGO=0 Dockerfile trap

Upstream switches to `golang:1.26-bookworm` + `CGO_ENABLED=1`, implying the
plugin host requires cgo. It does not for our build:

- cgo loader is gated `//go:build cgo && (linux || darwin || freebsd)`
  (`internal/pluginhost/loader_unix.go`, `host_callbacks_unix.go`)
- `internal/pluginhost/loader_unsupported.go` is the no-op fallback for `CGO_ENABLED=0`

Verify before reverting upstream's change:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/cv ./cmd/server && rm /tmp/cv
```

Keep `alpine + CGO_ENABLED=0` with `--platform=$BUILDPLATFORM` /
`GOARCH=${TARGETARCH}` and `ENV TZ=Australia/Sydney`. We lose only runtime `.so`
plugin loading (unused). Accepting upstream's change silently regresses amd64
build time from ~15s to ~3min (QEMU emulation) for zero benefit.

## Deploy gate

1. Rebuild local container (`docker/docker-compose.local.yml`, host port **8312**);
   confirm health and that new models/aliases show in `/v1/models`.
2. User tests in Cursor against `127.0.0.1:8312`.
3. After sign-off: build `:prod` (amd64) → push to `ghcr.io/harshav167/cliproxyapi`
   → tag `:upstream-sync-<sha8>` + `:prod` → pull on prod VM → restart → smoke-test
   via the clanker tunnel.
4. Back up the hand-edited prod config before changes; patch it in place. Never
   `scp` the local config over prod.

## Definition of done

- All conflicts resolved keeping fork behavior; no fork-feature file reduced.
- `gofmt` clean, `go build` clean, `go test ./...` green.
- `MIGRATION-LEDGER.md` updated with date, commit count, per-file decisions.
- `main` fast-forwarded + pushed.
- Local verified on 8312; prod deployed only after user sign-off.

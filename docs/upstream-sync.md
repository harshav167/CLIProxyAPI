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
- CGO-enabled, glibc-compatible plugin runtime: Bookworm builder with Zig
  cross-compilation and a distroless Debian production image

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
| `Dockerfile` | upstream build/runtime changes vs our plugin-capable CGO + distroless policy | Preserve `CGO_ENABLED=1`, Zig cross-compilation, glibc compatibility, the distroless Debian production runtime, non-root execution, and `ENV TZ=Australia/Sydney` |

When in doubt: prefer ours, or keep both if additive. Only take upstream's side
for a pure bugfix that does not touch fork behavior.

## Plugin-capable CGO build policy

The production build must keep `CGO_ENABLED=1`. Go's real `plugin` loader is
CGO-only, and store-downloaded `.so` plugins require glibc at runtime. An Alpine
or `CGO_ENABLED=0` build may compile through the unsupported-loader fallback,
but it disables the plugin-store behavior production is required to preserve.

The production Dockerfile therefore uses:

- `golang:1.26-bookworm` as the builder
- Zig for reproducible Linux cross-compilation
- `gcr.io/distroless/base-debian12:nonroot` as the runtime
- glibc, libssl, CA certificates, and tzdata from the distroless base
- no shell, package manager, curl, or in-container healthcheck
- non-root uid 65532 and `ENV TZ=Australia/Sydney`

Verify the ARM64 production binary with:

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
  CC="zig cc -target aarch64-linux-gnu" \
  go build -o /tmp/cv ./cmd/server && rm /tmp/cv
```

`Dockerfile.debug` is the shell-enabled Debian image for debugging only. Do not
replace the production distroless runtime with it.

## Deploy gate

1. Rebuild local container (`docker/docker-compose.local.yml`, host port **8312**);
   confirm health and that new models/aliases show in `/v1/models`.
2. User tests in Cursor against `127.0.0.1:8312`.
3. After sign-off: build and push an ARM64 image tagged
   `:upstream-sync-<sha8>` and `:prod-arm64`. Update the multi-arch `:prod`
   manifest only after it includes the ARM64 image. Never push `:latest`.
4. Deploy only to the ARM Axion production VM through the
   `cliproxy-prod-arm` MCP. In `/home/wade/cliproxy`, run
   `docker compose pull cli-proxy-api-test && docker compose up -d cli-proxy-api-test`,
   then smoke-test `GET /healthz` and the expected model aliases. Do not use the
   decommissioned x86 VM or direct SSH.
5. Back up the hand-edited prod config before changes; patch it in place. Never
   `scp` the local config over prod.

## Definition of done

- All conflicts resolved keeping fork behavior; no fork-feature file reduced.
- `gofmt` clean, `go build` clean, `go test ./...` green.
- `MIGRATION-LEDGER.md` updated with date, commit count, per-file decisions.
- `main` fast-forwarded + pushed.
- Local verified on 8312; prod deployed only after user sign-off.

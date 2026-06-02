# Security & Robustness Backlog

Tracked issues surfaced by a code-review pass on 2026-06-03, NOT yet fixed.
These are pre-existing fork/upstream code paths, separate from the SigNoz
observability work (commits `a5d1f2a0`, `3443c413`, which already fixed the
observability-owned items: partial-Start provider leak, span error redaction,
logrus value redaction, and the codex/antigravity nil-auth stream guards).

Each item below was confirmed against live source at the cited line at the time
of writing. Line numbers drift — grep the symbol, not the line.

Status legend: `open` (unstarted), `in-progress`, `done` (link the commit).

---

## Auth / OAuth

### [P1] Completing one management OAuth login clears ALL pending sessions for that provider — `open`
- **File:** `internal/api/handlers/management/oauth_sessions.go` (`CompleteProvider`, ~line 116)
- **Call sites:** `internal/api/handlers/management/auth_files.go` (e.g. ~1773, 2031, 2178, 2338, 2520, 2599)
- **Detail:** Successful flows call `CompleteOAuthSession(state)` then
  `CompleteOAuthSessionsByProvider("anthropic"|"gemini"|"codex"|…)`, which
  deletes *every* pending session for the provider, not just the completed
  state. A second concurrent Web-UI login for the same provider loses its
  session and fails callback handling.
- **Fix direction:** delete only the completed `state`; never bulk-clear by
  provider. Add a concurrent-two-login test.

### [P1] Gemini management OAuth does not verify callback `state` — `open`
- **File:** `internal/api/handlers/management/auth_files.go` (`RequestGeminiCLIToken`, callback consumption ~1834+)
- **Detail:** Reads `.oauth-gemini-{state}.oauth` and accepts code on
  error/code only. The Anthropic flow explicitly checks
  `resultMap["state"] != state` (~line 52 of that flow); Gemini never compares
  the stored OAuth payload `state` to the registered session → weak CSRF /
  session binding.
- **Fix direction:** mirror the Anthropic state-equality check before accepting
  the code.

### [P1] Gemini OAuth `state` is predictable — `open`
- **File:** `internal/api/handlers/management/auth_files.go` (~line 1800)
- **Detail:** Gemini uses `state := fmt.Sprintf("gem-%d", time.Now().UnixNano())`
  while every other provider uses `misc.GenerateRandomState()`. Timestamp-based
  state is guessable.
- **Fix direction:** switch Gemini to `misc.GenerateRandomState()`. Trivial,
  do alongside the state-verification fix above.

### [P2] Manual xAI callback paste ignores IdP `state` (CLI) — `open`
- **File:** `sdk/auth/xai.go` (`parseXAIManualCallbackToken`, ~234–238; validated ~177–179)
- **Detail:** For a pasted code, `State` is set to the locally-expected value,
  so the later `result.State != state` check passes without validating the
  provider-returned state. A stolen authorization code can be bound to the
  active CLI session. (The browser-callback path still validates query state.)
- **Fix direction:** validate the IdP-returned state from the pasted callback
  URL, not a locally-synthesized one.

### [P2] Home cluster JWT decoded without signature verification — `open`
- **File:** `internal/home/certificate.go` (`parseHomeJWTClaims` ~80–108, enrollment ~300–318)
- **Detail:** Only base64-decodes the payload. Enrollment trust rests on
  `EnrollmentSecret` over TCP to `claims.IP:claims.Port`; a forged JWT can
  redirect enrollment to an attacker host if the secret leaks/is guessable. CA
  fingerprint is checked on returned CA material, not on JWT authenticity.
- **Fix direction:** verify JWT signature against the expected cluster key
  before trusting `IP`/`Port`. Larger change — design first.

---

## Storage / queue

### [P1] Postgres store bootstrap can wipe local-only auth files — `open`
- **File:** `internal/store/postgresstore.go` (`syncAuthFromDatabase`, ~line 458)
- **Detail:** Runs `os.RemoveAll(s.authDir)` then rebuilds only from DB rows.
  Credentials present on disk but not yet persisted to Postgres are destroyed
  on bootstrap/sync. Data-loss risk on first PG enablement.
- **Fix direction:** reconcile (upsert DB rows over disk) instead of
  remove-all-then-rebuild; or push disk-only creds to DB before clearing.
  Guard behind a confirmed-empty-or-merged check.

### [P1] Usage queue JSON stores API key field in Redis — `open` (severity depends on path)
- **File:** `internal/redisqueue/plugin.go` (`queuedUsageDetail.APIKey` ~119, set from `record.APIKey` ~49/101)
- **Nuance (verify before fixing):** For the inbound client path,
  `UsageReporter` already fingerprints the key (`FingerprintAPIKey` → `fp_…`),
  so the queue stores a fingerprint, NOT the raw inbound key. The exposure is
  real only if any upstream path populates `record.APIKey` with a raw upstream
  credential. Audit all `record.APIKey` writers before deciding.
- **Fix direction:** guarantee `record.APIKey` is always a fingerprint at the
  source, or drop/fingerprint the field at the queue boundary. Don't assume
  raw — confirm with a writer audit first.

### [P1] Redis usage enqueue drops records on `LPUSH` failure — `open`
- **File:** `internal/redisqueue/redis_backend.go` (`Enqueue`, ~133–137)
- **Detail:** Logs and returns on `LPUSH` error with no retry/alternate
  backend → usage events lost during brief Redis outages.
- **Fix direction:** bounded retry + fallback to the in-memory backend so a
  transient outage doesn't silently drop billing data.

### [P2] Redis usage prune may leave stale tail entries — `open`
- **File:** `internal/redisqueue/redis_backend.go` (`pruneTail`, ~141+)
- **Detail:** `pruneTail` caps iterations (`maxPruneIterations`). Under backlog,
  old entries remain and get skipped by `PopOldest` without removal → growing
  dead list data.
- **Fix direction:** make prune resumable / track a low-water cursor so a long
  backlog eventually fully drains.

---

## Notes

- The auth/OAuth cluster (5 items) is the highest-value batch and should be one
  focused PR with concurrent-login + state-validation tests.
- The Postgres-wipe item is the scariest data-loss risk; do it before enabling
  `PGSTORE_*` anywhere with disk-only creds.
- Per repo conventions, changes that are *only* in `internal/translator/` need
  the WRITE/MAINTAIN/ADMIN permission check first — none of the above are in
  `internal/translator/`, so normal contribution rules apply.

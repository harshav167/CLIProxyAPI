# Final Manual QA

Status: PASS.

## Passed Gates

```text
go build -o test-output ./cmd/server && rm test-output
go vet ./internal/config ./internal/runtime/executor ./sdk/api/handlers/openai ./internal/api
go test -race ./internal/config ./internal/runtime/executor ./sdk/api/handlers/openai ./internal/api -run '<focused transport patterns>' -count=1
go test ./internal/config ./internal/runtime/executor ./sdk/api/handlers/openai -count=1
go test -race ./internal/api -count=1
go test ./... -count=1
```

Results: build passed; vet passed; 34 focused transport race tests passed; 748 affected-package tests passed; all 40 API tests passed under the race detector; all 3141 repository tests passed across 104 packages.

Local `httptest` coverage proves upstream WS success, 55-minute age rollover, connection-limit retry and HTTP fallback, generic pre-output close fallback, post-output non-retry, passthrough cache recovery, route-level HTTP/SSE framing, terminal failures, and non-stream aggregation. No production endpoint or credential was used.

## Catalog Baseline Correction

The first full-suite run exposed a stale test expectation in `TestModelsWithClientVersionReturnsCodexCatalog`. `applyCodexClientNonTemplatePriorities` intentionally assigns `max(template priority) + 100`; commit `93b0a8fe` raised the embedded maximum to 43, while the older assertion introduced in `9a8098d2` still expected 129. The test now expresses the current 43 + 100 contract with named constants. No model metadata or runtime behavior changed. The isolated test, API race suite, and full repository suite all pass after the correction.

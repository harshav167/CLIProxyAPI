# SigNoz observability

CLIProxyAPI can export OpenTelemetry traces, metrics, and logs to the local SigNoz stack.

## Local SigNoz stack

Use the cloned SigNoz repository and the local port override:

```bash
cd ~/Developer/signoz/signoz/deploy/docker
docker compose -f docker-compose.yaml -f docker-compose.local-57000.yaml up -d
```

Local endpoints:

- UI: `http://localhost:57080`
- OTLP/gRPC: `localhost:57017`
- OTLP/HTTP: `http://localhost:57018`
- Collector health: `http://localhost:57133`
- Collector pprof: `http://localhost:57177`

The upstream compose file is unchanged. `docker-compose.local-57000.yaml` only remaps host-facing ports.

## Cloudflare Tunnel

Do not publish SigNoz through Cloudflare Tunnel yet. If this changes, use one hostname with path routing:

- `^/v1/(traces|logs|metrics)$` -> `http://127.0.0.1:57018`
- all other paths -> `http://127.0.0.1:57080`

That keeps the SigNoz UI and OTLP/HTTP ingest on one Cloudflare Tunnel hostname. Keep OTLP/gRPC local-only because this setup does not support public hostname routing for gRPC.

## CLIProxyAPI config

```yaml
observability:
  enabled: true
  service-name: cliproxy
  environment: local
  transport-logs: true
  transport-logs-full-body: false
  otlp:
    endpoint: http://localhost:57018
    protocol: http/protobuf
    headers: {}
    insecure: true
    traces: true
    metrics: true
    logs: true
    sample-ratio: 1.0
```

For tunneled ingest, set:

```yaml
observability:
  enabled: true
  otlp:
    endpoint: https://kaecilius.ecorp.cc
    insecure: false
```

Environment overrides:

- `CLIPROXY_OBSERVABILITY_ENABLED=true`
- `CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS=true`
- `CLIPROXY_OBSERVABILITY_TRANSPORT_LOGS_FULL_BODY=false`
- `OTEL_SERVICE_NAME=cliproxy`
- `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:57018`
- `OTEL_EXPORTER_OTLP_HEADERS=key=value`
- `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=local`

Sensitive header values such as authorization, tokens, API keys, passwords, and cookies are redacted before they are stored in runtime settings or log attributes.
`transport-logs` emits one redacted per-request summary with model, status, latency, token, and cache-control metadata. It does not include prompt or completion text. `transport-logs-full-body` is opt-in, redacted, size-bounded, and should stay disabled for shared or tunnel-exposed SigNoz instances.

## Local SigNoz MCP

The project Cursor config already points at a local HTTP MCP server:

```json
{
  "mcpServers": {
    "signoz": {
      "url": "http://127.0.0.1:57000/mcp"
    }
  }
}
```

Run the MCP server against the local SigNoz instance after creating an API key in the SigNoz UI (`http://localhost:57080`, Settings -> API Keys):

```bash
cd ~/Developer/signoz/signoz-mcp-server
SIGNOZ_URL=http://localhost:57080 \
SIGNOZ_API_KEY=<local-api-key> \
TRANSPORT_MODE=http \
MCP_SERVER_PORT=57000 \
./bin/signoz-mcp-server
```

Build the binary first if needed:

```bash
cd ~/Developer/signoz/signoz-mcp-server
make build
```

## Signals

Traces:

- Inbound HTTP spans for API, management, OAuth callback, health, websocket, and provider routes.
- Outbound upstream provider spans with provider, model, auth type, auth index, and API-key fingerprint attributes.
- Usage events attached to request spans with token counts and TTFT.

Metrics:

- `cliproxy.http.server.requests`
- `cliproxy.http.server.duration_ms`
- `cliproxy.http.server.active_requests`
- `cliproxy.streams.active`
- `cliproxy.upstream.requests`
- `cliproxy.upstream.ttft_ms`
- `cliproxy.tokens.input`
- `cliproxy.tokens.output`
- `cliproxy.tokens.total`
- `cliproxy.tokens.cache_read`
- `cliproxy.tokens.cache_creation`
- `cliproxy.websocket.connections`
- `cliproxy.websocket.disconnects`
- `cliproxy.config.reloads`
- `cliproxy.auth.refreshes`
- Go runtime metrics from `go.opentelemetry.io/contrib/instrumentation/runtime`

Logs:

- Logrus entries are mirrored into OpenTelemetry logs when log export is enabled.
- Redacted transport summaries are emitted as `body = "cliproxy.transport_summary"` when `observability.transport-logs` is enabled.
- Existing stdout/file logging behavior is unchanged.

## Dashboard starters

Import the upstream SigNoz templates first:

- `~/Developer/signoz/dashboards/go-runtime/go-runtime-metrics.json`
- `~/Developer/signoz/dashboards/openai/openai-dashboard.json`
- `~/Developer/signoz/dashboards/anthropic/anthropic-dashboard.json`
- `~/Developer/signoz/dashboards/google-gemini/google-gemini-template.json`
- `~/Developer/signoz/dashboards/codex/codex-dashboard.json`

Then create the cache-burn dashboard using `docs/signoz-dashboard-cliproxy.json` as the panel checklist.

Cache-burn acceptance panels:

- Cache hit ratio: `sum(cliproxy.tokens.cache_read) / sum(cliproxy.tokens.input + cliproxy.tokens.cache_read + cliproxy.tokens.cache_creation)`.
- Cache creation volume by `cliproxy.model`, `cliproxy.api_key_fingerprint`, and `cliproxy.conversation_id`.
- Top conversations by `gen_ai.usage.cache_creation_input_tokens` from `cliproxy.transport_summary` logs.
- Per-request scatter/table for input vs cache read vs cache creation, with `cliproxy.cache_control_summary`.
- Supporting request rate, error rate, and p95/p99 TTFT panels.

## Alert starters

Create SigNoz alert rules for:

- High upstream failure rate: `cliproxy.upstream.requests` grouped by `cliproxy.provider`, `cliproxy.model`, and `cliproxy.failed`.
- High inbound 5xx rate: `cliproxy.http.server.requests` grouped by `http.response.status_code`.
- High p95 upstream TTFT: `cliproxy.upstream.ttft_ms`.
- Active stream saturation: `cliproxy.streams.active`.
- Auth refresh failures: `cliproxy.auth.refreshes` where `cliproxy.success=false`.
- Config reload failures: `cliproxy.config.reloads` where `cliproxy.success=false`.

Keep alert routing out of repository config; notification channels are environment-specific SigNoz state.

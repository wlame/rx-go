# Webhooks

`rx` can fire outbound HTTP POSTs on three events during a trace
request. Webhooks are fire-and-forget (no retry, no acknowledgment)
and protected by SSRF validation.

## Events

| Event | Fires | Typical use |
|---|---|---|
| `on_file` | Once per file, after its scan finishes | Progress tracking in long scans |
| `on_match` | Once per match (requires `max_results`) | Real-time alerting |
| `on_complete` | Once per trace request | Completion signaling, persistence |

## Configuration

### Process-wide (environment variables)

Set these before starting `rx serve` (or before running CLI commands
with hooks):

```bash
export RX_HOOK_ON_FILE_URL=https://example.com/rx/file
export RX_HOOK_ON_MATCH_URL=https://example.com/rx/match
export RX_HOOK_ON_COMPLETE_URL=https://example.com/rx/complete

rx serve
```

### Per-request override (HTTP)

Query parameters on `GET /v1/trace` override the env values:

```text
GET /v1/trace?path=/var/log/app.log&regexp=error
             &hook_on_complete=https://other.example/notify
```

### Per-request override (CLI)

Flags on `rx trace`:

```bash
rx trace "error" /var/log/app.log \
    --hook-on-complete=https://other.example/notify \
    --max-results=100
```

### Disabling per-request overrides

Set `RX_DISABLE_CUSTOM_HOOKS=true` and only env-configured URLs
will fire. Query-param and CLI-flag overrides are silently ignored.

## Payload shapes

All payloads are POSTed as JSON. Content-Type is
`application/json; charset=utf-8`. Every payload includes
`request_id` for correlation with server logs.

### `on_file`

```json
{
  "event":      "on_file",
  "request_id": "01936c8e-7b2a-7000-8000-000000000001",
  "path":       "/var/log/app-2026-03.log",
  "matches":    17,
  "bytes_scanned": 582137856,
  "duration_ms": 1234
}
```

### `on_match`

```json
{
  "event":      "on_match",
  "request_id": "01936c8e-7b2a-7000-8000-000000000001",
  "path":       "/var/log/app-2026-03.log",
  "pattern":    "timeout",
  "offset":     1024581,
  "absolute_line_number": 9812,
  "line_text":  "2026-03-14 08:23:51 ERROR timeout 5023ms"
}
```

### `on_complete`

```json
{
  "event":      "on_complete",
  "request_id": "01936c8e-7b2a-7000-8000-000000000001",
  "path":       ["/var/log/app-2026-03.log"],
  "patterns":   ["timeout"],
  "total_matches":    17,
  "scanned_files":    1,
  "skipped_files":    0,
  "duration_seconds": 2.341
}
```

## Security: SSRF protection

Webhook URLs are validated before the first POST. By default, these
are **rejected**:

| Address space | Example | Why |
|---|---|---|
| Loopback | `127.0.0.1`, `::1`, `localhost` | Local services |
| Link-local | `169.254.169.254` | AWS/GCP IMDS (credential theft) |
| RFC 1918 private | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | Internal networks |
| CGNAT | `100.64.0.0/10` | Carrier-grade NAT |
| Unspecified | `0.0.0.0`, `::` | Locally-routed |

The validation runs at two points:

1. **Static check** — if the host is an IP literal or the string
   `"localhost"`, the address space is checked directly
2. **DNS resolution check** — for hostnames, `rx` resolves the name
   (2-second timeout) and checks every returned IP against the same
   address-space rules

A DNS failure is a **soft-accept** — better to let a DNS blip through
than to false-positive during a transient outage. The POST will fail
naturally if the host is truly unreachable.

### Overrides

| Variable | Effect |
|---|---|
| `RX_ALLOW_INTERNAL_HOOKS=true` | Bypass all SSRF checks. Use only when you actually need internal destinations (e.g. an internal logging service). |
| `RX_HOOK_STRICT_IP_ONLY=true` | Reject **any** hostname, accept only IP literals. Strongest defense against DNS rebinding attacks. Operators must maintain IP allowlists. |

### Known limitations

- **DNS rebinding** is not mitigated: an attacker who controls DNS can
  resolve to a public IP at validation time and to an internal IP at
  POST time. Use `RX_HOOK_STRICT_IP_ONLY=true` if this matters in your
  threat model.

## Failure handling

- Each POST has a **3-second timeout**. Longer responses are canceled.
- Failures are logged at Warn level and bump the
  `rx_hook_calls_total{status="failure"}` metric.
- **No retry.** A single failed POST is the final attempt for that
  event.
- If the webhook queue is full (over 512 pending events), new events
  are **dropped** with a warning log. The queue backs up when the
  webhook endpoint is slower than the scan rate.

## Dispatch internals

- One process-wide dispatcher with a buffered channel (depth 512) and
  a pool of 8 worker goroutines
- Events enqueue fast (non-blocking on a live queue) and dispatch
  asynchronously
- The trace engine never waits for webhook responses — fire-and-forget
- On graceful shutdown, the dispatcher drains the queue before the
  server exits

## Operational guidance

### `on_match` + `max_results` is mandatory

```bash
# This fails with 400.
rx trace "error" /var/log/app.log --hook-on-match=https://example.com/...

# This works.
rx trace "error" /var/log/app.log --hook-on-match=https://example.com/... --max-results=100
```

Without the cap, a scan with 1 million matches would fire 1 million
POSTs — a DoS on your own webhook endpoint.

### Hook endpoints should respond fast

The 3-second timeout is per-event. If your webhook does synchronous
work (DB writes, downstream API calls), you'll saturate the 8-worker
pool quickly. Accept the payload, queue it for async processing,
respond `202 Accepted` immediately.

### Use request_id for correlation

Every webhook payload includes the `request_id` that triggered it.
Match it against `X-Request-ID` response headers and server-side
slog records to trace a request end-to-end.

### Disable overrides in multi-tenant deployments

If multiple clients share one `rx serve`, they can probe each other's
internal services by passing malicious hook URLs. Combine:

- `RX_DISABLE_CUSTOM_HOOKS=true` — ignore per-request overrides
- Explicit env-configured URLs — all traffic goes to operator-controlled
  endpoints

## Monitoring

```promql
# Webhook POST rate.
sum by (kind) (rate(rx_hook_calls_total[5m]))

# Failure rate.
sum by (kind) (rate(rx_hook_calls_total{status="failure"}[5m]))

# p95 latency per kind.
histogram_quantile(0.95,
  sum by (kind, le) (rate(rx_hook_call_duration_seconds_bucket[5m])))
```

## See also

- [`/v1/trace`](endpoints/trace.md) — the endpoint that fires webhooks
- [Configuration](../configuration.md) — all `RX_HOOK_*` env vars
- [concepts/security](../concepts/security.md) — full security posture
- [Metrics](endpoints/metrics.md) — webhook metric families

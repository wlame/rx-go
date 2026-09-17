# Webhooks

`rx` can call a URL of yours on three events during a trace. Each call
is one HTTP `GET` with the payload in the query string and no request
body. Webhooks are fire-and-forget (no retry, no acknowledgment) and
protected by SSRF validation.

The same protocol is used whether the trace runs as `rx trace` or as
`GET /v1/trace`, and rx-python sends the same calls, so one webhook
target serves both backends.

## Events

| Hook | `event` value | Fires | Typical use |
|---|---|---|---|
| `on_file` | `file_scanned` | Once per file, after its scan finishes (with several patterns, once its lines' patterns are decided, at most 64 files behind the scan) | Progress tracking in long scans |
| `on_match` | `match_found` | Once per match of the response, after the scan (requires `max_results`) | Alerting on the matches found |
| `on_complete` | `trace_complete` | Once per trace request | Completion signaling, persistence |

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

### Per-request (HTTP)

Query parameters on `GET /v1/trace` set the URLs for that request
alone:

```text
GET /v1/trace?path=/var/log/app.log&regexp=error
             &hook_on_complete=https://other.example/notify
```

Concurrent requests do not share URLs: each request's events go to the
URLs it resolved, and carry its `request_id`.

### Per-invocation (CLI)

Flags on `rx trace`:

```bash
rx trace "error" /var/log/app.log \
    --hook-on-complete=https://other.example/notify \
    --max-results=100
```

### Precedence

For each of the three hooks, separately:

1. When `RX_DISABLE_CUSTOM_HOOKS` is set, the environment URL is used
   and the query parameter or flag is ignored.
2. Otherwise a URL given on the request (`hook_on_*`) or on the command
   line (`--hook-on-*`) wins over the environment URL.
3. Otherwise the environment URL is used, if there is one.

So with `RX_HOOK_ON_COMPLETE_URL` set, a request that passes its own
`hook_on_complete` is notified at its own URL only, and a request that
passes none is notified at the environment URL.

## Payloads

Every call is a `GET` to the configured URL with the payload appended as
query parameters (after `&` when the URL already has a query). Every
value is a string in the query; numbers are written in decimal. Every
payload carries `event` and `request_id`.

### `file_scanned` (`on_file`)

| Parameter | Meaning |
|---|---|
| `event` | `file_scanned` |
| `request_id` | The trace's request ID |
| `file_path` | Path of the scanned file |
| `file_size_bytes` | Size of the file on disk, in bytes |
| `scan_time_ms` | Time spent on this file, in milliseconds |
| `matches_count` | Matches found in this file |

```text
GET https://example.com/rx/file?event=file_scanned
    &file_path=%2Fvar%2Flog%2Fapp.log&file_size_bytes=582137856
    &matches_count=17&request_id=01936c8e-7b2a-7000-8000-000000000001
    &scan_time_ms=1234
```

### `match_found` (`on_match`)

| Parameter | Meaning |
|---|---|
| `event` | `match_found` |
| `request_id` | The trace's request ID |
| `file_path` | Path of the file that holds the match |
| `pattern` | The pattern that matched, as given in the request |
| `offset` | Byte offset of the matched line in the file's text (the decompressed stream for a compressed file) |
| `line_number` | The line's 1-based number in the file, or `-1` when it is unknown |

The calls describe the matches of the response and nothing else: one
call per match, sent once the trace has numbered the matches and cut
them to `max_results`, in the response's order. Each call's
`line_number` is that match's `absolute_line_number` in the response.
It is `-1` where the response has `-1`: a scan of a plain file that the
`max_results` cap cut short may not have read the lines before a match,
and rx does not read them just to number it (see
[`/v1/trace`](endpoints/trace.md)). `offset` is always sent, so
`rx samples --offsets=<offset>` resolves such a line. A number counted
from the start of a chunk is never sent.

Because the calls follow the numbering, `match_found` calls of a trace
go out after its `file_scanned` calls, and the dispatcher's 8 workers
may deliver them in any order.

```text
GET https://example.com/rx/match?event=match_found
    &file_path=%2Fvar%2Flog%2Fapp.log&line_number=9812&offset=1024581
    &pattern=timeout&request_id=01936c8e-7b2a-7000-8000-000000000001
```

### `trace_complete` (`on_complete`)

| Parameter | Meaning |
|---|---|
| `event` | `trace_complete` |
| `request_id` | The trace's request ID |
| `paths` | The requested paths, joined with `,` |
| `patterns` | The patterns, joined with `,` |
| `total_files_scanned` | Files the trace searched |
| `total_files_skipped` | Files it skipped (binary, unreadable) |
| `total_matches` | Matches in the response |
| `total_time_ms` | Duration of the trace, in milliseconds |

```text
GET https://example.com/rx/complete?event=trace_complete
    &paths=%2Fvar%2Flog%2Fapp.log&patterns=timeout
    &request_id=01936c8e-7b2a-7000-8000-000000000001
    &total_files_scanned=1&total_files_skipped=0&total_matches=17
    &total_time_ms=2341
```

## Security: SSRF protection

Webhook URLs are validated before the first call. By default, these
are **rejected**:

| Address space | Example | Why |
|---|---|---|
| Loopback | `127.0.0.1`, `::1`, `localhost` | Local services |
| Link-local | `169.254.169.254` | AWS/GCP IMDS (credential theft) |
| RFC 1918 private | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | Internal networks |
| CGNAT | `100.64.0.0/10` | Carrier-grade NAT |
| Multicast | `224.0.0.0/4`, `ff00::/8` | Group addressing |
| Unspecified | `0.0.0.0`, `::` | Locally-routed |

A URL that carries credentials (`http://user:pass@host/`) is rejected as
well, whatever address it points at. `RX_ALLOW_INTERNAL_HOOKS` does not
switch that rule off.

A rejected URL is refused before the trace runs: `GET /v1/trace`
answers `400`, `rx trace` exits `2`, and `rx serve` refuses to start
when an `RX_HOOK_ON_*_URL` is rejected.

The validation runs at three points:

1. **Static check** — if the host is an IP literal or the string
   `"localhost"`, the address space is checked directly
2. **DNS resolution check** — for hostnames, `rx` resolves the name
   (2-second timeout) and checks every returned IP against the same
   address-space rules
3. **Connect-time check** — the HTTP client's dialer applies the same
   rules to the literal IP it is about to connect to, which is what
   defeats DNS rebinding; see
   [concepts/security](../concepts/security.md#dns-rebinding-is-checked-at-connect-time)

A DNS failure at the second point is a **soft-accept** — better to let
a DNS blip through than to false-positive during a transient outage.
The call will fail naturally if the host is truly unreachable, and the
connect-time check still applies.

### Overrides

| Variable | Effect |
|---|---|
| `RX_ALLOW_INTERNAL_HOOKS=true` | Bypass all SSRF checks. Use only when you actually need internal destinations (e.g. an internal logging service). |
| `RX_HOOK_STRICT_IP_ONLY=true` | Reject **any** hostname, accept only IP literals. No name is resolved at all. Operators must maintain IP allowlists. |

### Redirects are never followed

Validation only sees the configured URL, so a target that answers a
`3xx` could otherwise redirect the request to any host — including the
ones in the table above. The dispatcher refuses every redirect, to a
public host as much as an internal one. The hop is logged at debug
level as `hook_redirect_refused`, the `3xx` counts as a non-2xx
response, and the hook is recorded as a failure. Point the hook at the
final URL instead of one that redirects.

## Failure handling

- Each call has a **3-second timeout**. Longer responses are canceled.
- A response outside `2xx`, a timeout or a connection error is logged
  at Warn level as `hook_failed` and bumps
  `rx_hook_calls_total{status="failure"}`.
- **No retry.** A single failed call is the final attempt for that
  event.
- If the webhook queue is full (512 pending events), new events are
  **dropped** with a `hook_queue_full_dropped` warning and
  `rx_hook_calls_total{status="dropped"}`. The queue backs up when the
  webhook endpoint is slower than the scan rate.

## Dispatch internals

- One dispatcher per process — per `rx serve`, or per `rx trace`
  invocation — with a buffered channel (depth 512), one HTTP client and
  a pool of 8 worker goroutines
- The dispatcher holds no URLs: each trace request hands it events
  already addressed to that request's URLs and stamped with its
  `request_id`, so concurrent requests share the queue and nothing else
- Events enqueue fast (non-blocking on a live queue) and dispatch
  asynchronously
- The trace engine never waits for webhook responses — fire-and-forget
- On graceful shutdown, and before `rx trace` exits, the dispatcher
  drains the queue

## Operational guidance

### `on_match` + `max_results` is mandatory

```bash
# This exits 2 (usage error); over HTTP the request gets a 400.
rx trace "error" /var/log/app.log --hook-on-match=https://example.com/...

# This works.
rx trace "error" /var/log/app.log --hook-on-match=https://example.com/... --max-results=100
```

Without the cap, a scan with 1 million matches would make 1 million
calls — a DoS on your own webhook endpoint.

### Hook endpoints should respond fast

The 3-second timeout is per-event. If your webhook does synchronous
work (DB writes, downstream API calls), you'll saturate the 8-worker
pool quickly. Accept the call, queue it for async processing, and
respond `202 Accepted` or `204 No Content` immediately.

### Use request_id for correlation

Every payload's `request_id` is the `request_id` of the trace response
that caused it. Pass your own with the `request_id` query parameter of
`GET /v1/trace` (or `--request-id=…` on `rx trace`) to know it before
the response arrives; otherwise `rx` generates a UUID v7. It is not the
`X-Request-ID` header, which the HTTP layer assigns to every request
for its logs.

### Disable overrides where several people share a server

If several people share one `rx serve`, any of them can point a hook at
a URL of their choice. Combine:

- `RX_DISABLE_CUSTOM_HOOKS=true` — ignore per-request URLs
- Explicit env-configured URLs — all traffic goes to operator-controlled
  endpoints

## Monitoring

```promql
# Webhook call rate.
sum by (kind) (rate(rx_hook_calls_total[5m]))

# Failure rate.
sum by (kind) (rate(rx_hook_calls_total{status="failure"}[5m]))

# Events dropped because the queue was full.
sum by (kind) (rate(rx_hook_calls_total{status="dropped"}[5m]))

# p95 latency per kind.
histogram_quantile(0.95,
  sum by (kind, le) (rate(rx_hook_call_duration_seconds_bucket[5m])))
```

## See also

- [`/v1/trace`](endpoints/trace.md) — the endpoint that fires webhooks
- [Configuration](../configuration.md) — all `RX_HOOK_*` env vars
- [concepts/security](../concepts/security.md) — full security posture
- [Metrics](endpoints/metrics.md) — webhook metric families

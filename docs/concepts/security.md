# Security

`rx` has three main security surfaces:

- **The path sandbox** — `--search-root` prevents reading arbitrary
  files
- **Tarball extraction** — the `rx-viewer` SPA is downloaded from
  GitHub and extracted; extraction rejects traversal attacks
- **Webhook SSRF protection** — outbound webhook URLs are validated
  to prevent probing internal services

Each is described below. At the end, we cover the threat model and
known limitations.

## Path sandbox (`--search-root`)

### What it does

Every endpoint and CLI command that accepts a file path validates it
against a configured list of **search roots**:

```bash
rx serve --search-root=/var/log --search-root=/var/data/exports
```

Any path that resolves outside all configured roots is rejected with
`403 path_outside_search_root` (HTTP) or exit code 4 (CLI).

### How validation works

For each user-supplied path:

1. Resolve to absolute form via `filepath.Abs`
2. Follow symlinks via `filepath.EvalSymlinks`
3. Check the canonical form against each configured root's canonical
   form
4. Accept iff the path equals a root or is prefixed by `<root>/`
5. Reject otherwise

The prefix check uses the OS path separator, so `/var/logdata` is NOT
considered inside `/var/log` — a common false-positive trap.

### When validation runs

- `rx serve`: on every file-accepting endpoint
- CLI: on every path argument when `RX_SEARCH_ROOTS` is set or roots
  have been configured

When no roots are configured (typical CLI default), no validation
runs. The sandbox is opt-in for CLI and on-by-default for
`rx serve` (which defaults to the current working directory as the
sole root).

### Symlink behavior

Symlinks are **followed once** at startup. If you configure
`--search-root=/srv/logs` and `/srv/logs` is a symlink to `/data/logs`,
the stored canonical root is `/data/logs`. Later path checks compare
against `/data/logs`.

This means:

- A symlink swap after startup doesn't change the sandbox (restart to
  pick it up)
- Symlinks inside the sandbox that point outside are rejected (the
  `EvalSymlinks` step sees the escape)

### Failure modes

- **Configured root doesn't exist**: `rx serve` refuses to start with
  a clear error
- **Configured root is not a directory**: same
- **Empty `--search-root` value**: rejected as a config error

### Examples

```bash
# Server bound to /var/log only.
rx serve --search-root=/var/log

# These succeed:
curl "http://localhost:7777/v1/trace?path=/var/log/app.log&regexp=error"
curl "http://localhost:7777/v1/trace?path=/var/log/nginx/access.log&regexp=500"

# This returns 403:
curl "http://localhost:7777/v1/trace?path=/etc/passwd&regexp=root"
```

## Tarball extraction defenses

`rx serve` downloads the `rx-viewer` SPA from GitHub on first start
and extracts it to `~/.cache/rx/frontend/`. Tarball extraction
anywhere is a traditional source of "zip slip" vulnerabilities —
malicious archives with entries like `../../etc/passwd`.

`rx`'s tarball extractor rejects any entry whose resolved path would
escape the target directory:

1. The target path is resolved to its canonical absolute form
2. For each tarball entry, the proposed destination is computed
3. `filepath.Clean` is applied to normalize `..` and `.` segments
4. The cleaned destination must still start with the canonical target
   prefix; if not, the extraction aborts with an error

This catches:

- `..` traversal (`../etc/passwd`)
- Absolute-path entries (`/etc/passwd`)
- Symlinks that point outside the target
- Long-chain traversal (`foo/../../../bar/../etc/passwd`)

A malformed tarball causes the extraction to abort entirely — no
partial writes leak through. The in-flight temp file is deleted.

Test coverage includes malicious-tarball fixtures.

## Webhook SSRF protection

### The threat

`rx` fires outbound HTTP POSTs on trace events when configured via
`hook_on_*` query params or the corresponding env vars. If the
webhook URL is user-controlled (e.g. a per-request query param in a
shared `rx serve` instance), a malicious user could:

- Probe internal services by setting `hook_on_match=http://10.0.0.5/`
- Read cloud IAM credentials via
  `hook_on_match=http://169.254.169.254/latest/meta-data/iam/security-credentials/`

This is a classic **SSRF** (server-side request forgery) attack.

### The defenses

When validating a hook URL, `rx` rejects:

| Address space | Example | Reason |
|---|---|---|
| Loopback | `127.0.0.1`, `::1`, literal `"localhost"` | Local services |
| Link-local | `169.254.0.0/16`, `fe80::/10` | Cloud IMDS |
| RFC 1918 private | `10/8`, `172.16/12`, `192.168/16`, `fc00::/7` | Internal networks |
| CGNAT (RFC 6598) | `100.64.0.0/10` | Carrier-grade NAT |
| Unspecified | `0.0.0.0`, `::` | Locally-routed |

Validation runs at two layers:

1. **Static check**: if the URL's host is an IP literal or the string
   `"localhost"`, it's checked directly against the above ranges
2. **DNS resolution check**: for hostname URLs, `rx` resolves the
   name (2-second timeout) and rejects if **any** returned IP falls
   in a blocked range

A DNS failure is a **soft-accept** — a transient resolver outage
shouldn't false-positive-reject every validation.

### Overrides

| Variable | Effect |
|---|---|
| `RX_ALLOW_INTERNAL_HOOKS=true` | Bypass all SSRF checks. Use only when you explicitly need an internal destination. |
| `RX_HOOK_STRICT_IP_ONLY=true` | Reject every hostname; accept only IP literals. Strongest mitigation against DNS rebinding. |

### Known limitation: DNS rebinding

The DNS-resolution check runs **once** at validation time. An
attacker who controls DNS can:

1. Configure `evil.example.com` to resolve to a public IP initially
2. Pass a hook URL for `evil.example.com` to `rx`
3. The validation step resolves the public IP — accepted
4. Between validation and the POST, DNS is changed to resolve to
   an internal IP
5. The POST hits the internal IP

Mitigations:

- `RX_HOOK_STRICT_IP_ONLY=true` — force IP-literal URLs only
- Deploy `rx serve` with no outbound access to internal networks
  (firewall egress)
- Run `rx serve` in a dedicated namespace with restricted routing

A future release may add DNS re-resolution at POST time as a
defense-in-depth measure.

## Threat model

### Things `rx` defends against

- Arbitrary-file-read via `/v1/trace?path=/etc/passwd` — blocked by
  `--search-root`
- Zip-slip / tar-slip in the SPA cache — blocked by extractor
  validation
- SSRF to internal services via hook URLs — blocked by address-range
  check + DNS resolution

### Things `rx` does NOT defend against

- **Auth** — `rx serve` has no built-in authentication. Anyone who
  can reach the socket can run any operation within the sandbox. Put
  it behind a reverse proxy with auth.
- **DoS** — no built-in rate limiting. A single client can
  simultaneously launch N traces and exhaust CPU. Use a reverse
  proxy or a process supervisor that caps concurrent requests.
- **DNS rebinding** — the SSRF guard is vulnerable to DNS rebinding.
  Use `RX_HOOK_STRICT_IP_ONLY` or firewall egress.
- **Cache poisoning** — multi-user cache directories share entries;
  a user who writes a bad cache entry affects other users. Use
  per-user `RX_CACHE_DIR`.
- **Regex DoS** — user-supplied regex patterns are compiled by
  `ripgrep`. `ripgrep` uses the Rust `regex` crate, which rejects
  exponential backtracking patterns by design. Still, a bad regex
  against a huge file can run for a long time. Use `--max-results`
  and reasonable timeouts.

## Deployment recommendations

For production:

1. **Set multiple specific `--search-root` values** rather than one
   broad root
2. **Front `rx serve` with a reverse proxy** that handles TLS, auth,
   and rate limiting
3. **Disable per-request hook overrides** in multi-tenant settings:
   `RX_DISABLE_CUSTOM_HOOKS=true`
4. **Enable `RX_HOOK_STRICT_IP_ONLY=true`** if webhook destinations
   are internal and DNS rebinding is in the threat model
5. **Monitor `/metrics`** — `rx_errors_total{error_type="permission_denied"}`
   and `rx_hook_calls_total{status="failure"}` flag misbehavior
6. **Use separate per-user `RX_CACHE_DIR`** if multiple users share
   a host

## Related concepts

- [`rx serve`](../cli/serve.md) — sandbox configuration
- [Webhooks](../api/webhooks.md) — full webhook behavior
- [Configuration](../configuration.md) — every `RX_*` env var
- [Caching](caching.md) — cache isolation

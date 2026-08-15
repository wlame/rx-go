# Security

## Intended use, first

**`rx` is built for internal use on a trusted network. It is not intended
to be exposed to the internet.**

`rx serve` has no authentication. Anyone who can reach the port can read
any file under `--search-root`, and `--search-root` defaults to the
current directory. There is no TLS and no multi-tenancy.

That is a deliberate scope decision, and it divides this page in two.
**Building the perimeter is the operator's job** — bind to loopback,
reach it over a VPN or an SSH tunnel, or front it with an authenticating
reverse proxy. Identity, RBAC, sessions and certificates belong there,
not in `rx`.

**What `rx` takes responsibility for is hygiene inside that perimeter**:
that a path outside the sandbox is refused, that a webhook cannot be
turned into a probe of your internal network, and that the viewer bundle
it downloads is the one it expected. Those are the surfaces below.

## The three surfaces

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

### Write paths are validated too

The sandbox covers destinations, not only sources. `POST /v1/compress`
and `rx compress` validate the effective output path — `output_path` (or
`--output`/`--output-dir` on the CLI) when given, otherwise the derived
`<input>.zst` — before the encoder opens anything. A rejected output path
produces `403` (HTTP) or an access-denied error (CLI) and leaves the
filesystem untouched: nothing is created, truncated or removed, and
`force` does not relax the check.

Without this, an unauthenticated `rx serve` would expose an
arbitrary-file-write primitive to any client that can reach the socket.

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

## Hidden files and directories

Entries whose name starts with a dot are **not served by default**, in
either backend.

The case this exists for: `rx serve` with no `--search-root` serves the
current directory. Started from a home directory, that includes `~/.ssh`,
`~/.aws` and `~/.gnupg` — readable through `/v1/samples` by anyone who
can reach the port. The sandbox was working exactly as designed; the
default root was the problem.

The rule matches ripgrep's, which rx users already know:

```bash
rx "token" ~/                 # skips ~/.ssh, ~/.aws, ~/.bashrc
rx --hidden "token" ~/        # includes them
RX_HIDDEN=true rx "token" ~/  # same, from the environment
```

### It is enforced in the sandbox, not the listing

Omitting hidden entries from a directory listing hides them from someone
browsing. It does nothing about a caller who already knows the path and
asks for the file directly. So the check lives in the path validator that
every endpoint and every CLI command already goes through:

```bash
# Hidden entries do not appear in the tree...
curl -s "$RX/v1/tree?path=/home/u" | jq '.entries[].name'
# ["logs", "data"]

# ...and asking for one by name is refused, not merely undocumented.
curl -si "$RX/v1/samples?path=/home/u/.ssh/id_rsa&lines=1" | head -1
# HTTP/1.1 403 Forbidden
```

The refusal names the rule and the way out, because "access denied" alone
sends people looking for a permissions problem they do not have:

```json
{
  "detail": "Access denied: path '/home/u/.ssh/id_rsa' contains hidden component '.ssh'; pass --hidden (or set RX_HIDDEN=true) to include hidden files and directories"
}
```

It is a different error type from the outside-the-sandbox refusal
(`ErrHiddenPath` in Go, `HiddenPathError` in Python), so a client can
tell "not yours to read" from "pass `--hidden` if you meant it". Both are
403.

### Components of the root itself are exempt

`--search-root=~/.local/share/logs` is a deliberate choice. Only
components *below* a root are subject to the rule — refusing to serve the
directory you were pointed at would be absurd:

```bash
rx serve --search-root=/home/u/.local/share/logs   # works, no --hidden needed
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

## Viewer bundle integrity

The bundle is unpacked into the cache and served to a browser from the rx
origin, so what it contains matters as much as where it lands.

### The bundle is verified before it is unpacked

Every release publishes `dist.tar.gz.sha256` beside the asset. After the
download, `rx` fetches that sidecar and checks the digest:

| Sidecar | Result |
|---|---|
| Present and matching | Extracted; the digest is stored in `.metadata.json` |
| Present and not matching | Refused. Nothing is unpacked, the cached bundle is untouched, and the failure is logged as `frontend_checksum_mismatch` |
| Absent (HTTP 404) | Accepted with a `frontend_checksum_missing` warning — releases published before the sidecar existed are still installable |
| Present but unreadable | Refused. A release host answering with something that is not a digest is broken, not sidecar-less |

rx-python behaves identically.

### A failed download never costs the working bundle

The tarball streams to a temp file, is verified, and is unpacked into a
`.staging` directory. Only when all of that succeeds is the previous
bundle replaced. A truncated download, a bad digest or a malformed archive
leaves the last good bundle exactly where it was.

### "Latest" is bounded

`rx serve` resolves the newest viewer release, but installs it only when
its version falls inside the range this backend was built against —
`0.2.0 <= v < 0.3.0` today (the viewer is a 0.x product, where a minor bump
may break compatibility). A newer release is logged as
`frontend_version_incompatible` and skipped; the server runs without the
SPA rather than serving a viewer that expects fields this backend does not
send.

`RX_FRONTEND_VERSION=v0.2.0` pins an exact tag and bypasses the range
check. `RX_FRONTEND_URL` bypasses release resolution entirely. Both are
for an operator who knows what they are doing.

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
| Multicast | `224.0.0.0/4`, `ff00::/8` | Group addressing |
| Unspecified | `0.0.0.0`, `::` | Locally-routed |

A URL that carries credentials (`http://user:pass@host/`) is rejected
too, whatever it points at: those end up in proxy and access logs, and
`RX_ALLOW_INTERNAL_HOOKS` does not switch that rule off.

Validation runs at three layers, the last of which is the one that
cannot be raced:

1. **Static check**: if the URL's host is an IP literal or the string
   `"localhost"`, it's checked directly against the above ranges
2. **DNS resolution check**: for hostname URLs, `rx` resolves the
   name (2-second timeout) and rejects if **any** returned IP falls
   in a blocked range
8. **Dial-time check** (rx-go): the same table is applied again to the
   literal IP the HTTP client is about to connect to. See "DNS rebinding
   is checked at connect time" below for why the first two are not enough
   on their own.

A DNS failure is a **soft-accept** — a transient resolver outage
shouldn't false-positive-reject every validation.

### Overrides

| Variable | Effect |
|---|---|
| `RX_ALLOW_INTERNAL_HOOKS=true` | Bypass all SSRF checks. Use only when you explicitly need an internal destination. |
| `RX_HOOK_STRICT_IP_ONLY=true` | Reject every hostname; accept only IP literals. Strongest mitigation against DNS rebinding. |

### Redirects are refused

Validation only ever sees the URL that was configured. A webhook target
that answers `301`/`302` would otherwise steer the request at a host
nobody vetted — including every address in the table above — so the
dispatcher refuses **every** redirect, public or internal. The hop is
logged at debug level (`hook_redirect_refused`) and the hook is counted
as a failure.

The viewer-bundle downloader is the one client that still follows
redirects, because GitHub redirects asset URLs to a CDN. It re-checks
each hop against the same address table and aborts on a hop that points
somewhere internal, keeping the standard 10-hop cap.

### DNS rebinding is checked at connect time

The address check runs twice, and the second time is the one that counts.

Validating a hook URL resolves its hostname and checks the addresses.
That alone is advisory: the HTTP client resolves the name *again* when
the request goes out, and nothing makes the two answers agree. An
attacker who controls DNS for a hostname they can get configured returns
a public address for the first lookup and `127.0.0.1` for the second. The
same thing happens without an attacker whenever a short-TTL record
changes in between.

So the client's dialer re-applies the whole address policy to the literal
IP it is about to connect to. By that point resolution has already
happened and there is no window left for the answer to change.

`RX_ALLOW_INTERNAL_HOOKS=true` is honored there too, so an operator who
deliberately points a hook at a local collector is unaffected.

`RX_HOOK_STRICT_IP_ONLY=true` remains available and is still the
strictest option: it refuses hostname URLs outright, so no resolution
happens at all.

!!! note "rx-python"

    rx-python validates at configuration time only. `httpx` has no
    equivalent of Go's `DialContext` hook, so closing the gap there means
    resolving, checking, and connecting to a pinned IP with the `Host`
    header and SNI set by hand. Until that lands, use
    `RX_HOOK_STRICT_IP_ONLY=true` on the Python backend if DNS rebinding
    is in your threat model.

## Threat model

### Things `rx` defends against

- Arbitrary-file-read via `/v1/trace?path=/etc/passwd` — blocked by
  `--search-root`
- Reading `~/.ssh`, `~/.aws` and friends when `rx serve` was started
  from a home directory — blocked by the hidden-entry rule
- Zip-slip / tar-slip in the SPA cache — blocked by extractor
  validation
- SSRF to internal services via hook URLs — blocked by address-range
  check + DNS resolution

### Things `rx` does NOT defend against

- **Auth** — `rx serve` has no built-in authentication. Anyone who
  can reach the socket can run any operation within the sandbox. This
  is by design; see "Intended use, first" above.
- **DoS** — no built-in rate limiting. A single client can
  simultaneously launch N traces and exhaust CPU. Use a reverse
  proxy or a process supervisor that caps concurrent requests.
- **Exposure to an untrusted network** — there is no authentication
  and no TLS. This is the scope decision at the top of the page, not a
  bug. Put `rx` behind a perimeter.
- **DNS rebinding, on rx-python only** — rx-go re-checks the address at
  connect time; rx-python validates once. Use
  `RX_HOOK_STRICT_IP_ONLY=true` there.
- **Cache poisoning** — multi-user cache directories share entries;
  a user who writes a bad cache entry affects other users. Use
  per-user `RX_CACHE_DIR`.
- **Regex DoS** — user-supplied regex patterns are compiled by
  `ripgrep`. `ripgrep` uses the Rust `regex` crate, which rejects
  exponential backtracking patterns by design. Still, a bad regex
  against a huge file can run for a long time. Use `--max-results`
  and reasonable timeouts.

## Deployment recommendations

1. **Put it behind a perimeter.** Loopback binding, a VPN, an SSH tunnel
   (`ssh -L 7777:127.0.0.1:7777 loghost`), or an authenticating reverse
   proxy. `rx` should never be directly reachable from an untrusted
   network. Everything below assumes this one is done.
2. **Set multiple specific `--search-root` values** rather than one
   broad root. The sandbox is only as narrow as you make it.
3. **Leave `--hidden` off** unless you need it. It is off by default, and
   turning it on inside a home directory re-exposes exactly the files the
   rule exists to keep back.
4. **Let the proxy handle TLS and rate limiting** as well as auth. `rx`
   serves plain HTTP and has no rate limiter.
5. **Disable per-request hook overrides** where more than one person can
   reach the server: `RX_DISABLE_CUSTOM_HOOKS=true`.
6. **Enable `RX_HOOK_STRICT_IP_ONLY=true`** if webhook destinations are
   internal — and on rx-python, if DNS rebinding is in your threat model
   at all.
7. **Monitor `/metrics`** — `rx_errors_total{error_type="access_denied"}`
   and `rx_hook_calls_total{status="failure"}` flag misbehavior. The full
   `error_type` set is `access_denied`, `file_not_found`,
   `invalid_params`, `invalid_regex`, `service_unavailable` and
   `internal_error`.
8. **Use separate per-user `RX_CACHE_DIR`** if several people share a
   host. Cache entries are not isolated between users.

## Related concepts

- [`rx serve`](../cli/serve.md) — sandbox configuration
- [Webhooks](../api/webhooks.md) — full webhook behavior
- [Configuration](../configuration.md) — every `RX_*` env var
- [Caching](caching.md) — cache isolation

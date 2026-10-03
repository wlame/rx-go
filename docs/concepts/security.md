# Security

## Intended use, first

**`rx` is built for internal use on a trusted network. It is not intended
to be exposed to the internet.**

`rx serve` has no identity system. Unless `RX_API_TOKEN` is set, anyone
who can reach the port can read any file under `--search-root`, and
`--search-root` defaults to the current directory. There is no TLS and
no multi-tenancy.

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

Each is described below, together with the opt-in API token. At the
end, we cover the threat model and known limitations.

## Opt-in API token

Some deployments sit on a network trusted enough not to need a proxy
but shared enough that a shared secret is worth having. Set
`RX_API_TOKEN` and every `/v1` request must carry it:

```bash
RX_API_TOKEN="$(openssl rand -hex 24)" rx serve --host=0.0.0.0 --search-root=/var/log

curl -H "Authorization: Bearer $RX_API_TOKEN" \
  "http://loghost:7777/v1/trace?regexp=error&path=/var/log/app.log"
```

A request without it answers `401` with a `WWW-Authenticate: Bearer`
challenge and the usual `{"detail": "..."}` body. The comparison is
constant-time.

- **One value for everyone.** No users, no sessions, no expiry. Rotate
  it by restarting the server with a new value. Anything more is the
  perimeter's job.
- **Only `/v1` is guarded.** `/health`, `/metrics`, `/docs`,
  `/openapi.json` and the viewer's files stay open, so probes and
  scrapers keep working. `/health` reports `RX_API_TOKEN` — like any
  variable whose name contains `TOKEN`, `SECRET`, `PASSWORD` or
  `API_KEY` — as `<redacted>`.
- **The viewer** takes the token from the link it is opened with,
  `http://loghost:7777/#token=…`, keeps it for the browser tab, and
  removes it from the address bar. Without one it asks.
- **It is not a substitute for TLS.** The token crosses plain HTTP in
  clear text; anyone who can read the traffic can read the token. For
  anything beyond a trusted network, keep the VPN, the SSH tunnel or
  the TLS proxy.

The OpenAPI document declares the scheme as `bearerAuth` on every `/v1`
operation, as optional — a server without `RX_API_TOKEN` ignores the
header.

## Security response headers

Every response from `rx serve` carries these:

| Header | Value |
|---|---|
| `Content-Security-Policy` | `frame-ancestors 'none'` |
| `X-Frame-Options` | `DENY` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |

The viewer renders untrusted content by definition — the log lines it
displays are attacker-influenced in many deployments — and a browser
**ignores these three in a meta tag**, so the SPA cannot set them itself:

- without `frame-ancestors` (or its older equivalent `X-Frame-Options`)
  nothing stops the viewer being framed, so a page on another origin can
  overlay it and harvest clicks;
- without `nosniff` a browser may re-guess a response's type and execute
  a log file as script;
- without a referrer policy a filesystem path in the query string travels
  to any host the user navigates to next.

The header CSP carries **only** `frame-ancestors`. The full policy stays
in the SPA's meta tag, which is the artifact that knows what Monaco
needs; the two intersect, so a stricter header would break the editor.
The `/docs` page huma serves sets a fuller policy of its own, which also
denies framing.

`serve` binds loopback by default, which limits exposure but does not
remove it: users run it behind a reverse proxy, and a browser tab on any
site can reach `127.0.0.1`.

rx-python sends the same table.

## Path sandbox (`--search-root`)

### What it does

Every endpoint and CLI command that accepts a file path validates it
against a configured list of **search roots**:

```bash
rx serve --search-root=/var/log --search-root=/var/data/exports
```

Any path that resolves outside all configured roots is rejected with
`403 path_outside_search_root` (HTTP) or exit code 4 (CLI).

The HTTP body is the `SandboxError` shape, published in the OpenAPI
document and identical in both backends:

```json
{
  "detail": "path_outside_search_root",
  "error": "path_outside_search_root",
  "message": "path \"/outside/x.log\" is not within any configured --search-root",
  "path": "/outside/x.log",
  "roots": ["/srv/data", "/var/log"]
}
```

`error` is the stable machine code to branch on; `detail` repeats it so a
client that reads only that key still gets something comparable. `roots`
is sorted, so the flag order the operator used does not change the body.

The other two refusals — a hidden entry, and a directory the process
cannot read — keep the ordinary `{"detail": "..."}` envelope, because the
fix for them is different: `--hidden`, or file permissions, not another
root.

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
- CLI: on every path argument, including output paths, when
  `--search-root` or `RX_SEARCH_ROOTS` names at least one root

When no roots are configured (the CLI default), no validation runs. The
sandbox is opt-in for the CLI and on by default for `rx serve`, which
falls back to `RX_SEARCH_ROOTS` and then to the current working
directory.

### Switching the sandbox on from the CLI

`--search-root` is accepted by every subcommand, repeats to name several
roots, and may be written before or after the subcommand:

```bash
rx compress app.log --output=/tmp/app.zst --search-root=/var/log
rx --search-root=/var/log --search-root=/srv/data trace error /srv/data/app.log
RX_SEARCH_ROOTS=/var/log rx samples /var/log/app.log --lines=100
```

The flag wins over `RX_SEARCH_ROOTS`. A root that does not exist, or is
not a directory, is a usage error (exit code 2) rather than a silent
"no sandbox" — a caller who asked to be confined and was not would never
find out. A path outside every root is exit code 4.

`rx serve` keeps its own `--search-root`, whose default is the current
directory rather than "no sandbox", and publishes the roots it resolved
in `RX_SEARCH_ROOTS` so any process it spawns inherits the same
confinement.

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

### Symlinks inside a directory search

A search of a directory reads only what naming the same path would let
you read. `rx trace` on a directory (recursive or `--no-recursive`),
`rx index` on a directory (with or without `--recursive`) and the
`/v1/tree` listing all apply one rule to every symbolic link they meet
inside the directory:

| Where the link leads | What the walk does |
|---|---|
| A file inside a search root, not hidden | Reads it, under the link's own path |
| A directory inside a search root, not hidden, not searched yet | Descends into it (recursive walks only) |
| A directory the walk has already searched | Skips it, reason `directory already searched through '<path>'` |
| Outside every search root | Skips it, reason `symlink leads outside all search roots` |
| Into a hidden entry, without `--hidden` | Skips it, reason `symlink leads into hidden entry '.name'; …` |
| Back to a directory the walk is already inside | Skips it, reason `symlink loop: …`, so a loop cannot hang the walk |
| Nowhere, or to itself | Skips it, reason `cannot resolve symlink: …` |

Each directory is searched at most once per walk, so the work is
bounded by the number of real directories, however many links lead
into them. Without that rule six levels of six links to the next level
made one request search 46,656 paths to one file. The walk follows
links to directories only after it has searched every real directory
below the walked one, so a directory reached both directly and through
a link is searched under its own path, and the link is the one
reported as skipped.

The target is checked the way a named path is: its symlinks are
resolved, the result must lie inside a root, and no component below
that root may be hidden. So a link that would answer 403 (HTTP) or exit
code 4 (CLI) when named directly is never searched as part of a
directory either.

A skipped link appears in `skipped_files` of a trace answer, and in
`skipped` and `skip_reasons` of `rx index --json`, so you can see that
part of the tree was not searched. `/v1/tree` leaves it out of the
listing, as it leaves out hidden entries: listing it would only offer a
path that returns 403. A link to a directory is never listed or searched
as a file; with `--no-recursive` it is passed over like any directory.

A file reached both directly and through a link inside the roots is
searched once under each path, as `rg --follow` does. Only directories
are searched once.

`/v1/tree` lists one directory per request, so it has no walk to bound:
every link to a directory inside the roots is listed, as a way to browse
into it.

Without a sandbox (the CLI without `--search-root`) there is no root to
stay inside, and naming a link is always allowed: a walk follows links
wherever they lead, still skipping loops and links that resolve to
nothing. Entries whose own name starts with a dot are skipped either
way, unless `--hidden`.

```bash
ln -s /etc/passwd /var/log/app/passwd.log
rx --search-root=/var/log trace root /var/log/app --json | jq .skipped_files
# ["/var/log/app/passwd.log"]
```

### A file is read only while it is the file that was checked

A check of a path and the read of it are two separate look-ups. Between
them, someone who can write in a served directory can retarget a link,
or replace a file or a directory with a link, and a read by path would
then reach a file the check never saw.

So rx records what each check found: the device and inode of the file
the path led to. Every read of a searched file opens the path and then
compares the identity of the file it actually opened with the recorded
one; when they differ, the file is not read. This holds for every read
of a trace (the chunk scan, gzip and other compressed streams, seekable
zstd frames, the rebuild of an answer from the trace cache, and the
numbering of lines a capped search left unknown), for `samples`, for an
index build and for the input of `rx compress`. An index build opens
the file once and takes everything from that one handle: the format,
the fingerprint, the lines, a seekable file's seek table and frames,
and the text `--analyze` reads. A directory walk lists
each directory through a handle checked the same way, so a directory
swapped for a link while the walk runs cannot lend it the files of
another directory.

A refused file is reported, never read: a trace lists it in
`skipped_files`, and `samples` fails with `file changed after it was
checked`. The path you named is what every answer reports, also when
it is a link.

The identity itself is taken by walking down from the search root one
directory at a time, through handles, refusing any link on the way:
the checked location has every link already resolved, so a link found
there now means the tree changed during the check. A directory swapped
for a link while the check runs, to outside the roots or into a hidden
directory, fails the check instead of having its file recorded.

A line index is looked up by the path, so it is used for a pinned
file only when the inode it recorded is that file's. An index built
from whatever else the path led to for a moment is treated as absent,
and the answer is computed from the file itself.

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
`0.2.0 <= v < 0.5.0` today (the viewer is a 0.x product, where a minor bump
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
- Reading a file outside the roots, or a hidden one, through a symlink
  that someone placed inside a served directory — blocked by checking
  every symlink a directory search meets as if it were named
- Retargeting such a link, or replacing a checked file or directory with
  a link, between the check and the read — blocked by reading a file
  only while its device and inode are the ones the check recorded
- Tying up the server with a tree of links that lead sideways to other
  directories (6 levels × 6 links = 46,656 paths to one file) — blocked
  by searching each directory once per walk
- Exhausting the server's memory with one search for a common
  character over a file with a very long line (ripgrep reports the line
  whole, plus about 50 bytes per submatch: some 5 GB for a 100 MB line)
  — blocked by reading ripgrep's output without holding it and keeping
  at most `RX_MAX_LINE_TEXT_BYTES` of a line and
  `RX_MAX_SUBMATCHES_PER_LINE` submatches; see
  [long lines](../api/endpoints/trace.md#long-lines)
- Zip-slip / tar-slip in the SPA cache — blocked by extractor
  validation
- SSRF to internal services via hook URLs — blocked by address-range
  check + DNS resolution

### Things `rx` does NOT defend against

- **Identity** — `rx serve` has no users, roles or sessions. Without
  `RX_API_TOKEN`, anyone who can reach the socket can run any operation
  within the sandbox; with it, anyone who has the one shared token can.
  This is by design; see "Intended use, first" above.
- **DoS** — no built-in rate limiting. A single client can
  simultaneously launch N traces and exhaust CPU. Each line costs a
  bounded amount of memory, but an uncapped trace still holds every
  match it returns. Use a reverse proxy or a process supervisor that
  caps concurrent requests, and `max_results`.
- **Exposure to an untrusted network** — there is no TLS, and the
  optional token travels in clear text. This is the scope decision at
  the top of the page, not a bug. Put `rx` behind a perimeter.
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
5. **Set `RX_API_TOKEN`** when the server is reachable from machines
   other than the operator's. `rx serve` warns at startup when it binds
   beyond loopback without one.
6. **Disable per-request hook overrides** where more than one person can
   reach the server: `RX_DISABLE_CUSTOM_HOOKS=true`.
7. **Enable `RX_HOOK_STRICT_IP_ONLY=true`** if webhook destinations are
   internal — and on rx-python, if DNS rebinding is in your threat model
   at all.
8. **Monitor `/metrics`** — `rx_errors_total{error_type="access_denied"}`
   and `rx_hook_calls_total{status="failure"}` flag misbehavior. The full
   `error_type` set is `access_denied`, `file_not_found`,
   `invalid_params`, `invalid_regex`, `service_unavailable` and
   `internal_error`.
9. **Use separate per-user `RX_CACHE_DIR`** if several people share a
   host. Cache entries are not isolated between users.

## Related concepts

- [`rx serve`](../cli/serve.md) — sandbox configuration
- [Webhooks](../api/webhooks.md) — full webhook behavior
- [Configuration](../configuration.md) — every `RX_*` env var
- [Caching](caching.md) — cache isolation

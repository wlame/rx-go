# `GET /v1/tree`

List the contents of a directory within the configured search roots.

## Purpose

Powers file-tree navigation in UIs that sit on top of `rx serve`.
Returns directory listings enriched with per-entry metadata: size,
modification time, compression status, whether a line index exists, etc.

With no `path` parameter, returns the configured search roots as
top-level entries.

## Request

```text
GET /v1/tree                       # list search roots
GET /v1/tree?path=<dir>            # list dir contents
```

### Query parameters

| Parameter | Type | Required | Description |
|---|---|:-:|---|
| `path` | string | no | Directory path. Omit to list search roots. |

## Response — 200 OK

### Search-root listing

```json
{
  "path":          "/",
  "parent":        null,
  "is_search_root": true,
  "total_entries": 2,
  "total_size":    null,
  "total_size_human": null,
  "entries": [
    {
      "name":        "log",
      "path":        "/var/log",
      "type":        "directory",
      "size":        null,
      "size_human":  null,
      "modified_at": "2026-04-18T14:00:00.000000Z",
      "is_compressed":      null,
      "compression_format": null,
      "is_text":            null,
      "not_text_reason":    null,
      "is_indexed":         null,
      "line_count":         null,
      "children_count":     47,
      "findings":           null
    },
    {
      "name":        "exports",
      "path":        "/var/data/exports",
      "type":        "directory",
      "size":        null,
      "size_human":  null,
      "modified_at": "2026-04-17T08:12:34.207415Z",
      "is_compressed":      null,
      "compression_format": null,
      "is_text":            null,
      "not_text_reason":    null,
      "is_indexed":         null,
      "line_count":         null,
      "children_count":     12,
      "findings":           null
    }
  ]
}
```

### Directory listing

```json
{
  "path":    "/var/log",
  "parent":  "/var",
  "is_search_root": false,
  "total_entries": 4,
  "total_size":    623190208,
  "total_size_human": "594.32 MB",
  "entries": [
    {
      "name":        "nginx",
      "path":        "/var/log/nginx",
      "type":        "directory",
      "size":        null,
      "size_human":  null,
      "modified_at": "2026-04-18T13:59:00.000000Z",
      "is_compressed":      null,
      "compression_format": null,
      "is_text":            null,
      "not_text_reason":    null,
      "is_indexed":         null,
      "line_count":         null,
      "children_count":     4,
      "findings":           null
    },
    {
      "name":        "audit-2026-02.log.zst",
      "path":        "/var/log/audit-2026-02.log.zst",
      "type":        "file",
      "size":        41048256,
      "size_human":  "39.15 MB",
      "modified_at": "2026-04-01T00:05:12.940117Z",
      "is_compressed":      true,
      "compression_format": "zstd",
      "is_text":            true,
      "not_text_reason":    null,
      "is_indexed":         false,
      "line_count":         null,
      "children_count":     null,
      "findings":           null
    },
    {
      "name":        "audit-2026-03.log",
      "path":        "/var/log/audit-2026-03.log",
      "type":        "file",
      "size":        582137856,
      "size_human":  "555.17 MB",
      "modified_at": "2026-04-18T13:42:10.381526Z",
      "is_compressed":      false,
      "compression_format": null,
      "is_text":            true,
      "not_text_reason":    null,
      "is_indexed":         true,
      "line_count":         3528914,
      "children_count":     null,
      "findings":           {"state": "full", "count": 412}
    },
    {
      "name":        "dump.bin",
      "path":        "/var/log/dump.bin",
      "type":        "file",
      "size":        4096,
      "size_human":  "4.00 KB",
      "modified_at": "2026-04-02T10:00:00.000000Z",
      "is_compressed":      false,
      "compression_format": null,
      "is_text":            false,
      "not_text_reason":    "not a text file: a NUL byte in its first 8 KiB",
      "is_indexed":         false,
      "line_count":         null,
      "children_count":     null,
      "findings":           null
    }
  ]
}
```

## Response fields

| Field | Type | Description |
|---|---|---|
| `path` | string | The listed directory (or `"/"` for search-root list) |
| `parent` | string \| null | Parent directory, or `null` for search roots |
| `is_search_root` | bool | `true` when `path` equals a configured search root |
| `total_entries` | int | Number of `entries` |
| `total_size` | int64 \| null | Sum of all file sizes under `path` (top-level only); `null` when the sum is 0 |
| `total_size_human` | string \| null | Human-readable total size |
| `entries` | array | One `TreeEntry` per child |

### `entries[]` fields

| Field | Type | When not `null` |
|---|---|---|
| `name` | string | Always — the basename |
| `path` | string | Always — absolute path |
| `type` | string | Always — `"file"` or `"directory"` |
| `modified_at` | string | When available — UTC with exactly six fractional digits (`2026-04-18T14:00:00.000000Z`) |
| `size` | int64 | Files only |
| `size_human` | string | Files only |
| `children_count` | int | Directories only — non-recursive entry count |
| `is_compressed` | bool | Files only |
| `compression_format` | string | Compressed files only — `"gzip"`, `"bz2"`, `"xz"`, `"zstd"` (a seekable zstd file is `"zstd"`) or `"journal"` (a systemd journal), decided by the magic bytes, never by the name |
| `is_text` | bool | Files only — false when the first 8 KiB of the file's text (decompressed for a compressed file) hold a NUL byte or start as UTF-16 text, or for a systemd journal whose header rx cannot read: the rules every command refuses a file by. A journal whose header rx can read is text |
| `not_text_reason` | string \| null | Files only — why `is_text` is false, in the words every command refuses the file with: `not a text file: a NUL byte in its first 8 KiB` (`… in the first 8 KiB of its decompressed text` for a compressed file), `not a text file: UTF-16 text, which rx does not decode` (`its decompressed text is UTF-16, …`), or `not a text file: a systemd journal rx cannot read: <why>`. `null` for a text file, a directory, and a file the listing could not open. Since contract 2.1 |
| `is_indexed` | bool | Files only — whether a line index exists in cache |
| `line_count` | int64 | Files with an index that has a line count |
| `findings` | object \| null | Text files only: what the findings cache holds for the file, `{"state": "full" \| "partial", "count": N}` (`count` sums the findings of every current entry, opt-in signatures included); `null` for a directory, a file that is not text, and a file the cache holds nothing current for. Read from the file's `meta.json` alone, its identity compared by the stat the listing already holds, so a listing reads nothing more of the file. Since contract 2.0 |

Every entry carries all 14 fields. A field that does not apply to the
entry is `null` (`size` on a directory, `children_count` on a file),
and every field except `name`, `path` and `type` is `null` when the
underlying stat failed.

### Sort order

Entries are sorted by type (directories first), then by name
(case-insensitive alphabetical). No client-side sorting is needed for
basic display.

### Which entries are listed

A listing shows what a caller can open by name. Entries whose name
starts with a dot are left out unless the server runs with `--hidden`.
A symbolic link is listed under its own name with the type of its
target, so a link to a directory is a `directory`. A link that leads
outside every search root, into a hidden entry, or to nothing is left
out, and `children_count` does not count it; so is a named pipe, a
socket or a device, which no route opens. Every link to a directory
inside the roots is listed, also when several lead to the same one: the
listing shows one level, and each link is a way to browse into it. See
[Symlinks inside a directory search](../../concepts/security.md#symlinks-inside-a-directory-search).

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Listing returned |
| `400 Bad Request` | Path is a file, not a directory |
| `401 Unauthorized` | The server sets `RX_API_TOKEN` and the request has no valid `Authorization: Bearer` header ([Authentication](../index.md#authentication)) |
| `403 Forbidden` | Path outside `--search-root`, or permission denied |
| `404 Not Found` | Directory doesn't exist |

## Examples

### List search roots

```bash
curl -s 'http://127.0.0.1:7777/v1/tree' | jq '.entries[] | .path'
```

### List a directory

```bash
curl -sG 'http://127.0.0.1:7777/v1/tree' \
    --data-urlencode 'path=/var/log' \
    | jq '.entries[] | select(.type == "file") | {name, size_human, is_indexed}'
```

### Find all compressed files

```bash
curl -sG 'http://127.0.0.1:7777/v1/tree' \
    --data-urlencode 'path=/var/log' \
    | jq '.entries[] | select(.is_compressed == true) | .path'
```

### Walk recursively (client-side)

`/v1/tree` is non-recursive — one directory per request. For a recursive
walk, iterate client-side:

```bash
walk() {
    local path="$1"
    curl -sG 'http://127.0.0.1:7777/v1/tree' --data-urlencode "path=$path" \
        | jq -c '.entries[]' | while read entry; do
            p=$(echo "$entry" | jq -r '.path')
            t=$(echo "$entry" | jq -r '.type')
            echo "$p"
            [ "$t" = "directory" ] && walk "$p"
        done
}
walk /var/log
```

## Performance notes

- One listing of the directory per request, plus one listing of each
  subdirectory for its `children_count`
- Per-entry metadata costs one `os.Stat`, one `os.Open` (for text
  probe), one index-cache lookup, and one read of the file's findings
  `meta.json` (for `findings`)
- On a directory with a thousand files, expect response time on the
  order of tens of milliseconds
- `total_size` is a sum of direct children only — not a recursive
  disk-usage computation

## See also

- [`rx serve`](../../cli/serve.md) — configure search roots
- [concepts/security](../../concepts/security.md) — sandbox details
- [API conventions](../conventions.md) — path validation

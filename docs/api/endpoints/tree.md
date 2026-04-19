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
      "modified_at": "2026-04-18T14:00:00Z",
      "children_count": 47
    },
    {
      "name":        "data",
      "path":        "/var/data/exports",
      "type":        "directory",
      "modified_at": "2026-04-17T08:12:34Z",
      "children_count": 12
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
  "total_entries": 3,
  "total_size":    623186112,
  "total_size_human": "594.3 MiB",
  "entries": [
    {
      "name":        "nginx",
      "path":        "/var/log/nginx",
      "type":        "directory",
      "modified_at": "2026-04-18T13:59:00Z",
      "children_count": 4
    },
    {
      "name":        "audit-2026-03.log",
      "path":        "/var/log/audit-2026-03.log",
      "type":        "file",
      "size":        582137856,
      "size_human":  "555.2 MiB",
      "modified_at": "2026-04-18T13:42:10Z",
      "is_compressed": false,
      "is_text":       true,
      "is_indexed":    true,
      "line_count":    3528914
    },
    {
      "name":        "audit-2026-02.log.zst",
      "path":        "/var/log/audit-2026-02.log.zst",
      "type":        "file",
      "size":        41048256,
      "size_human":  "39.1 MiB",
      "modified_at": "2026-04-01T00:05:12Z",
      "is_compressed":      true,
      "compression_format": "seekable_zstd",
      "is_text":            true,
      "is_indexed":         false
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
| `total_size` | int64 \| null | Sum of all file sizes under `path` (top-level only) |
| `total_size_human` | string \| null | Human-readable total size |
| `entries` | array | One `TreeEntry` per child |

### `entries[]` fields

| Field | Type | When present |
|---|---|---|
| `name` | string | Always — the basename |
| `path` | string | Always — absolute path |
| `type` | string | Always — `"file"` or `"directory"` |
| `modified_at` | string | When available (RFC 3339 nanosec) |
| `size` | int64 | Files only |
| `size_human` | string | Files only |
| `children_count` | int | Directories only — non-recursive entry count |
| `is_compressed` | bool | Files only |
| `compression_format` | string | Files only — `"gzip"`, `"bzip2"`, `"xz"`, `"zstd"`, `"seekable_zstd"` |
| `is_text` | bool | Files only — based on NUL-byte probe of first 512 bytes |
| `is_indexed` | bool | Files only — whether a line index exists in cache |
| `line_count` | int64 | Files with an index that has a line count |

All fields except `name`, `path`, and `type` are pointer types in Go
and may appear as `null` in JSON when the underlying stat failed.

### Sort order

Entries are sorted by type (directories first), then by name
(case-insensitive alphabetical). No client-side sorting is needed for
basic display.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Listing returned |
| `400 Bad Request` | Path is a file, not a directory |
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

- One `os.ReadDir` call per request
- Per-entry metadata costs one `os.Stat`, one `os.Open` (for text
  probe), and one index-cache lookup
- On a directory with a thousand files, expect response time on the
  order of tens of milliseconds
- `total_size` is a sum of direct children only — not a recursive
  disk-usage computation

## See also

- [`rx serve`](../../cli/serve.md) — configure search roots
- [concepts/security](../../concepts/security.md) — sandbox details
- [API conventions](../conventions.md) — path validation

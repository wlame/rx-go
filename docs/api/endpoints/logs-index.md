# `POST /v1/logs/index`

Builds and stores the line index of every part of one log chain, in a
background task: the background form of
[`rx logs index`](../../cli/logs.md#rx-logs-index).

## Purpose

A chain is `ready` once each frozen part has a current line index (see
[`GET /v1/logs/chain`](logs-chain.md)), which starts the builds it waits
for by itself. This route indexes the whole chain on demand: every part
without a current index, the active file too, whose index speeds a jump
inside it until it grows; with `force=true`, every part again.

## Request

```text
POST /v1/logs/index?path=<handle>[&force=true][&fingerprint=<fp>]
```

No body.

### Query parameters

| Parameter | Type | Required | Description |
|---|---|:-:|---|
| `path` | string | yes | The chain's handle: its directory joined with its name (`/var/log/syslog`), as `GET /v1/logs/chains` gives it in `path` |
| `force` | bool | no | Build the index of every part again, current ones too (default `false`) |
| `fingerprint` | string | no | The fingerprint of a description the client holds (16 hex digits). When the chain's files changed since, the answer is `409` with the current description, and no task starts |

## The task

The request describes the chain as `GET /v1/logs/chain` does, to learn
its parts and which have a current index, starts the task and answers at
once. The task has the operation `chain_index` and shows the handle as
its `path` at [`GET /v1/tasks/{id}`](tasks.md).

- **One task per chain.** While a chain's task runs, a second
  `POST /v1/logs/index` and a describe of the pending chain join it: the
  same `task_id`. A joined task builds what its first start asked for,
  so `force=true` does not change a task that is already running. The
  task holds the key `chain:<handle>`, not the handle: a task on the
  active file, whose path equals the handle (`POST /v1/index` of it),
  runs beside it.
- **Every part, whatever its size.** `RX_LARGE_FILE_MB`, below which
  `rx index` and `POST /v1/index` skip a file, does not apply: the chain
  needs each part's line count and times, and rotated parts are often
  small.
- **The same builds as a samples lookup.** Each part is built by the
  background build `GET /v1/samples` starts for a file, so a lookup in a
  part waits for the same build, and a build already running for a part
  (started by a lookup or by `POST /v1/index`) is joined, not repeated.
  At most `RX_MAX_INDEX_BUILDS` builds run at once, the server over; the
  task submits at most that many of its parts at a time and the next one
  as one of them ends, so a chain of thousands of parts never fills the
  queue of 256 builds that wait for a slot.
- **Progress** is the share of the task's parts done, a part whose
  build runs counted by that build's own progress.
- **Failure.** The first part whose build fails fails the task, with
  `error` naming the part and the build's error
  (`app.log.1: build index: …`); no further part is submitted, and the
  builds already running go on as tasks of their own. A part whose path
  a compression holds is waited for and left as it is. When other
  files' builds fill the queue, the task waits until a build ends and
  submits the part then.

### Task result

A completed task's `result` is a `ChainIndexTaskResult`:

```json
{
  "path": "/var/log/syslog",
  "built": ["syslog-20261005-1791158400.gz", "syslog-20261006-1791244800.gz", "syslog"],
  "cli_command": "rx logs index /var/log/syslog"
}
```

| Field | Type | Description |
|---|---|---|
| `path` | string | The chain's handle |
| `built` | string[] | The parts whose line index a build completed for the task (one it started or one it joined), in the chain's order. A part whose index was current and not rebuilt is not listed |
| `cli_command` | string | `rx logs index HANDLE`, with `--force` when it was given |

## Response — 200 OK

The task, as `POST /v1/index` gives one:

```json
{
  "task_id": "8f6c1a52-3e0a-4c1e-9a67-2d6c3f1b9e10",
  "status": "queued",
  "message": "Indexing 3 parts of the log chain /var/log/syslog; follow GET /v1/tasks/8f6c1a52-3e0a-4c1e-9a67-2d6c3f1b9e10",
  "path": "/var/log/syslog",
  "started_at": "2026-10-08T09:12:44.103254Z"
}
```

A request that joins a running task says so in `message`.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | The chain's index task, started or joined |
| `400 Bad Request` | The handle ends in no name (`/var/log/`) |
| `403 Forbidden` | The handle's directory is outside `--search-root`, or the directory or the name is hidden |
| `404 Not Found` | The handle names fewer than two parts, or its directory does not exist |
| `409 Conflict` | The body is the current description (as `GET /v1/logs/chain` gives it): `fingerprint` differs from it, or a part was replaced while the request read it. No task starts |
| `422 Unprocessable Entity` | `path` is missing, or `fingerprint` is not 16 hex digits |
| `500 Internal Server Error` | The chain's files kept changing on every attempt to read them (three) |

## Examples

```bash
task=$(curl -sX POST -G 'http://127.0.0.1:7777/v1/logs/index' \
    --data-urlencode 'path=/var/log/syslog' | jq -r .task_id)
curl -s "http://127.0.0.1:7777/v1/tasks/$task" | jq '{status, progress, result}'
```

## See also

- [`rx logs index`](../../cli/logs.md#rx-logs-index) — the same in the foreground
- [`GET /v1/logs/chain`](logs-chain.md) — the chain's state, and the task it starts for a pending chain
- [`GET /v1/tasks/{id}`](tasks.md) — follow the task

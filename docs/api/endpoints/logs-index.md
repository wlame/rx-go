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
  chain is known by its directory's device and inode and its name (by
  its handle on a filesystem that gives every directory inode 0), so
  two handles that reach one directory by different paths (a symbolic
  link to it, or another case of its name on a case-insensitive disk)
  share the task, which shows the handle it was started with as its
  `path`. The task holds that key, not the handle: a task on the
  active file, whose path equals the handle (`POST /v1/index` of it),
  runs beside it.
- **At most 128 chain tasks at once.** As many chains' index tasks run
  or wait at once as the part builds of all chains may take places in
  the build queue (half of it, 128), so each can hold one. Past that, no
  task starts: the request answers `503` with `Retry-After: 5` (the
  seconds to wait before asking again) and starts nothing, and a
  describe of a pending chain names no task (`index_build` null, with
  the reason in `index_build_refused`) until one of them ends. A
  request for a chain whose task runs still joins it.
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
  queue of 256 builds that wait for a slot. The part builds of all
  chains together, queued or running, take at most half of that queue
  (128): a chain that finds them all taken waits in line for room, so
  a lookup in another file always finds room, however many chains are
  pending. Each build's end that leaves room lets the first chain in
  line go on: one chain, not all of them.
- **Part builds leave other tasks alone.** Each part build the task
  starts is a task of its own (operation `index`, its `path` the
  part's), shown at `GET /v1/tasks/{id}` like any other. Finished part
  builds are kept apart from the other tasks: at most 256 of them, the
  oldest dropped first, so a chain of thousands of parts never drops a
  task another client follows (`POST /v1/index`, `POST /v1/compress`,
  a samples lookup's build) from the table.
- **Progress** is the share of the task's parts done, a part whose
  build runs counted by that build's own progress.
- **Failure.** The first part whose build fails fails the task, with
  `error` naming the part and the build's error
  (`app.log.1: build index: …`); no further part is submitted, and the
  builds already running go on as tasks of their own. The task learns
  how each part's build ended from the build itself, so a failure
  counts even when the table no longer holds that build's task. A part
  whose path a compression holds is waited for and left as it is. When
  other files' builds fill the queue, the task waits until a build ends
  and submits the part then.

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
| `503 Service Unavailable` | No task runs for the chain, and none can start: 128 chains' index tasks run or wait already. No task started; ask again once one of them has ended. The header `Retry-After: 5` gives the seconds to wait before asking again |

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

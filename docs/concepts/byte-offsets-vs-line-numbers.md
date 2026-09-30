# Byte offsets vs line numbers

`rx` exposes two units for addressing positions in a file: **byte
offsets** and **line numbers**. They're different tools with different
costs. Understanding when to use each is the single most useful thing
to know about `rx`'s design.

## The quick rule

- **Byte offsets are O(1) to seek.** Given a byte offset, the OS can
  `seek` directly to that position regardless of file size.
- **Line numbers are O(n) to compute.** Given a line number, you
  either count `\n` bytes from the start of the file or consult an
  index.

If you have the choice, use byte offsets.

## Why byte offsets are the default

Throughout `rx`, byte offsets are the primary unit:

- `rx trace` output reports match byte offsets
- `rx samples` prefers byte-offset mode (`--offsets`)
- `/v1/trace` responses include `offset` in every match
- Cache keys are computed with byte offsets

This is because every reported offset can be fed straight back into
another seek, regardless of file size. A 100 GB file has the same
constant-time seek cost as a 100 MB file, as long as the OS can
satisfy the syscall (which it always can for regular files on local
disks).

## Why line numbers exist at all

Humans think in lines, not bytes. When you look at a log:

```text
2026-03-14 08:23:51 ERROR timeout 5023ms
```

You don't care that this is at byte offset 1,024,581. You care that
it's "the 9,812th line" or "around line 10k".

Tools that accept input from users (`rx samples --lines=100`) need to
accept line numbers. Tools that integrate with other software (grep,
editors, log viewers) often expect line numbers.

## The cost of line numbers

### Without an index

Given a line number N, the only way to find its byte offset is:

```text
open file
counter = 0
while (counter < N):
    read until next '\n'
    counter++
return current byte position
```

This scans from the start of the file to line N. For N = 1,000 this
is quick (12 ms for the whole `rx samples` call). For N = 40,000,000 in
a 6.3 GB log, it took 2.46 s.

### With an index

`rx` builds sparse checkpoints: "line 12500 is at byte 2097152", "line
25000 is at byte 4194304", etc. To find the byte offset of line
N = 13217:

1. Binary-search the index: find the largest checkpoint where
   `line_number <= 13217`. That's `(12500, 2097152)`.
2. Seek to byte 2097152.
3. Read forward counting newlines until we pass line 13217 - 12500 = 717
   newlines.

Step 3 scans at most a few hundred KB — the distance between
checkpoints. On warm cache, this is milliseconds regardless of file
size.

See [line indexes](line-indexes.md) for the full index format.

## Match-offset → content-retrieval chain

A common pattern in `rx` is:

1. `rx trace` returns match byte offsets
2. `rx samples --offsets=...` retrieves context around those offsets

Because both use byte offsets, each sample starts at a known position.
`samples` still numbers the line it prints, which reads from the
nearest index checkpoint (or from byte 0 without an index) once for the
whole batch: one pass serves every offset in the request.

Example:

```bash
# Get match offsets as a comma-separated list.
offsets=$(rx "timeout" /var/log/app.log --json \
    | jq -r '.matches | map(.offset | tostring) | join(",")')

# Retrieve context at each offset.
rx samples /var/log/app.log --offsets="$offsets" --context=2
```

On a 465 MB log, an offset 403 MB in took 15 ms without an index and
14 ms with one.

## When line numbers are unavoidable

### When you know the line, not the byte

Human-facing inputs — "show me line 10000" — are line-native.
`rx samples --lines=10000` is the right command.

### When you want the last N lines

"The last 50 lines of the file" can't be expressed in byte offsets.
`rx samples --lines=-50--1` asks for them directly. On a plain file it
is cheap with or without an index (11 ms on a 465 MB log either way);
on a gzip, bzip2, xz or plain zstd file it streams the file twice, once
to count the lines.

### When presenting to users

The JSON output of `rx trace` includes both `offset` (byte) and
`absolute_line_number` so the downstream UI can display whichever is
more useful.

## Compressed files

A byte offset `rx` reports for a compressed file is a position in the
decompressed text — the same number the plain copy of the file gives.
`rx trace` on a log, its `.gz` copy and a seekable `.zst` made from it
reports the same offset and line for each match (403366791 and
1191541 for one match in a 465 MB log), and the `lines` map of
`rx samples --lines` on the `.gz` reports that offset too.

`rx samples --offsets` takes the same positions, so `--lines=N` and
`--offsets=` the offset of line N lead to each other on a compressed
file as on a plain one. A capped trace of a seekable zstd file can
leave a match without a line number (`-1`); `rx samples --offsets=…`
with the match's offset resolves it. The cost differs by format: a
gzip, bzip2, xz or plain zstd file is decompressed from its first byte
up to the last offset asked about, while an indexed seekable zstd file
decompresses the frames around each offset.

Seekable zstd files produced by [`rx compress`](../cli/compress.md)
make lookups cheap: `samples --lines` decompresses only the frames
holding the wanted lines, and `samples --offsets` only the frames around
the wanted offsets.

## Implications

- **Design your client around byte offsets.** If you're building a
  tool that consumes `rx trace` output, store match offsets and use
  them for follow-up queries. Don't convert to line numbers unless
  you need to display them.

- **Line lookups deep into a large file want an index.** `rx samples`
  builds one by itself on the first lookup in a plain file of 50 MB or
  more; after that, line 700000 of a 465 MB log took 13 ms against
  90 ms without one.

- **On compressed files, prefer seekable zstd.** A gzip, bzip2, xz or
  plain zstd file is streamed in full for every line lookup, twice for
  `--lines=-N`, and up to the last offset for an offset lookup.

## Related

- [Chunking](chunking.md) — how parallel workers handle byte ranges
- [Line indexes](line-indexes.md) — how `rx` makes line lookups fast
- [Compression](compression.md) — what each compressed format supports

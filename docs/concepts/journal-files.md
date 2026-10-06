# Journal files

systemd-journald keeps its log in binary files (`system.journal`, its
`system@….journal` archives, `user-1000.journal`) that only `journalctl`
reads. `rx` reads them itself, with no `journalctl` and on every system
`rx` runs on, macOS included. A journal is read as a compressed file
is: `rx` decodes it to a text, one line or more for each entry, and
every command works on that text. Line numbers and byte offsets in every
answer are positions in it, as they are in the decompressed text of a
gzip file.

This page says what `rx` reads, what the text is and where it differs
from `journalctl`'s, what happens when a journal is damaged or still
being written, how rotated journals form a chain, and what is not
supported.

## What rx reads

**A journal is recognized by its first bytes, never by its name.** A file
that starts with the 8 bytes `LPKSHHRH` is a journal. A journal named
`app.log` is a journal, and a text file named `system.journal` is text:

```bash
cp demo.journal app.log
rx samples app.log --lines=1 --context=0
```

```text
File: app.log
Compressed: journal
Context: 0 before, 0 after

=== app.log:1:0 ===
2026-01-15T08:00:01.000000+00:00 fixture-host demo-app[4101]: basic 1: service started

```

Every way journald stores a journal is read: the regular and the
compact layout, and values stored plain or compressed with xz, lz4 or
zstd. A gzip copy of a journal (`system.journal.gz`) is not read: its
decompressed bytes start with the journal header, which holds NUL bytes,
so it is refused as a binary file like any other.

**Commands.** A journal is a stream format, read from its start like a
gzip file (see [Compression](compression.md)). With that, every command
works on it:

| Command | On a journal |
|---|---|
| `rx trace` | Searches the text with one ripgrep. A directory search includes journals. Matches carry the line numbers and offsets of the text |
| `rx samples` | Lines, offsets and times, read from the start of the text. The header of the answer says `Compressed: journal`; `--json` says `"is_compressed": true` and `"compression_format": "journal"` |
| `rx index`, `rx time-range` | The [line index](line-indexes.md) of the text (`file_type: "compressed"`), with a `journal` section ([below](#damage-and-its-report)) |
| `rx trace --signatures`, `rx scan` | The [signatures](signatures.md) scan the text |
| `rx compress` | Writes the text as [seekable zstd](compression.md#seekable-zstd). The default output name is the journal's whole name with `.zst` added: `system.journal.zst` |
| `rx logs` | Rotated journals read as one [chain](#chains-of-rotated-journals) |
| `rx serve` | The same operations over HTTP; `GET /v1/tree` lists a journal with `compression_format: "journal"` |

The trace cache does not hold a journal, as it holds no gzip file.
Because a journal cannot be entered in the middle, `rx samples --lines`
and `--offsets` read the text from its start up to what they look for,
as they do in a gzip file (see [`rx samples`](../cli/samples.md)).

**A journal rx cannot read is refused whole.** Only a header rx cannot
use does that: a file shorter than 208 bytes, a `header_size` below 208
or past the end of the file, a state other than offline, online or
archived, or an incompatible flag rx does not know. Every command
refuses it as not text, with the reason:

```text
Error: Not a text file: a systemd journal rx cannot read: incompatible flags 0x80 hold bits rx does not know (0x80): app.journal
```

`rx trace` and `rx index` skip it and say why, `GET /v1/tree` lists it
with `is_text: false` and the same words in `not_text_reason`. A
header that is fine but promises more than the file holds is not a
refusal: it is a copy cut short, and it is read as far as it goes (see
[damage](#damage-and-its-report)).

## The text

The text of a journal is, entry by entry, what

```bash
journalctl --file=system.journal --no-pager --utc -q -o short-iso-precise
```

of systemd 257 prints: times in UTC with microseconds, and none of the
`-- Boot … --` markers that `journalctl` prints without `-q`. `rx` keeps
the text of systemd 257 whatever later versions of systemd print, so
that line numbers do not change under a stored index. Here are the first
six lines of a small invented journal:

```bash
rx samples demo.journal --lines=1-6
```

```text
File: demo.journal
Compressed: journal
Context: 3 before, 3 after

=== demo.journal:1-6 ===
2026-01-15T08:00:01.000000+00:00 fixture-host demo-app[4101]: basic 1: service started
2026-01-15T08:00:02.001237+00:00 fixture-host demo-app[4101]: basic 2: a message of three lines
                                                              basic 2, second line
                                                              basic 2, third line
2026-01-15T08:00:03.002474+00:00 fixture-host demo-app[4101]: [40B blob data]
2026-01-15T08:00:04.003711+00:00 fixture-host demo-app[4101]: basic 4: column        value red text plain again

```

- Lines 2 to 4 are one entry. A message of several lines continues
  under the first one, indented to the column where the message starts.
- Line 5 is an entry whose message holds a NUL byte, which no text can
  show. It is replaced by `[40B blob data]`, the size in bytes.
- Line 6 is an entry whose message held a tab and a color sequence. The
  tab became 8 spaces and the color sequence was removed.

**The rules, in short.** Each line is made of the same parts, in this
order, from the entry's fields:

| Part | Rule |
|---|---|
| Time | The entry's `_SOURCE_REALTIME_TIMESTAMP`, when it reads as a number (decimal, or hexadecimal, octal or binary with the usual prefixes) of at least 1 and below 2^55 microseconds; otherwise the time journald stamped on the entry. Always 32 bytes: `2026-01-15T08:00:01.000000+00:00` |
| Host | A space and `_HOSTNAME`, when the value is under 300 bytes and valid printable UTF-8; otherwise nothing. An empty host prints the space alone |
| Name | A space and `SYSLOG_IDENTIFIER`, else `_COMM`, each under the same condition; else ` unknown` |
| Process id | `[` and `_PID`, else `SYSLOG_PID`, and `]`, under the same condition; else nothing. An empty id prints `[]` |
| Separator | `: ` |
| Message | `MESSAGE`, after the changes below |

An entry with the same field twice shows the last of its values in the
order the journal stores them. The changes to the message:

- A tab becomes 8 spaces, wherever it is.
- Color sequences (`ESC [`, digits and `;`, then `m`) and operating
  system command sequences (`ESC ]` up to a bell or `ESC \`) are
  removed.
- A carriage return is dropped when a line feed follows it or the
  message ends; any other one makes the message a blob.
- A message that still holds a control character, an ESC, or bytes that
  are not valid UTF-8 is not shown. The line says
  `[<size> blob data]` instead, with the size counted after the changes
  above, in systemd's units: `1023B`, `1K`, `1.1K`, `1.5M`.
- The lines of a message after the first are indented by the number of
  bytes of the time, host, name and process id, plus 2 for the
  separator. A name printed as ` unknown` counts as nothing, as in
  `journalctl`. The last line feed of a message adds no line.

No length limit, line limit or width applies: a message of a megabyte is
one line of a megabyte.

### Entries not shown

An entry without a `MESSAGE` field is not in the text, as `journalctl`
shows nothing for it either. An entry with an empty `MESSAGE` is: its
line ends with the separator. The index counts the entries without a
message as `without_message`. A damaged entry is not in the text
either, for another reason (see [below](#damage-and-its-report)).

### Where the text differs from journalctl's

The text is `journalctl`'s for every journal `journalctl` can print,
with six differences that `rx` chose:

- An entry whose text would be larger than 256 MiB is left out as
  damaged.
- An entry whose shown values come to more than 128 MiB together is left
  out as damaged. Every item of a field the text shows counts, so a
  field the entry lists twice counts twice, though the text prints it
  once.
- An xz value of a field the text shows whose stream declares a
  dictionary above 16 MiB is damage. journald writes its xz values with
  a 1 MiB dictionary.
- A field the text does not show is read only as far as its name. The
  text shows seven fields (`MESSAGE`, `_HOSTNAME`, `SYSLOG_IDENTIFIER`,
  `_COMM`, `_PID`, `SYSLOG_PID` and `_SOURCE_REALTIME_TIMESTAMP`), so
  damage in the rest of the value of any other field is not noticed, and
  a core dump of any size stored in the journal costs the length of its
  name. The size bounds of the first three points apply to the values
  the text shows; a field the text does not show is never damage for its
  size, nor for the dictionary its xz stream declares, except that a zstd
  value whose frame declares a window above 128 MiB is damage (crafted
  input only; journald writes windows of a few MiB).
- Finding a field's name may need the start of its compressed value. A
  name that needs more than 129 KiB of the stored bytes is damage. Only
  an lz4 value whose text starts with an incompressible run of more than
  about 32 MiB can do that.
- A damaged entry is left out and the others are shown. `journalctl` may
  refuse the whole file.

## Damage and its report

A journal that was cut short by an interrupted copy, or that a disk
damaged, is read as far as it can be, and `rx` says what it left out.

**What counts as damage.** An object that lies outside the file or
outside the part the header says holds objects, that is misaligned, of
the wrong type or of a size that does not fit; an offset that goes back
or stays where it was; a value that does not decompress or decodes to
more than 128 MiB; a zstd or lz4 value that declares more text than its
stored bytes can hold; an xz value of a field the text shows whose
stream declares a dictionary above 16 MiB; an entry with no valid time;
an entry whose shown values, every item of a field the text shows
counted each time the entry lists it, come to more than 128 MiB
together; an entry whose text would be larger than 256 MiB.

**What is left out.** An entry with a damaged object (the entry itself
or any of its fields) is left out whole, because the damaged field could
be its message. Reading goes on with the next entry.

**What ends the list.** The entries are read in the order of the
journal's entry list, a chain of arrays of entry offsets. A damaged
array ends the list, and so does a list that ends before the header's
count: the entries after that point are *not reached*.

The text of a damaged journal is still a fixed function of the file's
bytes. Its line numbers are exact, and it can be indexed, scanned,
searched and made part of a chain like any other.

**The report.** The index of a journal records what a read of its text
found, in the `journal` object. For a journal cut off in the middle (a
copy of `demo.journal` that stops after 3,740,000 bytes):

```bash
head -c 3740000 demo.journal > cut.journal
rx index cut.journal --threshold=0 --force > /dev/null
rx index cut.journal --info | tail -n 8
```

```text
  journal:
    state: offline
    entries: 15
    shown: 9
    without_message: 1
    left_out: 2
    not_reached: 3
    first_damaged_entry: 11 (the ENTRY object at offset 3740200 lies beyond the end of the file (3740000 bytes))
```

(`--threshold=0` indexes a file below the size threshold; a real journal
is larger.) The counts add up: `entries` (the number the header gives)
is `shown` plus `without_message` plus `left_out` plus `not_reached`.
`rx index --json` and `GET /v1/index` give the object as `journal`, with
the header's `compact`, `seqnum_id`, `head_seqnum` and `tail_seqnum`
too:

```bash
rx index cut.journal --info --json | jq .journal
```

```json
{
  "entries": 15,
  "shown": 9,
  "without_message": 1,
  "left_out": 2,
  "not_reached": 3,
  "first_damaged_entry": 11,
  "damage_reason": "the ENTRY object at offset 3740200 lies beyond the end of the file (3740000 bytes)",
  "state": "offline",
  "compact": false,
  "seqnum_id": "f8a07413e8824f928b3c8685ba71729c",
  "head_seqnum": 1,
  "tail_seqnum": 15
}
```

`journal` is `null` for a file that is not a journal, and the other
answers (samples, trace, findings) do not repeat it. See
[`rx index`](../cli/line-index.md#the-journal-section) and
[`GET /v1/index`](../api/endpoints/line-index.md).

**In a search.** `rx trace` lists a damaged journal it read to the end of
its text in `skipped_files`, once, and keeps its matches with their line
numbers:

```bash
rx trace "basic (7|10)" cut.journal 2> /dev/null | tail -n +5
```

```text
Files skipped: 1
  cut.journal: not searched in full: a damaged journal; 2 entries left out, 3 not reached; the other entries are searched
Matches: 2

Matches (file:line:offset [pattern]):
  cut.journal:8:601 [basic (7|10)]
  cut.journal:11:973 [basic (7|10)]
```

A `journal_damaged` warning goes to the log with the path and both
counts. A search that a `--max-results` cap stops before the end of the
journal's text does not list it. The reason also appears in
`skip_reasons` of `rx trace --json` and `GET /v1/trace`.

**In the viewer.** From viewer 0.9.0 on, the editor header of an open
journal shows a warning such as `3 damaged entries left out · 40 entries
not reached`. Its tooltip repeats the chip's text, then gives the reason
and the first damaged entry; on a narrow pane the chip's text is cut
short, and the tooltip holds all of it.

## Live journals

journald writes the active `system.journal` in place. It reserves the
file in 8 MiB steps, so most of a live journal after its last objects is
zeros that `rx` never reads. The progress of an index build
(`progress` of its task) and of a findings scan (`bytes_read` and
`bytes_total` of its task) counts to the end of the journal's last
object, not to the end of the file.

`rx` reads the entries the header counts at the moment it opens the file.
An entry journald adds meanwhile belongs to the next read.

**A live journal is decoded again from its start after every write.**
After each write journald changes the file's modification time and the
counts in its header, within a quarter of a second. Every cache of the
file (its line index, its findings) describes one state of the file, so
each write makes them stale, and the next command that needs one builds
it again from the first entry. A journal is a stream format, so rx cannot
extend an index from the last entry. The cost is the decode of the whole
active file, up to the size at which journald rotates it, after every
write; journald may write every second on a busy host, and every samples
request, findings request or chain description that touches the active
file pays it. A rotated archive does not change, and its caches
last. A check that reads only a file's metadata can lag a write by up to
that quarter of a second.

## Chains of rotated journals

When journald rotates, it renames `system.journal` to
`system@{id}-{seqnum}-{time}.journal` and starts a new one. These files
form a log chain, as rotated text logs do: `rx logs` reads them as one
text with one numbering.

```bash
rx logs list /srv/journals
```

```text
/srv/journals: 1 chain
NAME            PARTS  SIZE       IDX  MISSING
system.journal  3      721.47 KB  -    -
```

The rules are those of [Log chains](log-chains.md#rotated-journals);
in short:

- **The order is the header order.** The times a journal's text shows
  may go back (a clock set back, a program that sends its own time), so
  a chain is ordered as systemd orders journals, by the headers of its
  parts: the parts of one counter (`seqnum_id`) by the sequence number of
  their first entry, and the counters by the time of their first entry.
  The order needs no index.
- **A copy of a part makes the chain invalid.** Two neighbors of one
  counter that hold entries in common (the earlier ends at or past the
  entry the later starts at) fail the check `entries_overlap`, which a
  copy of a part left in the directory causes. `rx logs show` then
  exits 6 and names both parts:

    ```bash
    cp /srv/journals/system@b6423d6d2ca543b08b82cd46fafca6e4-0000000000000001-00065db7fbffd8e8.journal \
       /srv/journals/system@b6423d6d2ca543b08b82cd46fafca6e4-0000000000000001-00065db7fbffd8e9.journal
    ```

    ```text
    $ rx logs show /srv/journals/system.journal
    /srv/journals/system.journal: invalid, 4 parts, fingerprint 9b2c4d6e8f0a1c3e, times in UTC
    reason entries_overlap: system@b6423d6d2ca543b08b82cd46fafca6e4-0000000000000001-00065db7fbffd8e8.journal ends with entry 5 of its counter, which is not before entry 1, the first of system@b6423d6d2ca543b08b82cd46fafca6e4-0000000000000001-00065db7fbffd8e9.journal: the two parts hold the same entries, as a copy of a part does
    ...
    ```

    The other checks are `active_not_last` and `unreadable` (a part that is
    not a journal, or whose header `rx` cannot read). The check on times
    that overlap does not apply to a journal chain, and no part is missing
    from one: the numbers between two archives belong to journald's other
    journals.
- **The text of a chain** is the texts of its parts one after the other.
  For the parts of one counter that is what `journalctl` prints over all
  of them (`journalctl --file=P1 --file=P2 …`). For the parts of two
  counters it is not: `journalctl` interleaves their entries by time,
  and `rx` gives each counter's entries in one block, in the order
  above. The counters differ, for instance, after journald started
  without its earlier state.
- **Which files are parts.** `system.journal` and its archives; a file of
  journald's `….journal~` kind, which it makes when it disposes of a
  damaged journal, stays a single file; a `.journal.zst` copy of a
  journal's text matches no journal name, so it is not a part.
- A damaged journal rx can read stays in its chain, its line index
  counting the entries it lacks. A file that is not a readable journal
  makes the chain invalid, with `unreadable`.

## Not supported

- A merged view of all the journals of a machine (`system` and every
  `user-*`), interleaved by time as plain `journalctl` shows them.
  Each chain is read on its own.
- Jumping to an entry without reading the journal from its start. The
  index holds line checkpoints in the text, not entry numbers.
- Extending the index of a growing journal from its last entry instead
  of building it again after a write.
- A journal inside another compression (`system.journal.gz`): its
  decompressed bytes are not text, so it is refused as a binary file.
- Other layouts of `journalctl` (`json`, `export`, `verbose`, `cat`),
  a search by field, and reading the `PRIORITY` of an entry as a
  severity in the findings.
- Verifying the seals of a sealed journal. Sealed journals are read; the
  seals are ignored.
- Writing a journal.

## Related concepts

- [Compression](compression.md) — how rx reads a stream format
- [Line indexes](line-indexes.md#the-journal-section) — the `journal`
  section of an index
- [Log chains](log-chains.md#rotated-journals) — rotated journals
- [Security](security.md#reading-a-systemd-journal) — what the reader
  guarantees on hostile input

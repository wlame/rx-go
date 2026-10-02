# Signatures

A **signature** is a named definition of something worth noticing in a
long file: a Python traceback, an AWS access key, a run of blank lines.
rx has built-in signatures and reads more from YAML files you write.
[`rx signatures`](../cli/signatures.md) lists them, shows one, checks
your files and prints a template to start from.

This page is the reference of the signature model and of the file
format.

## Words

| Word | Meaning |
|---|---|
| signature | a named definition, built in or from a user file |
| finding | one place in one file where a signature matched |
| group | the one bucket of a signature, for listing and selection (`traceback`, `secret`, …) |
| tag | extra words of a signature, any number, for selection |
| level | how much a finding matters: `info`, `low`, `medium`, `high`, `critical` |
| anchor | the line regex that opens a finding: `regex`, a block's `start`, a run's `match` |
| keyword | a literal of 3 bytes or more that every anchor line contains |
| line pass | the walk over every line, for signatures that no keyword can gate |
| default set | every signature without `default: false` |

## A signature file

`rx signatures template` prints a commented file with one signature of
each kind. A shorter one:

```yaml
disable: [blank-run]          # built-ins to switch off, by id

groups:                       # groups of your own
  - name: billing
    description: Events of the billing service.

signatures:
  - id: invoice-failed
    name: Invoice failed
    group: billing
    tags: [invoices]
    level: high
    description: An invoice the billing service could not create.
    regex: '\binvoice \d+ failed\b'
    examples:
      match: ['2026-01-02 10:00:00 ERROR invoice 812 failed: card expired']
      no_match: ['2026-01-02 10:00:01 INFO invoice 813 sent']

  - id: worker-dump
    name: Worker dump
    group: billing
    description: The state a worker prints when it stops.
    block:
      start: '^=== worker dump ==='
      continue: '^\s'
      end: '^=== end of dump ==='

  - id: billing-burst
    name: Burst of billing lines
    group: billing
    description: A minute with five times the median number of lines or more.
    measure: {name: density, bucket: 1m, factor: 5}
```

A file holds three keys at its top, all optional: `signatures`,
`disable` and `groups`. Any other key refuses the file.

## Common fields

| Field | Required | Default | Meaning |
|---|---|---|---|
| `id` | yes | — | kebab-case (`^[a-z0-9]+(-[a-z0-9]+)*$`), at most 64 bytes, unique; `all` and `default` are reserved |
| `name` | yes | — | a short title for lists |
| `group` | yes | — | a built-in group or one declared under `groups:` |
| `description` | yes | — | free text; several lines with YAML `|` |
| `tags` | no | `[]` | kebab-case words; `all` and `default` are reserved |
| `level` | no | `medium` | `info`, `low`, `medium`, `high` or `critical` |
| `links` | no | `[]` | `http` or `https` URLs, shown, never fetched |
| `metadata` | no | `{}` | a map of text to text, shown, never interpreted |
| `default` | no | `true` | `false`: the signature runs only when a selection names it |
| `redact` | no | `false` | mask the value of every finding in every output; refused on a `blank` or `same` run and on a measure other than `entropy` and `base64-run`, which have no value |
| `keywords` | no | derived | literals for the [keyword gate](#keywords-and-the-gate) |
| `examples` | no (yes for built-ins) | — | `match:` and `no_match:` lists of texts; each text is one or more lines |

A field the table does not list refuses the signature, and so does a
field given twice. YAML anchors and aliases work for any value; merge
keys (`<<`) do not.

Every signature has exactly one of the four kind keys: `regex`,
`block`, `run` or `measure`. None, or two, refuses it.

## Kinds

### `regex`: one line

| Field | Type | Meaning |
|---|---|---|
| `regex` | regex | the anchor: a line matches when the regex finds a match in it |
| `not` | regex | the line must not match it (the RE2 way to say "not followed by") |
| `capture` | whole number, at least 0 | which group of the regex is the value; `0` (the default) is the whole match; the first match on the line is used |
| `entropy` | number, 0 to 8 | the least Shannon entropy, in bits per byte, the value must have |
| `validate` | `luhn`, `iban`, `ipv4`, `ipv6` | a check on the value after the regex |

These fields sit at the top of the signature, beside `regex`; on a
signature of another kind they refuse it.

Regexes are Go's [RE2 syntax](https://github.com/google/re2/wiki/Syntax):
no lookaround and no backreferences, so a scan takes linear time on any
line. A regex is matched against one line without its line ending. Any
regex field also takes a list, which means "any of these":
`['^panic: ', '^fatal error: ']` runs as `(?:^panic: )|(?:^fatal error: )`.

### `block`: a start line and the lines after it

The **simple form** names the start line and how the block goes on:

```yaml
block:
  start: '^Traceback \(most recent call last\):'
  continue: '^\s'                  # a line that joins
  end: '^[A-Za-z_][\w.]*(?::|$)'   # a line that joins and closes the block
```

`start` is the anchor. Each following line, in order:

1. if `end` matches, the line joins and the block closes;
2. else if `continue` matches, the line joins;
3. else the block closes before the line.

With only `end`, every line joins until `end` matches. With
`lines: N` (1 to 10,000) the block is the start line and the next N
lines, whatever they hold; `continue` and `end` are then not allowed,
and `max_lines` must be at least N + 1. A block with `start` alone is
refused: it would never end.

The **states form** is for a block of several parts:

```yaml
block:
  states:
    - {state: start, regex: '^Traceback \(most recent call last\):', next: frames}
    - {state: frames, regex: '^\s', next: frames}
    - {state: frames, regex: '^[A-Za-z_][\w.]*(?::|$)', end: true}
```

A block is in one state. For each following line the rules of that
state are tried in order; the first that matches joins the line and
moves the block to `next`, or closes it after the line when `end: true`.
When no rule of the state matches, the block closes before the line. A
rule with neither `next` nor `end` stays in its state. The first rule's
state must be `start`, and the regexes of the `start` rules are the
anchor; every `next` must name a state that has rules. The simple form
is the states form written short: rx compiles it to `start` and `body`
rules.

Every block also takes:

| Field | Default | Meaning |
|---|---|---|
| `max_lines` | `200` | 1 to 10,000: the block closes after this many lines, the start line included |
| `min_lines` | `1` | a block shorter than this is not a finding |
| `not` | — | a regex the start line must not match |

### `run`: consecutive lines

```yaml
run:
  blank: true      # or: match: '<regex>', or: same: true
  min: 3
```

A run is at least `min` (2 to 1,000,000) consecutive lines that each
qualify, by exactly one of:

- `match`: the line matches the regex (and not `not`, which only
  `match` takes);
- `blank: true`: the line is empty or holds only spaces and tabs;
- `same: true`: the line is equal to the line before it.

### `measure`: Go code with parameters

```yaml
measure: {name: long-line, factor: 50, min_bytes: 4096}
```

`name` is one of the measures below; every other key is one of its
parameters. A parameter left out takes its default. A duration is a
number and a unit: `500ms`, `60s`, `5m`, `2h` or `1d`, at most `3650d`,
in whole milliseconds; a negative, longer or finer one refuses the
signature.

| Measure | Parameter | Default | Range |
|---|---|---|---|
| `long-line` | `window` | `100` | 1 to 10000 |
| `long-line` | `factor` | `10` | 1 to 1000 |
| `long-line` | `min_bytes` | `1024` | 1 to 1073741824 |
| `binary` | — | — | — |
| `entropy` | `min_length` | `20` | 8 to 1024 |
| `entropy` | `base64` | `4.5` | 1 to 6 |
| `entropy` | `hex` | `3` | 1 to 4 |
| `json-block` | `max_lines` | `200` | 2 to 10000 |
| `unprefixed` | `min` | `5` | 1 to 1000000 |
| `unprefixed` | `min_share` | `0.5` | 0 to 1 |
| `base64-run` | `min_length` | `100` | 16 to 1048576 |
| `density` | `bucket` | `auto` | `auto`, or 1s to 7d |
| `density` | `factor` | `5` | 1 to 1000 |
| `density` | `z` | `3.5` | 0 to 100 |
| `density` | `min_count` | `100` | 1 to 1000000000 |
| `time-gap` | `min_gap` | `1m` | 1ms to 365d |
| `time-gap` | `factor` | `50` | 1 to 10000 |
| `time-backwards` | `tolerance` | `1s` | 0s to 1d |

What each measure finds:

| Measure | Finding |
|---|---|
| `long-line` | a line whose length is at least `max(min_bytes, factor × the median length of the window lines on each side)` |
| `binary` | a run of lines that hold a NUL byte or bytes that are not UTF-8 |
| `entropy` | a token of `min_length` or more base64-like characters whose entropy reaches `base64` bits per byte (`hex` for a token of hex digits only); UUIDs and tokens of digits only are skipped |
| `json-block` | a line that is only `{` or `[`, through its matching closer at the same indent |
| `unprefixed` | a run of `min` or more lines without a timestamp at their start, when stamped lines are at least `min_share` of the file |
| `base64-run` | a run of at least `min_length` base64 bytes inside a line |
| `density` | consecutive time buckets with a burst of lines: at least `factor` × the median count, a modified z-score over `z` and at least `min_count` lines (`auto` aims at about 1,000 buckets over the file's span) |
| `time-gap` | a silence of at least `max(min_gap, factor × the median interval)` between two stamped lines |
| `time-backwards` | a line stamped earlier than the stamped line before it by more than `tolerance` |

How each measure decides, in detail:

- **`long-line`** compares a line's full length (without its ending)
  with its neighbors: the `window` lines before it and the `window`
  lines after it, fewer at the start and the end of the text. The line
  is long when its length is at least `min_bytes` and at least `factor`
  times the median of their lengths; the median of an even count is the
  lower of the two middle values. A text of one line has no long line.
  The windows reach across chunk edges: every chunking gives the same
  findings.
- **`json-block`** opens on a line that is only `{` or `[` (spaces and
  tabs around it allowed) and counts brackets on the following lines,
  outside JSON strings (a backslash in a string escapes the next
  character). It closes on the line where the count comes back to zero,
  when that line ends with the matching `}` or `]` at the opener's
  indent; a balance anywhere else, or more closers than openers,
  abandons it without a finding. It is a block in every other way: it
  owns its lines, `max_lines` closes it as a finding of that many lines,
  and the end of the text closes it.
- **`entropy`** cuts the line view into tokens, the longest runs of
  `A-Z a-z 0-9 + / = _ -`, and keeps those of `min_length` bytes or
  more. A UUID (`8-4-4-4-12` hex digits) and a token of digits only are
  skipped. A token of hex digits only needs `hex` bits per byte of
  Shannon entropy, any other `base64`. The first token that reaches its
  threshold is the finding's value; one finding per line. A random
  40-character key has about 5.3 bits per byte; a run of English words
  joined by `_` about 4.4.
- **`base64-run`** finds, by a scan of the bytes, a longest run of the
  base64 alphabet (`A-Z a-z 0-9 + /`) followed by up to two `=` of
  padding, `min_length` bytes or longer, padding included. The first
  such run is the finding's value; one finding per line. A run lies
  inside one line, so a chunk edge never cuts it.
- **`binary`** counts a line whose view holds a NUL byte or bytes that
  are not UTF-8 (a character that the end of a long line's view cuts in
  two is not counted); consecutive such lines are one finding. A file
  with a NUL byte in its first 8 KiB is not text and is not scanned at
  all, so this measure finds NUL bytes only after them.
- **`unprefixed`** counts a line that has no timestamp of its own, as the
  file's timestamp format reads it (see Times below); `min` or more
  consecutive such lines are one finding. When fewer than `min_share` of
  the file's lines have a timestamp, or the file has no timestamp
  format, the measure finds nothing: there, lines without one are the
  rule.

The three time measures read the file's timestamps as the scan does
for a finding's time (see Times below); in a file without a timestamp
format they find nothing.

- **`time-backwards`** compares each stamped line with the stamped line
  before it: a line stamped earlier by more than `tolerance` is a
  finding.
- **`time-gap`** looks at the interval between each two consecutive
  stamped lines, a step back counted as 0. An interval of at least
  `min_gap` and at least `factor` times the median interval is a
  silence. Its finding runs from the line just before the later stamped
  line (the earlier stamped line, or a line without a timestamp after
  it, such as the last line of a traceback) to that stamped line, and
  takes the time of its first line. The median is taken over all the
  intervals of the file, kept to three significant digits (exact under
  a second; 61,234 ms counts as 61,200). Until the median is known, a
  scan keeps every interval of at least `min_gap`, at most 65,536 of
  them in each chunk (or `RX_FINDINGS_MAX_PER_SIGNATURE`, when larger):
  a file with more such silences keeps only the longest ones, the same
  ones however the file is cut, and reports only silences longer than
  the longest one it dropped, so its count is then a lower bound.
- **`density`** counts every line, from the first stamped line on, in
  the time bucket of its own timestamp or, for a line without one, of
  the last stamped line before it. The bucket is `bucket`, or with
  `auto` the smallest of 1s, 5s, 15s, 30s, 1m, 5m, 15m, 30m, 1h, 3h,
  6h, 12h and 1d (each a whole multiple of the one before) that cuts
  the file's span (its latest time minus its earliest) into at most
  1,000 buckets; buckets
  start at multiples of their size since 1970. A series holds at most
  65,536 buckets: past that, the bucket doubles until it fits. A bucket
  is a burst when it holds at least `min_count` lines, at least
  `factor` times the median count, and its modified z-score
  `0.6745 × (count − median) / MAD` is over `z`. The median and the MAD
  (the median of the absolute deviations from it) are taken over the
  buckets that hold a line, so the empty buckets of a log that sleeps at
  night do not make every busy bucket a burst; when the MAD is 0, a
  count above the median is a burst by the z-score, and the median
  itself is not. Consecutive burst buckets are one finding, from the
  first line counted in any of them to the last, with the time of that
  first line. The series of every bucket's count over the span (the
  bucket size, the start of the first bucket, the counts) is kept with
  the findings, for a chart.

Every chunking and every compressed copy give the same findings and
the same series: each chunk reports its first and last stamped lines
and its counts, and the merge pairs and adds them in text order.

With `redact: true`, `entropy` and `base64-run` mask their value as a
regex signature masks its own, and they mask every excerpt of a line
that holds such a value, as every redacting signature does.

## Keywords and the gate

Most lines of a log match no signature, and running every anchor regex
on every line would be slow. So each anchor has **keyword clauses**:
sets of literals, each literal 3 bytes or more, such that every line
the anchor matches holds a member of every clause. A line is tested
against the anchor only when every clause has a member on it, compared
ASCII case-insensitively. One pass over the line finds all the keywords
of all signatures at once.

The gate is part of what a signature means: a line that fails it is
not a finding of the signature, whatever the regex says. Derived
keywords never fail a line the regex matches (a line that holds the
Kelvin sign or the long s skips the gate, under "Case" below), so for
a signature without declared keywords the gate only makes the scan
faster.

**Derived keywords.** Without a `keywords` field, rx derives the
clauses from the anchor regex:

- a literal gives a clause of itself, lowered;
- a sequence joins adjacent literals and alternations into longer
  literals (`(AKIA|ASIA)` gives `akia, asia`; `A(?:3T[A-Z0-9]|KIA)`
  gives `a3t, akia`, because every match of the alternation starts with
  `3T` or `KIA`), as long as the result is at most 64 literals of at
  most 32 bytes; every other part with a clause gives its own;
- an alternation gives one clause, the union of the best clause of
  each branch, or none when a branch has none;
- a repeat that must happen at least once (`+`, `{2,}`) gives the
  clauses of what it repeats; `*`, `?` and character classes give none;
- a clause with a member under 3 bytes is dropped (a two-byte keyword
  is on almost every line), and so is a clause another one implies.

| Anchor | Keyword clauses |
|---|---|
| `^Traceback \(most recent call last\):` | `[traceback (most recent call last):]` |
| `(?i)\bdelete\s+from` | `[delete] AND [from]` |
| `\bUPDATE\s+\S+\s+SET\b` | `[update] AND [set]` |
| `(AKIA|ASIA)[A-Z2-7]{16}` | `[akia, asia]` |
| `(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}` | `[a3t, akia, asia, abia, acca]` |
| `foo(?:[0-9]bar|baz)` | `[foo] AND [bar, baz]` |
| `foo|ba` | none (`ba` is under 3 bytes) |
| `\d+\.\d+` | none |

A signature with no clause runs in the **line pass**: its anchor is
tested on every line. That is slower, never wrong. `rx signatures
check` warns about such a signature (`no keywords: runs in the line
pass`), and `rx signatures show` says which clauses a signature has. A
measure and a `blank` or `same` run have no anchor and always see every
line.

**Declared keywords.** A `keywords: [word, …]` list replaces the derived
clauses with one clause of its own: a line passes when it holds any of
the words. Each word must be 3 bytes or more; `keywords: []` declares no
gate at all. A declaration that leaves out a word some matching line
needs makes the signature miss that line; the examples check catches it
when an example shows such a line.

**Case.** The gate is ASCII case-insensitive by definition: it compares
ASCII letters without case and every other byte exactly. A letter
outside ASCII that has another case, such as `é` in a case-insensitive
(`(?i)`) anchor, gives no keyword: the clause is cut there.

Under `(?i)`, Go's regexp also matches `k` to the Kelvin sign (U+212A)
and `s` to the long s (U+017F), and no keyword holds either character.
So a line whose view holds one of them skips the gate: every anchor is
tested on it, one with declared keywords too. `(?i)select` gives the
keyword `select`, and the line `ſelect 1` is still a finding of that
signature, as the regex alone would make it. The same rule holds for
redaction: a redacting `(?i)key=(\w+)` masks the value of `Key=…`
written with a Kelvin sign. The keyword pass finds the two characters
itself, so the rule reads no byte twice; it costs anchor tests only on
the rare lines that hold them.

## How a scan reads a file

A scan reads the file's text once, in parallel, for every selected
signature at the same time:

- a plain file, or a seekable zstd file, is cut into chunks of whole
  lines, about eight per worker and at least 4 MiB each, read by
  `RX_WORKERS` workers at once;
- a gzip, bzip2, xz or zstd stream is decoded once, by one reader that
  hands batches of whole lines (about 4 MiB each) to the workers.

The findings are the same whatever the number of workers, the chunk
sizes and the way the file is stored: a plain file and its compressed
copies give identical findings, field by field.

**The line view.** Every matcher sees a line as its view: the line
without its ending (`\n` or `\r\n`), cut at `RX_SIGNATURES_LINE_BYTES`
bytes (64 KiB by default). A UTF-8 byte-order mark at the start of the
text is not part of line 1's view, so `^Traceback` matches a first line
that starts with one; offsets still count its 3 bytes. A longer line is
matched on its view and counted at its full length, and the scan never
holds more of it than the view. A line is **blank** when its view is
empty or holds only spaces and tabs, so a `\r\n` line is blank.

**Numbers.** A finding's lines are 1-based line numbers in the text, the
numbers `rx samples --lines=` takes. Its offsets are byte positions in
the text (the decompressed text of a compressed file), the end offset
exclusive: it is where the line after the finding starts, or the end of
the text after a last line without a line break.

**Regex signatures.** For each line, one pass finds the keywords of every
signature (the gate above); the anchor of a signature runs only on a
line that passes its gate, then `not`. The first match on the line
decides the value (its `capture` group), `entropy` (Shannon entropy of
the value, in bits per byte) and `validate`:

| `validate` | The value passes when it is |
|---|---|
| `luhn` | digits that pass the Luhn checksum, spaces and dashes between them skipped, at least two |
| `iban` | an IBAN: a country code that issues them, two check digits, that country's length (spaces skipped), and the ISO 13616 checksum (modulo 97) |
| `ipv4` | four decimal numbers 0 to 255, without leading zeros |
| `ipv6` | an IPv6 address, an IPv4-mapped form and a zone included |

A signature gives at most one finding per line.

**Blocks.** The gate applies to the start line only: the lines of an
open block are tried against the rules of its state, whatever keywords
they hold. While a block is open, every line goes to it; only a line
the block refused (it closed before the line) may start another block
of the same signature, and the line that closed a block by `end` or
`max_lines` may not. When a block closes, its trailing blank lines are
dropped; it is then a finding if it still has `min_lines` lines, and
either way it owned the lines it took. The end of the text closes an
open block.

**Runs.** A `match` line passes the gate, matches and does not match
`not`. A `blank` line is blank. A `same` run is a group of consecutive
equal lines, the first of them included: two lines are equal when
their views and their full lengths are. A run is a finding when it has
`min` lines or more.

**Chunk edges.** A block or a run may cross the edge between two
chunks. The scan joins a run's two halves, and finishes a block that
crosses into the next chunk by walking that chunk's lines again from its
start until the walk agrees with the chunk's own; so a finding is never
cut or doubled by an edge.

**Excerpt and redaction.** A finding's excerpt is its first line's view,
cut to 200 bytes on a UTF-8 character boundary. It is always valid
UTF-8: each byte that is not part of a UTF-8 character becomes U+FFFD
(`�`), after masking and before the cut, so a `binary` finding's
excerpt shows replacement characters, and every output and the cache
hold the same text. The file itself stays readable byte for byte
through `rx samples`. For a signature with
`redact: true`, every secret on that line is masked in the excerpt
before the excerpt is kept: the value of every match of the anchor (the
`capture` group of a regex signature, or the whole match when the group
took no part in it; the whole match of a block's or a run's anchor), and
every other copy of such a value on the line, so a secret echoed later
without its `key=` is hidden as well. Copies that overlap are masked as
one. A mask keeps a quarter of the value's characters, at most 4 (a
20-character key shows 4, an 8-character value 2, a 3-character value
none), and turns the rest into `*`, at most 12 of them
(`AKIA************`).

Redaction fails closed. A line whose secrets cannot all be followed
within its bounds (more than 8 distinct values, or a value longer than
256 bytes) shows only its text before the first secret, at most 256
bytes, and is masked from there on; a line with more than 64 distinct
values or 4,096 matches is masked whole. The unmasked value is never
kept.

Every excerpt is masked by every redacting signature rx has loaded,
whether the scan selected it or not, default or opt-in: an `error-line`
finding on a line that holds an AWS key shows the key masked, and the
finding is marked masked. A redacting signature takes part only on a
line that passes its keyword gate (a line that holds the Kelvin sign or
the long s passes every gate), and only where an excerpt is kept.

**Bodies of redacting blocks and runs.** The key material of a PEM
private key is in the lines after its `BEGIN` line, and those lines hold
no value a mask could find. So every scan also walks every redacting
`block` and `run` signature rx has loaded, selected or not, gated by its
keywords, and notes where their bodies lie: every line a block joined
after its anchor line (a block shorter than its `min_lines`, which is no
finding, and the blank lines it joined at its end, included), and every
line of a run after its first. A finding of any signature whose first
line is a body line has the excerpt `[masked, N bytes]`, N the line's
length in bytes without its line ending, and is marked masked: a
`base64-run` finding on a key line shows no byte of the key. A run's
lines all match its anchor, so a run too short to be a finding still has
its values masked on every line, as above. The bodies are found the same
way for every chunking and every storage of the file, however many
there are: a signature's first 100,000 bodies are noted line by line,
and past that every line from its last noted body to the end of the
file counts as a body (redaction fails closed).

**Times.** Every finding has a time: the timestamp of its first line,
or else that of the nearest earlier line with one, when that line
starts at most `RX_TIMESTAMP_LOOKBACK_KB` KiB before the finding's first
line (the rule of [effective timestamps](timestamps.md#effective-timestamps));
otherwise none, and none in a file without timestamps. A traceback two
lines after a stamped line takes that line's time. The scan reads the
file's timestamp format from the head of its text, and each line's
timestamp, with the functions an index build uses, but it never reads a
stored index: a finding's time is the time the index records for the
same line, whether the file has an index or not. A time is milliseconds
since the Unix epoch in the file's own frame, as the index keeps it:
the UTC instant for a file whose timestamps carry zones, the wall clock
as written for one whose timestamps carry none. A chunk that holds no
stamped line before a finding takes the last stamped line of the chunks
before it, by the same distance rule, so every chunking gives the same
time.

**The cap.** A scan keeps the first `RX_FINDINGS_MAX_PER_SIGNATURE`
findings of each signature (10,000 by default), by line; their count
stays exact past it.

## Examples

```yaml
examples:
  match:
    - |
      Traceback (most recent call last):
        File "app.py", line 3, in <module>
      ValueError: bad input
  no_match:
    - 'INFO traceback collection is off'
```

Each `match` text must give a finding of the signature and each
`no_match` text none; a text of several lines is read as lines. Built-in
signatures carry at least one of each. rx runs them through the scan
engine when it loads the signatures, each text as a file of its own:
a failing example refuses the signature, with its file, line and id
([`rx signatures check`](../cli/signatures.md#rx-signatures-check)
lists them).

## Where rx finds signature files

The built-in signatures are part of the binary. The user files are every
`*.yml` and `*.yaml` file in the first of:

1. the entries of `RX_SIGNATURES_PATH`, directories or files separated
   by `:`, in their order;
2. `$XDG_CONFIG_HOME/rx/signatures/`;
3. `~/.config/rx/signatures/`.

The two configuration directories are used when `RX_SIGNATURES_PATH` is
unset or empty, the first of them that exists. The files of a directory
load in name order; subdirectories and names starting with a dot are
skipped. A file named in `RX_SIGNATURES_PATH` loads whatever its name.
A directory or file that does not exist is not an error.

Only the commands that use signatures read these files; `rx samples`,
`rx index` and the others never do. Paths come from the environment
only.

### Overrides, disable and groups

- A user signature with the `id` of a built-in **replaces** it; the
  list marks it `overrides`. A user signature that would replace a
  built-in and is refused leaves neither: the built-in does not run in
  its place.
- The same `id` in two user files is an error of the later one; the
  first loads.
- `disable: [id, …]` switches built-ins off. A disabled built-in is
  listed (`disabled by file:line`) and never runs. An id that is not a
  built-in's is an error of the file; the rest of the file loads.
- `groups:` declares groups (`name`, `description`). They come after
  the built-in groups, in load order, and any user file may use them. A
  group declared twice is an error.

The built-in groups, in order: `traceback`, `crash`, `secret`, `pii`,
`sql`, `http`, `severity`, `system`, `lifecycle`, `structure`, `time`.

## Errors

| Problem | Refuses |
|---|---|
| a YAML syntax error; more than one YAML document; an alias expansion over the limit; a top level that is not a mapping; an unknown top-level key; any [limit](#limits) | the whole file |
| a bad regex; an unknown field; a field given twice; a bad value; no kind or two kinds; a block `start` alone; an unknown group; a duplicate id; a failing example | that signature |
| a bad entry of `groups:` or `disable:` | that entry |

Every error names the file, the line and the signature id (when there
is one). `rx signatures` lists them under "Errors", and
`rx signatures check` prints them and exits 1.

## Limits

| Limit | Value |
|---|---|
| size of a signature file | 1 MiB |
| user signatures in all files | 1,000 (the file that passes it is refused) |
| source of one regex field | 4 KiB |
| nodes YAML aliases may add to a file | 10,000 |
| `max_lines` of a block | 10,000 |

## Selection

A command that runs signatures takes a **selection**: comma-separated
words, compared without case, applied left to right:

- a word names an `id` first, then a group, then a tag; `group:x` and
  `tag:x` name only a group or only a tag;
- `all` is every signature, `default` the default set;
- `-word` removes what the word names;
- an empty selection is the default set.

```text
traceback,sql,my-check      a group, another group and one signature
all,-pii                    everything but the pii group
tag:postgres                every signature tagged postgres
default,slow-invoice        the default set and one opt-in signature
```

An unknown word is an error that lists up to five close names; a word
that names a refused signature repeats its load error; one that names a
disabled built-in says where it was disabled; a selection that ends up
naming nothing is an error too, and so is one of more than 1,000
words.

## Signatures from the CLI

`rx` and `rx trace` scan files for a selection with `--signatures`:

```bash
rx --signatures app.log                              # the default set
rx trace --signatures=traceback,sql app.log /var/log/
rx --signatures=all,-pii -e timeout app.log          # with a pattern
```

- With `--signatures`, every positional argument is a path; a pattern
  goes with `-e`. `rx --signatures=traceback ERROR app.log` reads
  `ERROR` as a file.
- Every text file under the paths is scanned through the
  [findings cache](caching.md#findings), each file with its own entries:
  a second run answers from the cache and scans only what it lacks;
  `--no-cache` reads and writes no entry.
- The answer lists every finding with its lines read from the file at
  that moment, masked as the excerpt is (see [How a scan reads a
  file](#how-a-scan-reads-a-file)): every value a loaded redacting
  signature matches on a line, and every body line of a loaded
  redacting block or run, which shows as `[masked, N bytes]` in the
  lines of any finding and in the lines before and after one (`-B`,
  `-A`, `--context`) alike. No line past the end of the text the scan
  read is shown: a log that grew since shows its new lines after its
  next scan. Each line
  is cut at `RX_MAX_LINE_TEXT_BYTES`, and the text of all findings
  together stops at `RX_FINDINGS_TEXT_MAX_BYTES`; the findings past it
  keep their place and say `lines_truncated`.
- The matches of `-e PATTERN` (`line_text`, `context_lines`) are not
  masked: they are the output of your own regex, as `rx trace` without
  `--signatures` prints it, not text of a finding. A pattern that
  matches a secret prints it.
- `--json` adds `signatures`, `findings` and `finding_summary` to the
  trace answer; a trace without `--signatures` answers as before.

The output, the JSON fields and the flags are on the
[`rx trace`](../cli/trace.md#signatures) page.

## See also

- [`rx signatures`](../cli/signatures.md) — list, show, check, template
- [Configuration](../configuration.md#signatures) — `RX_SIGNATURES_PATH`

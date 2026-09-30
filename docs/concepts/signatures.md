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
| `redact` | no | `false` | mask the value of every finding in every output |
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
number and a unit: `500ms`, `60s`, `5m`, `2h` or `1d`, at most `3650d`;
a negative or longer one refuses the signature.

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
keywords never fail a line the regex matches, with one exception (the
Kelvin sign and the long s, under "Case" below), so for most
signatures the gate only makes the scan faster.

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
and `s` to the long s (U+017F); the gate does not. A line that matches
a `(?i)` anchor only through one of these two characters is not found:
`(?i)select` gives the keyword `select`, so the line `ſelect 1` is not
a finding of that signature, though the regex alone would match it.

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
signatures carry at least one of each. When rx can run them, a failing
example refuses the signature; [`rx signatures
check`](../cli/signatures.md#rx-signatures-check) says whether they ran.

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

## See also

- [`rx signatures`](../cli/signatures.md) — list, show, check, template
- [Configuration](../configuration.md#signatures) — `RX_SIGNATURES_PATH`

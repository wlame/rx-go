# Concepts

Deep-dive topics that explain how `rx` works underneath the command line
and the HTTP surface. Read these once and you'll understand the
performance trade-offs, operational behavior, and how to extend the tool.

## Topics

<div class="grid cards" markdown>

- **[Chunking](chunking.md)**  
  The parallel file-scanning algorithm. Why it's newline-aligned, how
  workers coordinate, when chunking engages and when it doesn't.

- **[Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md)**  
  Why byte offsets are the default, when line numbers cost more, how
  indexes bridge the gap.

- **[Line indexes](line-indexes.md)**  
  Sparse checkpoint files that enable O(1) line-number-to-byte seeks.
  Structure, invalidation, when to build one.

- **[Caching](caching.md)**  
  On-disk cache layout, cache key scheme, mtime-based invalidation,
  and the `--no-cache` / `--no-index` escape hatches.

- **[Compression](compression.md)**  
  Supported read formats (gzip, bzip2, xz, zstd, seekable-zstd), the
  seekable-zstd write format, and the trade-offs between frame size
  and ratio.

- **[Analyzers](analyzers.md)**  
  The pluggable `FileAnalyzer` interface, the empty-at-v1 registry,
  and how to add a detector.

- **[Security](security.md)**  
  Search-root sandbox, tarball traversal defenses, webhook SSRF
  protection, threat model.

</div>

## Reading order

If you're new to `rx`, read in this order:

1. [Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md) — the
   foundational distinction that drives the rest of the design
2. [Chunking](chunking.md) — how a single file becomes parallel work
3. [Line indexes](line-indexes.md) — how `rx` makes line-number lookups
   fast
4. [Caching](caching.md) — where results live between runs
5. [Compression](compression.md) — what works on compressed data and
   what doesn't
6. [Security](security.md) — before you expose `rx serve` to anything
7. [Analyzers](analyzers.md) — only if you want to add custom file
   analysis

## See also

- [Performance](../performance.md) — measured numbers and tuning advice
- [CLI reference](../cli/index.md) — commands that use these concepts
- [HTTP API](../api/index.md) — the same feature surface over HTTP

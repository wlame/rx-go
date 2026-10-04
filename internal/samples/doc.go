// Package samples is the shared implementation of `rx samples` used by
// both the CLI (internal/clicommand) and the HTTP API
// (internal/webapi). Parity testing revealed that
// the CLI and HTTP implementations had forked — byte-offset mode didn't
// work in the CLI at all, and the HTTP implementation had an
// off-by-context bug in the Lines map.
//
// Design invariants enforced by this package:
//
//  1. Single resolver — both entry points call Resolve().
//  2. Byte-offset mode and line mode share the same range parser.
//  3. Multi-range queries (e.g. "200-350,450-600") are supported.
//  4. Line-mode queries can opt into index-aware seek when a unified
//     index is available; without the index, a linear scan fallback
//     behaves identically.
//  5. Python parity on the `lines` / `offsets` map: for single values
//     the offset is of the REQUESTED position (not the context window).
//     For ranges the offset is sentinel -1 in line mode and the start
//     line number in byte-offset mode.
//  6. One pass per request: every window a request names is read in
//     one pass over the text, in order of position, and a pass stops
//     at the next read once the Resolve context is canceled.
//
// This package deliberately has ZERO dependencies on internal/webapi or
// internal/clicommand to keep the dependency arrows pointing inward.
package samples

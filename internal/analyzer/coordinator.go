package analyzer

// This file implements the per-worker coordinator that drives the
// streaming scan. The index builder (Task 5) creates one Coordinator
// per chunk worker and calls ProcessLine for every line, then Finalize
// exactly once. The coordinator owns one Window and fans each line out
// to every registered LineDetector.
//
// Design points:
//
//   - Per-line overhead should be near-zero when no detectors are
//     registered — the hot index-build loop must stay unchanged for
//     users who don't opt into --analyze. The zero-detector fast path
//     skips the window push entirely.
//
//   - Detectors receive only a *Window (not a *LineEvent) so they can
//     look back at previous lines when needed. The most-recent event
//     is w.Current(); prior events are w.At(back).
//
//   - Precomputed fields (IndentPrefix, IsBinary) are computed once
//     per line by the coordinator and stored in the window slot. Every
//     detector reads them via w.Current() — the work isn't duplicated.
//
//   - Finalize returns a single flat slice of Anomaly. The caller
//     (index builder, Task 5) collects one slice per worker and then
//     runs the dedup pass (Task 4) across them.
//
// Thread safety: a Coordinator and its Window are NOT safe for
// concurrent use. One Coordinator per worker; workers run in parallel
// but never share state.

// Coordinator dispatches per-line events to a set of LineDetectors
// while maintaining a sliding window of recently observed lines.
//
// Instances are created via NewCoordinator and reused for the duration
// of one worker's scan over its chunk. They are NOT reusable across
// chunks or across files — allocate a fresh Coordinator each time so
// detector state can't leak.
type Coordinator struct {
	// detectors is the ordered list of detectors receiving OnLine
	// callbacks. Stored by interface value because each detector is an
	// instance created for this worker (see Task 5 wiring notes).
	detectors []LineDetector

	// window is this worker's sliding view of recent lines. Passed by
	// pointer to every OnLine call. Nil when len(detectors) == 0 —
	// that way the zero-detector fast path avoids even allocating the
	// Window's backing array.
	window *Window
}

// NewCoordinator returns a Coordinator configured with the given
// effective window size and detector list.
//
// If detectors is empty, the returned coordinator still satisfies the
// ProcessLine / Finalize contract but performs no per-line work — this
// is the fast path for `rx index` without --analyze. We deliberately
// skip window allocation in that case so memory cost is proportional
// to actual use.
//
// windowLines is the effective size; pass the value returned by
// ResolveWindowLines. Values outside [1, maxWindowLines] are clamped
// by NewWindow.
func NewCoordinator(windowLines int, detectors []LineDetector) *Coordinator {
	c := &Coordinator{detectors: detectors}
	// Only allocate the window when we actually have work to do. An
	// empty detector list means nothing will read the window.
	if len(detectors) > 0 {
		c.window = NewWindow(windowLines)
	}
	return c
}

// ProcessLine precomputes per-line helpers, pushes the line into the
// window, and dispatches OnLine to every detector in order.
//
// Arguments match the index builder's per-line data:
//   - num: 1-based absolute line number
//   - start/end: byte offset range of the line within the source file
//   - line: raw line bytes WITHOUT the trailing newline. Ownership of
//     this slice remains with the caller — the window copies what it
//     needs via append(buf[:0], line...)
//
// Zero-detector fast path: when no detectors are registered, this
// function is a no-op. The intent is that a worker running without
// --analyze pays only the one `len(c.detectors) == 0` branch per line.
func (c *Coordinator) ProcessLine(num, start, end int64, line []byte) {
	// Fast path for the "no detectors" case. See note above — this
	// keeps the per-line cost to a single branch for non-analyze runs.
	if len(c.detectors) == 0 {
		return
	}

	indent := countIndentPrefix(line)
	isBinary := containsNonTextBytes(line)

	// Push into the window first so detectors see this line as
	// Current(). The window's push method reuses slot buffers so this
	// is allocation-free after warmup.
	c.window.push(num, start, end, line, indent, isBinary)

	// Dispatch. Detectors run in registration order — stable, so tests
	// that assert on call order are deterministic.
	for _, d := range c.detectors {
		d.OnLine(c.window)
	}
}

// Finalize asks every detector for its anomaly list and returns the
// combined slice. The caller typically runs one Coordinator per
// worker and then deduplicates across workers' results (Task 4).
//
// flush carries file-global stats (total lines, median / P99 line
// length) computed by the index builder. It is passed through to each
// detector's Finalize; detectors that don't depend on these fields
// may ignore the argument.
//
// The coordinator stamps each anomaly's DetectorName with its producing
// detector's Name(). Rationale:
//
//   - Deduplicate (Task 4) keys on (DetectorName, start_offset, end_offset)
//     so two different detectors that both emit Category="log-pattern"
//     do not collapse across workers.
//
//   - Anomaly.Category stays as the SEMANTIC bucket the detector chose.
//     That's the field exposed as `category` on the wire contract and
//     grouped by /v1/detectors. The coordinator does NOT overwrite it
//     anymore (prior behavior contradicted the wire contract where
//     category and detector are described as independent fields).
//
// Returns nil when there are no detectors — avoids allocating an
// empty slice that the caller would just discard.
func (c *Coordinator) Finalize(flush *FlushContext) []Anomaly {
	if len(c.detectors) == 0 {
		return nil
	}
	// We do not know the per-detector anomaly count up front; let append
	// grow the slice. A fixed pre-size underestimates in the common case
	// (many anomalies per detector) and overestimates for files with
	// zero hits, so neither cap is worth the complexity.
	var out []Anomaly
	for _, d := range c.detectors {
		name := d.Name()
		for _, a := range d.Finalize(flush) {
			a.DetectorName = name
			out = append(out, a)
		}
	}
	return out
}

// countIndentPrefix returns the number of leading tab or space bytes.
// Anything else (including CR or non-ASCII whitespace like U+00A0)
// stops the count. This matches the intent of detectors that want a
// cheap proxy for "indented continuation line" — it is not a Unicode-
// aware indent measurement.
func countIndentPrefix(line []byte) int {
	n := 0
	for n < len(line) {
		b := line[n]
		if b != ' ' && b != '\t' {
			break
		}
		n++
	}
	return n
}

// containsNonTextBytes reports whether any byte is outside the set
// {\t, \n, \r} ∪ [0x20, 0x7E]. Intended as a fast "this line looks
// binary" heuristic so detectors can short-circuit on log files that
// embed raw binary chunks.
//
// We do NOT check for UTF-8 validity — non-ASCII UTF-8 sequences will
// be flagged as "binary" by this function, which is acceptable for
// the current detector catalog (all MVP detectors work on ASCII
// patterns). If future detectors need Unicode-awareness we can add a
// separate precomputed flag without touching this one.
func containsNonTextBytes(line []byte) bool {
	for _, b := range line {
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b > 0x7E {
			return true
		}
	}
	return false
}

package analyzer

// This file defines the line-oriented detector contract layered on top
// of the FileAnalyzer interface. Whereas FileAnalyzer consumes a whole
// file, LineDetector observes a stream of lines as the index builder
// scans them. This lets multiple detectors share a single file pass.
//
// Key design choices:
//
//   - LineEvent carries a BORROWED byte slice — the coordinator reuses
//     the underlying buffer between pushes. Detectors that need to
//     retain line bytes across calls MUST copy them (e.g. `append(nil,
//     e.Bytes...)` or `string(e.Bytes)`).
//
//   - FlushContext is passed to Finalize so detectors that need file-
//     global statistics (e.g. long-line's P99/median-based threshold)
//     can read them without re-scanning.
//
//   - OnLine receives a *Window rather than a *LineEvent so detectors
//     that need local context (traceback openers + continuation lines,
//     json-blob spanning multiple lines, etc.) can look back up to W-1
//     lines without maintaining their own ring buffer.

// LineEvent is the per-line handoff from the coordinator to detectors.
//
// The Bytes field is BORROWED from the Window's slot buffer. It stays
// valid only until the next push into the same slot. Detectors that
// need to remember the content must copy it — never store the slice
// header alone. Coordinator semantics:
//
//	// Safe: copy the bytes
//	saved := append([]byte(nil), ev.Bytes...)
//	savedStr := string(ev.Bytes) // string(...) from []byte always copies
//
//	// UNSAFE: will point to stale data a few pushes later
//	retained := ev.Bytes
type LineEvent struct {
	// Number is the 1-based line number of this event in the source
	// file. Matches the line index numbering used elsewhere in rx-go.
	Number int64

	// StartOffset is the byte offset of the first byte of the line in
	// the source file.
	StartOffset int64

	// EndOffset is the byte offset of the byte just past the last byte
	// of the line (i.e. end is exclusive). For a line "abc\n" starting
	// at offset 0, EndOffset is 4.
	EndOffset int64

	// Bytes is the line content WITHOUT the trailing newline. Borrowed
	// from the coordinator's Window; copy if you need to retain.
	Bytes []byte

	// IndentPrefix is the count of leading tab or space bytes on the
	// line. Precomputed once by the coordinator so every detector that
	// cares about indentation (json-blob, python traceback continuation,
	// etc.) gets it for free.
	IndentPrefix int

	// IsBinary is true when the line contains any byte outside the
	// printable ASCII range plus \t, \n, \r. Precomputed once so
	// detectors can short-circuit on binary lines.
	IsBinary bool
}

// FlushContext carries file-global stats to detectors' Finalize calls.
// Populated by the coordinator driver (the index builder, wired in
// Task 5) from the already-computed line-stats accumulator.
//
// Detectors read only what they need. A detector that does not depend
// on these stats may ignore the argument entirely.
type FlushContext struct {
	// TotalLines is the total number of lines seen during the scan.
	TotalLines int64

	// MedianLineLength is the 50th-percentile line length in bytes.
	MedianLineLength int64

	// P99LineLength is the 99th-percentile line length in bytes. The
	// long-line detector uses this to compute its dynamic threshold.
	P99LineLength int64
}

// LineDetector is the streaming-scan contract. A LineDetector embeds
// FileAnalyzer (so it registers, versions, and describes itself the
// same way) and adds two per-scan hooks:
//
//   - OnLine: called once per line in sequence with a pointer to the
//     coordinator's Window. The most-recent line is Window.Current();
//     prior lines are available via Window.At(back).
//
//   - Finalize: called once after the last line, with file-global
//     stats. The return value is this detector's anomalies.
//
// Implementations are created fresh per worker (the index builder
// shards the file across N workers), so instance state does NOT cross
// worker boundaries. Cross-worker correctness comes from a W-line
// overlap and a post-pass deduplication step.
//
// IMPORTANT: LineDetector embeds FileAnalyzer for registration
// uniformity, but the streaming path (coordinator → OnLine → Finalize)
// is what produces anomalies. FileAnalyzer.Analyze is a NO-OP for
// every shipped line detector — it returns an empty Report purely to
// satisfy the embedded interface. Do not call Analyze on a LineDetector
// and expect anomalies; run it through the coordinator instead.
type LineDetector interface {
	FileAnalyzer

	// OnLine is invoked for every line in the scan, in order. The
	// coordinator has already pushed the line into w, so
	// w.Current() is the line for this event. Borrowed-bytes contract
	// from LineEvent applies: do not retain w.Current().Bytes across
	// calls; copy if you need to.
	OnLine(w *Window)

	// Finalize is called exactly once, after the last OnLine. Returns
	// the detector's complete anomaly list for its input range.
	Finalize(flush *FlushContext) []Anomaly
}

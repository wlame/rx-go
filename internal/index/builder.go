// Builder implements the construction side of the unified line-offset
// index. It walks a file line-by-line, records byte offsets at roughly
// every `step_bytes` interval (aligned to line starts), and collects
// line-length statistics needed for the `--analyze` path.
//
// The on-disk schema is UnifiedFileIndex (pkg/rxtypes). This file writes
// the same JSON shape as rx-python/src/rx/unified_index.py::build_index
// so a cache produced on either side can be read by the other.
//
// Step-size trade-off (checkpoint density):
//
//	Dense checkpoints (small step)  → bigger index file on disk, but
//	                                 faster line-to-offset lookups because
//	                                 the linear-scan distance from the
//	                                 nearest checkpoint to the target
//	                                 line is bounded by step_bytes.
//	Sparse checkpoints (large step) → smaller file, slower lookups.
//
// Default step is threshold/50 = 1 MB when LargeFileMB=50 (Python parity).
// Callers who want a custom density pass `step_bytes` directly via
// BuildWithStep.
package index

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// BuildOptions fine-tunes the Build call. Zero value = Python defaults.
type BuildOptions struct {
	// StepBytes is the approximate number of bytes between line-offset
	// checkpoints. If 0, we take config.LargeFileMB() / 50 (matches
	// Python's get_index_step_bytes).
	StepBytes int64

	// Analyze toggles the line-length statistics and anomaly-ready
	// fields. When false, only the line-index itself is populated;
	// matches Python's "light" cache.
	Analyze bool

	// WindowLines is the sliding-window size passed to analyzer.NewCoordinator
	// when Analyze is true. Zero means "use the resolver default" — the
	// builder feeds this through analyzer.ResolveWindowLines(WindowLines, 0)
	// so the env-var and default-value branches of the resolver still apply.
	//
	// Ignored when Analyze is false.
	WindowLines int

	// Detectors is the list of line-oriented detectors to run during the
	// scan. Ignored when Analyze is false. When Analyze is true and this
	// slice is nil/empty, no detectors run but the builder still goes
	// through the coordinator code path (the coordinator's zero-detector
	// fast path makes that effectively free).
	//
	// Callers typically populate this from the analyzer registry
	// (analyzer.Snapshot filtered down to LineDetector); passing an
	// explicit slice here is mostly for tests that want deterministic
	// detector sets.
	Detectors []analyzer.LineDetector
}

// GetIndexStepBytes returns the default checkpoint step in bytes.
// Mirrors rx-python/src/rx/unified_index.py::get_index_step_bytes:
//
//	step = LargeFileMB_bytes // 50
//
// i.e. approximately 50 checkpoints across the threshold. For default
// LargeFileMB=50 this is 1 MB, which gives a balanced lookup cost
// (~1 MB linear scan worst case) without a huge index file.
func GetIndexStepBytes() int64 {
	threshold := int64(config.LargeFileMB()) * 1024 * 1024
	return threshold / 50
}

// Build reads sourcePath and constructs a UnifiedFileIndex. It records
// the current mtime + size into the index so IsValidForSource can later
// detect changes.
//
// On success the caller can hand the result straight to Save() or
// inspect LineIndex in-memory; Build does not write to disk itself.
func Build(sourcePath string, opts BuildOptions) (*rxtypes.UnifiedFileIndex, error) {
	started := time.Now()

	info, err := os.Stat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", sourcePath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("build: %s is a directory", sourcePath)
	}

	step := opts.StepBytes
	if step <= 0 {
		step = GetIndexStepBytes()
	}

	f, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer func() {
		// Close error ignored — file was opened read-only.
		_ = f.Close()
	}()

	// Wire up the analyzer coordinator when --analyze is on. One
	// coordinator per scan; today the builder is sequential so that's
	// effectively one "worker". When the builder later shards the file
	// across K workers (plan §Solution Overview), each worker will get
	// its own Coordinator, and the per-worker anomaly slices will be
	// combined by analyzer.Deduplicate before storage — preserving the
	// W-line-overlap correctness story described in the plan.
	//
	// When Analyze is false we pass a nil coordinator; walkLines skips
	// all per-line dispatch so the hot loop stays byte-identical to its
	// pre-analyzer shape (no regression for users who don't opt in).
	var coord *analyzer.Coordinator
	if opts.Analyze {
		windowLines := analyzer.ResolveWindowLines(opts.WindowLines, 0)
		coord = analyzer.NewCoordinator(windowLines, opts.Detectors)
	}

	// Walk the file. Python uses `for line in f` which yields lines
	// terminated by the platform's preferred newline. In Go, bufio's
	// ReadSlice('\n') gives the same slice-including-terminator view.
	//
	// Stage 9 Round 2 R1-B10: the walk always collects line-length stats
	// (Python parity — see rx-python/src/rx/unified_index.py::build_index).
	// Anomaly detection is gated at the call site (opts.Analyze controls
	// whether coord is non-nil).
	stats, err := walkLines(f, step, coord)
	if err != nil {
		return nil, err
	}

	// Build the final index.
	idx := &rxtypes.UnifiedFileIndex{
		Version:           Version,
		SourcePath:        sourcePath,
		SourceModifiedAt:  formatMtime(info.ModTime()),
		SourceSizeBytes:   info.Size(),
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		BuildTimeSeconds:  time.Since(started).Seconds(),
		FileType:          rxtypes.FileTypeText, // compressed detection left to caller
		IsText:            true,
		LineIndex:         stats.LineIndex,
		IndexStepBytes:    ptrInt64(step),
		AnalysisPerformed: opts.Analyze,
	}

	// Fill stats. Python always populates line_count/empty_line_count
	// AND the line-length aggregates, regardless of --analyze (Stage 9
	// Round 2 R1-B10 fix). The only fields gated on --analyze are
	// anomaly detection and prefix pattern fields (both Python-only at
	// v1 of rx-go).
	//
	// Post-Welford/reservoir refactor: the accumulator always returns a
	// zero-valued snapshot for empty input, so the previous
	// len(LineLengths)>0 branch collapses into a single unconditional
	// copy. Python's JSON wire shape is preserved byte-identically —
	// every pointer field is populated with either the real value or 0,
	// matching unified_index.py's L174-179 fallback.
	idx.LineCount = ptrInt64(stats.LineCount)
	idx.EmptyLineCount = ptrInt64(stats.EmptyLineCount)
	idx.LineLengthMax = ptrInt64(int64(stats.LineStats.Max))
	idx.LineLengthAvg = ptrFloat64(stats.LineStats.Mean)
	idx.LineLengthMedian = ptrFloat64(stats.LineStats.Median)
	idx.LineLengthP95 = ptrFloat64(stats.LineStats.P95)
	idx.LineLengthP99 = ptrFloat64(stats.LineStats.P99)
	idx.LineLengthStddev = ptrFloat64(stats.LineStats.StdDev)
	idx.LineLengthMaxLineNumber = ptrInt64(int64(stats.LineStats.MaxLineNumber))
	idx.LineLengthMaxByteOffset = ptrInt64(stats.LineStats.MaxLineOffset)

	// Line-ending detection runs off a prefix sample (first 64 KB) so
	// large files don't pay O(n). Python behaves the same.
	idx.LineEnding = ptrString(stats.LineEnding)

	// Finalize analyzer output. When opts.Analyze is true, build a
	// FlushContext from the line-stats accumulator and hand it to every
	// detector's Finalize. The per-worker anomaly lists are then
	// deduplicated by analyzer.Deduplicate — today there is only one
	// "worker" (the single sequential walk) so dedup is effectively a
	// pass-through, but writing the code through Deduplicate keeps the
	// plumbing ready for the future chunk-parallel builder described in
	// the plan.
	//
	// Empty slice vs nil: rxtypes.UnifiedFileIndex.Anomalies is
	// *[]AnomalyRangeResult so a nil pointer serializes to JSON null
	// (pre-analyze behavior) and a populated pointer serializes to
	// [...]. When Analyze is on we always set a pointer — to an empty
	// slice if the detectors emitted nothing — so the JSON shape is "[]"
	// rather than "null" in that case. That matches Python's behavior
	// for analysis_performed=true runs.
	if opts.Analyze && coord != nil {
		flush := &analyzer.FlushContext{
			TotalLines:       stats.LineCount,
			MedianLineLength: int64(stats.LineStats.Median),
			P99LineLength:    int64(stats.LineStats.P99),
		}
		// One group today (single-worker scan). When chunk-parallel builds
		// land, collect one group per worker and pass them all in here.
		groups := [][]analyzer.Anomaly{coord.Finalize(flush)}
		deduped := analyzer.Deduplicate(groups)

		results := make([]rxtypes.AnomalyRangeResult, 0, len(deduped))
		summary := make(map[string]int)
		for _, a := range deduped {
			// Category is the semantic bucket the detector chose
			// ("log-traceback", "secrets", "format", ...). DetectorName is
			// the globally-unique detector identifier stamped by the
			// coordinator. They are independent: two distinct detectors
			// can share a category. The wire shape exposes both so UIs
			// can group by category AND jump by detector.
			//
			// Summary is keyed by DetectorName (one counter per detector)
			// because the frontend's "jump to next $detector" logic
			// needs per-detector counts, not per-category.
			results = append(results, rxtypes.AnomalyRangeResult{
				StartLine:   a.StartLine,
				EndLine:     a.EndLine,
				StartOffset: a.StartOffset,
				EndOffset:   a.EndOffset,
				Severity:    a.Severity,
				Category:    a.Category,
				Description: a.Description,
				Detector:    a.DetectorName,
			})
			summary[a.DetectorName]++
		}
		idx.Anomalies = &results
		idx.AnomalySummary = summary
	}

	// Stage 9 Round 2 S6: gated helper — CLI mode skips observation.
	prometheus.ObserveIndexBuildDuration(time.Since(started))
	return idx, nil
}

// ==========================================================================
// Internals
// ==========================================================================

// walkStats is the aggregated output of a single walkLines pass.
//
// Post-refactor: line-length statistics are computed online via
// lineStatsAccumulator and flattened to LineStats at finish-time, so we
// no longer materialize a slice of every line length. Memory is O(1) in
// the number of lines (bounded by the reservoir cap, ~80 KB).
type walkStats struct {
	LineIndex      []rxtypes.LineIndexEntry
	LineCount      int64
	EmptyLineCount int64

	// LineStats is the finalized snapshot from the online accumulator —
	// mean/stddev from Welford, median/p95/p99 from reservoir sampling.
	LineStats lineStatsSnapshot

	LineEnding string
}

// walkLines streams through r, emitting line-index checkpoints and
// (when analyze) collecting line-length statistics. The algorithm
// matches rx-python/src/rx/unified_index.py::build_index byte-for-byte:
//
//  1. First line is always at offset 0 (checkpoint [1, 0]).
//  2. Track a running byte offset; each time it crosses the next
//     `step_bytes` boundary, emit a checkpoint at the NEXT line start.
//  3. For --analyze: collect lengths of non-empty lines (stripped of
//     trailing CR/LF) and track the longest line + its position.
//
// The line-ending sample is the first 64 KB of raw bytes (including
// terminators) — we feed it to detectLineEnding after the walk.
//
// coord is the analyzer coordinator for this walk, or nil when
// analysis is disabled. When non-nil, each line (stripped of its
// trailing CR/LF) is dispatched via coord.ProcessLine along with its
// absolute byte-offset range. Finalize is the caller's responsibility
// (Build invokes it after walkLines returns so the FlushContext can be
// populated from the finalized line-stats snapshot).
func walkLines(r io.Reader, step int64, coord *analyzer.Coordinator) (*walkStats, error) {
	stats := &walkStats{
		// Initial checkpoint: first line is always at offset 0.
		LineIndex:  []rxtypes.LineIndexEntry{{LineNumber: 1, ByteOffset: 0}},
		LineEnding: "LF",
	}

	// Online stats accumulator — replaces the previous []int64 of every
	// line's length. Memory footprint is O(reservoirCap) regardless of
	// total line count. See internal/index/linestats.go for the algorithm.
	acc := newLineStatsAccumulator(0)

	// bufio.Reader.ReadSlice is faster than Scanner for this purpose:
	// we want the TERMINATOR included so the byte count matches Python's
	// `len(line)` (which is bytes including \n / \r\n).
	br := bufio.NewReaderSize(r, 256*1024)

	// The Python code uses iteration `for line in f` which yields bytes
	// including the newline. We faithfully replicate by reading up to
	// '\n' with ReadSlice and appending when the buffer overflows.
	var (
		currentOffset    int64
		currentLine      int64 // 0-based until first iteration
		nextCheckpoint   = step
		lineEndingSample = make([]byte, 0, 65536)
		sampleComplete   bool
	)

	for {
		// ReadSlice may return ErrBufferFull for very long lines; join
		// pieces into a single logical line.
		var line []byte
		for {
			chunk, err := br.ReadSlice('\n')
			if len(chunk) > 0 {
				// We must copy — ReadSlice's buffer is reused on the
				// next call. Appending into `line` implicitly copies.
				line = append(line, chunk...)
			}
			if err == bufio.ErrBufferFull {
				// Long line; keep reading into `line` until we hit
				// '\n' or EOF.
				continue
			}
			if err != nil {
				// Either io.EOF or a real I/O error. If EOF and the
				// last fragment has content, it's the trailing
				// unterminated line. Otherwise we're done.
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("read: %w", err)
			}
			// Normal \n-terminated line.
			break
		}
		if len(line) == 0 {
			// Clean EOF.
			break
		}

		currentLine++
		lineLenBytes := int64(len(line))

		// Append to line-ending sample until we've collected 64 KB.
		//
		// Python parity (rx-python/src/rx/unified_index.py): Python's
		// loop does `line_ending_sample += line` (whole-line append)
		// and only checks the size threshold AFTER the append. This
		// means Python's sample can OVERSHOOT by up to one line's
		// worth of bytes — i.e. if the sample is at 65534 bytes and
		// the next line is 9 bytes, the sample becomes 65543 bytes
		// before the "we've got enough" check fires.
		//
		// A previous Go version truncated the last line at byte
		// granularity (`take = min(lineLen, remaining)`), which could
		// drop trailing CR/LF bytes that Python would have captured.
		// That broke line-ending detection for files whose ending-
		// style transition happened right around the 64 KB boundary
		// (see Stage 8 Reviewer 1 High #4 / Finding 6). The fix is to
		// append the WHOLE line and accept the overshoot.
		if !sampleComplete {
			lineEndingSample = append(lineEndingSample, line...)
			if len(lineEndingSample) >= 65536 {
				sampleComplete = true
			}
		}

		// Stats observation. Python parity (rx-python/src/rx/unified_index.py):
		// every line is inspected, whitespace-only lines are counted as
		// "empty", and non-empty lines' content lengths feed the aggregates.
		//
		// Unlike the pre-refactor code, the accumulator now handles BOTH
		// paths (analyze / non-analyze) with the same call — Python's
		// behavior is that line_length aggregates are populated regardless
		// of --analyze (Stage 9 Round 2 R1-B10), so there is no reason to
		// bypass the accumulator when analyze==false.
		stripped := stripLineEnd(line)
		contentLen := len(stripped)
		isEmpty := !hasNonWhitespace(stripped)
		acc.observe(contentLen, isEmpty, int(currentLine), currentOffset)

		// Dispatch to the analyzer coordinator if one was provided. We
		// pass the STRIPPED line (no trailing CR/LF) to match detector
		// expectations — the Window's LineEvent contract says Bytes is
		// "the line content WITHOUT the trailing newline". The absolute
		// byte range spans the raw line including its terminator so
		// anomaly start/end offsets align with what seek-to-line needs.
		//
		// Zero-detector fast path: if coord has no detectors registered,
		// ProcessLine is a no-op (single branch per line), so the cost
		// of passing a coordinator with an empty detector slice is
		// negligible.
		if coord != nil {
			coord.ProcessLine(currentLine, currentOffset, currentOffset+lineLenBytes, stripped)
		}

		currentOffset += lineLenBytes

		// Checkpoint check: once we've crossed `next_checkpoint`, emit
		// a record at the START of the next line.
		//
		// Note the off-by-one: currentOffset is now the offset AFTER
		// the newline we just consumed, which IS the start of the
		// next line. `current_line + 1` is the number we'd assign
		// to the next iteration's line.
		if currentOffset >= nextCheckpoint {
			stats.LineIndex = append(stats.LineIndex, rxtypes.LineIndexEntry{
				LineNumber: currentLine + 1,
				ByteOffset: currentOffset,
			})
			nextCheckpoint = currentOffset + step
		}
	}

	stats.LineCount = currentLine

	// Snapshot the accumulator. finish() copies the reservoir internally
	// so repeated calls are safe; we call it exactly once.
	stats.LineStats = acc.finish()
	stats.EmptyLineCount = int64(stats.LineStats.EmptyCount)

	stats.LineEnding = detectLineEnding(lineEndingSample)
	return stats, nil
}

// ==========================================================================
// Line-ending detection — mirrors rx-python/src/rx/unified_index.py
// ==========================================================================

// detectLineEnding classifies sample bytes as LF / CRLF / CR / mixed.
// Same logic as Python:
//   - crlf = count("\r\n")
//   - cr   = count("\r") - crlf
//   - lf   = count("\n") - crlf
//   - 0 endings → "LF" default; 1 distinct ending → that one; else "mixed".
func detectLineEnding(sample []byte) string {
	crlf := bytes.Count(sample, []byte("\r\n"))
	cr := bytes.Count(sample, []byte("\r")) - crlf
	lf := bytes.Count(sample, []byte("\n")) - crlf

	type kind struct {
		name  string
		count int
	}
	var endings []kind
	if crlf > 0 {
		endings = append(endings, kind{"CRLF", crlf})
	}
	if lf > 0 {
		endings = append(endings, kind{"LF", lf})
	}
	if cr > 0 {
		endings = append(endings, kind{"CR", cr})
	}

	switch len(endings) {
	case 0:
		return "LF"
	case 1:
		return endings[0].name
	default:
		return "mixed"
	}
}

// ==========================================================================
// Line utilities
// ==========================================================================

// stripLineEnd returns `line` with any trailing \r\n, \n, or \r removed.
// Python's line.rstrip(b'\r\n') strips both. We do the same.
func stripLineEnd(line []byte) []byte {
	n := len(line)
	for n > 0 && (line[n-1] == '\n' || line[n-1] == '\r') {
		n--
	}
	return line[:n]
}

// hasNonWhitespace returns true if `s` contains any non-whitespace byte.
// Python's `stripped.strip()` is truthy iff the result is non-empty,
// i.e. there's at least one non-whitespace character. We treat ASCII
// whitespace (space, tab, \v, \f, \r, \n) as whitespace — same as
// Python's bytes.strip() default set.
func hasNonWhitespace(s []byte) bool {
	for _, c := range s {
		switch c {
		case ' ', '\t', '\v', '\f', '\r', '\n':
			continue
		default:
			return true
		}
	}
	return false
}

// ==========================================================================
// Small pointer helpers (the struct has many nullable numeric fields)
// ==========================================================================

func ptrInt64(n int64) *int64       { return &n }
func ptrFloat64(v float64) *float64 { return &v }
func ptrString(s string) *string    { return &s }

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
	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/seekableindex"
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
	// Callers populate this from the analyzer registry
	// (analyzer.LineDetectorSnapshot); passing an explicit slice here is
	// mostly for tests that want deterministic detector sets.
	Detectors []analyzer.LineDetector

	// Progress, when set, counts how far the build has read, for a
	// caller that reports it while the build runs (a background index
	// task). Nil costs nothing.
	Progress *Progress

	// afterPin, when set, runs right after the source is pinned and
	// before it is opened. Tests use it to retarget a link at that
	// moment.
	afterPin func()

	// beforeWalk, when set, runs after the file is opened and stated and
	// before its text is read. Tests use it to change the file, or what
	// its path leads to, at that moment.
	beforeWalk func()
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

// analysisWindowLines is the sliding-window size an analysis with opts
// runs with: opts.WindowLines through the resolver, so 0 means the
// environment variable or the default.
func analysisWindowLines(opts BuildOptions) int {
	return analyzer.ResolveWindowLines(opts.WindowLines, 0)
}

// SatisfiesBuild reports whether a cached index answers a request that
// would otherwise build one with opts, so the caller may reuse it
// instead. The cached index must already be valid for its source
// (LoadForSource checks that).
//
// Without analysis, any index will do: the line index does not depend
// on the options. With analysis, the cached index must hold an analysis
// made with the same window and the same detector set: a different
// window finds different anomalies, and so does a detector added,
// removed or upgraded since. An analysis that does not record them
// (written before the index did) is never reused.
func SatisfiesBuild(idx *rxtypes.UnifiedFileIndex, opts BuildOptions) bool {
	if !opts.Analyze {
		return true
	}
	if !idx.AnalysisPerformed || idx.AnalysisWindowLines == nil || idx.AnalysisDetectorSet == nil {
		return false
	}
	return *idx.AnalysisWindowLines == analysisWindowLines(opts) &&
		*idx.AnalysisDetectorSet == analyzer.DetectorSetVersion(opts.Detectors)
}

// Build reads sourcePath and constructs a UnifiedFileIndex. It records
// the file's identity as its first stat saw it, and reads no further
// than that size, so IsValidForSource can later detect any change.
//
// The source is pinned first (paths.Pin): checked as a named path is,
// against the search roots and the hidden rule, and read only while it
// is still the file that check saw. A directory walk hands Build paths
// it checked earlier, and a link among them can be retargeted in
// between; the pin is what keeps that from indexing a file outside the
// roots.
//
// SECURITY: the pinned file is opened once, and every later read —
// format detection, fingerprint, text walk, seek table and frames —
// goes through that one handle. Nothing is looked up by sourcePath
// after the open, so a link retargeted during the build cannot make the
// build read another file.
//
// On success the caller can hand the result straight to Save() or
// inspect LineIndex in-memory; Build does not write to disk itself.
func Build(sourcePath string, opts BuildOptions) (*rxtypes.UnifiedFileIndex, error) {
	started := time.Now()

	src, err := paths.Pin(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", sourcePath, err)
	}
	info := src.Info()
	if info.IsDir() {
		return nil, fmt.Errorf("build: %s is a directory", sourcePath)
	}
	if opts.afterPin != nil {
		opts.afterPin()
	}

	f, err := src.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer func() {
		// Close error ignored — file was opened read-only.
		_ = f.Close()
	}()

	// The index describes the file as the pin's stat saw it. The
	// identity is taken now, before the walk, and the walk reads no
	// further than the stated size, so a log that grows during the
	// build gets an index of exactly the bytes its identity records.
	// The next load sees the larger size and rebuilds. Inode, ctime and
	// the fingerprint are what let a later run tell this exact file
	// from one rewritten with the same size and mtime; a fingerprint
	// that cannot be read is left out, and validation falls back to the
	// other fields.
	source := openedSource{
		path:     sourcePath,
		file:     f,
		info:     info,
		identity: IdentityFromOpenFile(f, info),
	}

	step := opts.StepBytes
	if step <= 0 {
		step = GetIndexStepBytes()
	}

	// A seekable .zst is indexed by its frames rather than by a byte
	// step: the frame table is the whole point of the format, and a
	// checkpoint that names a frame lets a lookup decompress that one
	// frame instead of the stream up to it. rx-python indexes the same
	// file the same way, and the cache is shared.
	if seekable.IsSeekableFile(sourcePath, f, info.Size()) {
		return buildSeekable(source, started, step, opts)
	}
	return buildText(source, started, step, opts)
}

// openedSource is the file an index build reads: the caller's path, the
// handle the pin opened, the pin's stat and the identity taken from
// them. Every read of the build goes through file.
type openedSource struct {
	path     string
	file     *os.File
	info     os.FileInfo
	identity SourceIdentity
}

// buildText indexes a plain or stream-compressed file by walking its
// text line by line.
func buildText(
	src openedSource,
	started time.Time,
	step int64,
	opts BuildOptions,
) (*rxtypes.UnifiedFileIndex, error) {
	sourcePath, info, identity := src.path, src.info, src.identity
	// The progress counts the file's own bytes, before any
	// decompression, so its total is the size the stat saw.
	if opts.Progress != nil {
		opts.Progress.start(info.Size())
	}
	statedBytes := opts.Progress.countReads(io.LimitReader(src.file, info.Size()))

	// A compressed file is indexed through its decompressor, so the
	// line numbers and byte offsets describe the text inside it. Read
	// straight off the compressed bytes and the checkpoints would
	// address compressed noise and the statistics would describe the
	// container: a 600 MB log came back as 209,365 lines with a "mixed"
	// line ending. rx-python indexes the same content the same way.
	source := statedBytes
	format, _ := compression.DetectFromOpenFile(sourcePath, src.file)
	if format != compression.FormatNone {
		// The file itself is closed by Build.
		dec, dErr := compression.NewReader(io.NopCloser(statedBytes), format)
		if dErr != nil {
			return nil, fmt.Errorf("decompress %s: %w", sourcePath, dErr)
		}
		defer func() { _ = dec.Close() }()
		source = dec
	}

	// The coordinator the walk feeds each line to; nil without --analyze.
	coord := newCoordinator(opts)

	// Walk the file. Python uses `for line in f` which yields lines
	// terminated by the platform's preferred newline. In Go, bufio's
	// ReadSlice('\n') gives the same slice-including-terminator view.
	//
	// the walk always collects line-length stats
	// (Python parity — see rx-python/src/rx/unified_index.py::build_index).
	// Anomaly detection is gated at the call site (opts.Analyze controls
	// whether coord is non-nil).
	if opts.beforeWalk != nil {
		opts.beforeWalk()
	}
	stats, err := walkLines(source, step, coord)
	if err != nil {
		return nil, err
	}

	// Build the final index.
	permissions, owner := FileOwnership(info)
	idx := &rxtypes.UnifiedFileIndex{
		Version:          Version,
		SourcePath:       sourcePath,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
		BuildTimeSeconds: time.Since(started).Seconds(),
		FileType:         rxtypes.FileTypeText,
		IsText:           true,
		Permissions:      permissions,
		Owner:            owner,
		LineIndex:        stats.LineIndex,
		IndexStepBytes:   ptrInt64(step),
	}
	identity.stampInto(idx)

	// Python always populates the line counts and the line-length
	// aggregates, whether or not --analyze is set; only the anomaly
	// fields depend on it.
	applyLineStats(idx, stats)

	// A compressed source records what it is and how much text it
	// holds; the index's own offsets are positions in that text.
	if format != compression.FormatNone {
		name := string(format)
		idx.CompressionFormat = &name
		idx.FileType = rxtypes.FileTypeCompressed
		idx.DecompressedSizeBytes = ptrInt64(stats.TotalBytes)
		if info.Size() > 0 && stats.TotalBytes > 0 {
			ratio := float64(stats.TotalBytes) / float64(info.Size())
			idx.CompressionRatio = &ratio
		}
	}

	if coord != nil {
		applyAnalysis(idx, coord, stats, opts)
	}

	// gated helper — CLI mode skips observation.
	prometheus.ObserveIndexBuildDuration(time.Since(started))
	return idx, nil
}

// newCoordinator returns the analyzer coordinator one build feeds its
// lines to, or nil when opts asks for no analysis. One coordinator per
// scan: the builder reads the text sequentially, so that is effectively
// one "worker". A builder that sharded the file across K workers would
// give each worker its own Coordinator and combine the per-worker
// anomaly slices with analyzer.Deduplicate before storage, so an
// anomaly seen in two overlapping windows is kept once.
//
// With a nil coordinator walkLines skips all per-line dispatch, so the
// hot loop costs nothing extra for a build that does not analyze.
func newCoordinator(opts BuildOptions) *analyzer.Coordinator {
	if !opts.Analyze {
		return nil
	}
	return analyzer.NewCoordinator(analysisWindowLines(opts), opts.Detectors)
}

// applyLineStats copies the line counts, the line-length statistics and
// the line ending a walk of the file's text collected into idx.
//
// The accumulator returns a zero-valued snapshot for empty input, so
// every pointer field is set, to the real value or to 0, which is the
// JSON shape rx-python writes (unified_index.py's fallback).
func applyLineStats(idx *rxtypes.UnifiedFileIndex, stats *walkStats) {
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
}

// applyAnalysis finalizes the detectors coord fed during the walk and
// records their anomalies, and the window and detector set that found
// them, in idx. It marks idx as analyzed.
//
// Each detector's Finalize gets a FlushContext built from the walk's
// line statistics. The per-worker anomaly lists then go through
// analyzer.Deduplicate: today there is one "worker" (the single
// sequential walk), so dedup is a pass-through, but it keeps the
// plumbing ready for a chunk-parallel builder.
//
// Anomalies is a pointer to a slice so that "no analysis" serializes as
// JSON null; an analysis always sets it, to an empty slice when the
// detectors found nothing, so the shape is [] rather than null. That
// matches rx-python for analysis_performed=true runs.
func applyAnalysis(idx *rxtypes.UnifiedFileIndex, coord *analyzer.Coordinator, stats *walkStats, opts BuildOptions) {
	flush := &analyzer.FlushContext{
		TotalLines:       stats.LineCount,
		MedianLineLength: int64(stats.LineStats.Median),
		P99LineLength:    int64(stats.LineStats.P99),
	}
	groups := [][]analyzer.Anomaly{coord.Finalize(flush)}
	deduped := analyzer.Deduplicate(groups)

	results := make([]rxtypes.AnomalyRangeResult, 0, len(deduped))
	summary := make(map[string]int)
	for _, a := range deduped {
		// Category is the semantic bucket the detector chose
		// ("log-traceback", "secrets", "format", ...). DetectorName is
		// the globally-unique detector identifier stamped by the
		// coordinator. They are independent: two distinct detectors can
		// share a category. The wire shape exposes both so UIs can group
		// by category AND jump by detector.
		//
		// Summary is keyed by DetectorName (one counter per detector)
		// because the frontend's "jump to next $detector" logic needs
		// per-detector counts, not per-category.
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
	window := analysisWindowLines(opts)
	detectorSet := analyzer.DetectorSetVersion(opts.Detectors)

	idx.AnalysisPerformed = true
	idx.Anomalies = &results
	idx.AnomalySummary = summary
	idx.AnalysisWindowLines = &window
	idx.AnalysisDetectorSet = &detectorSet
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

	// TotalBytes is the number of bytes the walk read. For a plain file
	// that is its size; for a compressed one it is the size of the text
	// inside it, which is what the index's offsets are measured in.
	TotalBytes int64
}

// walkLines streams through r, emitting line-index checkpoints and
// (when analyze) collecting line-length statistics. The algorithm
// matches rx-python/src/rx/unified_index.py::build_index byte-for-byte:
//
//  1. First line is always at offset 0 (checkpoint [1, 0]).
//  2. Track a running byte offset; each time it crosses the next
//     `step_bytes` boundary, emit a checkpoint at the NEXT line start.
//
// INVARIANT: every checkpoint names a line the text has, at the byte
// where that line starts. A checkpoint is therefore written only when
// the line it names is read: the crossing in step 2 marks one as due,
// and the next line read takes it. When the crossing line is the last
// one, no line follows and nothing is written; an empty text has no
// line 1 and gets no checkpoint at all. rx-python writes a checkpoint
// one line past the end in both cases; readers treat a missing
// checkpoint as "start at byte 0", so both kinds of index give the same
// answers.
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
		// Non-nil, so an index without checkpoints (an empty file)
		// serializes as [] rather than null.
		LineIndex:  []rxtypes.LineIndexEntry{},
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
		// checkpointDue is set when the next line read starts a
		// checkpoint. It starts true: the first line, if there is one,
		// is the checkpoint [1, 0].
		checkpointDue = true
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

		// This line exists and starts at currentOffset, so a
		// checkpoint marked due is written now, naming it.
		if checkpointDue {
			stats.LineIndex = append(stats.LineIndex, rxtypes.LineIndexEntry{
				LineNumber: currentLine,
				ByteOffset: currentOffset,
			})
			checkpointDue = false
		}

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
		// That broke line-ending detection for files whose ending-style
		// transition happened right around the 64 KB boundary. The whole
		// line is appended now and the overshoot accepted.
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
		// of --analyze, so there is no reason to
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

		// Checkpoint check: once we've crossed `next_checkpoint`, the
		// START of the next line gets a checkpoint. currentOffset is
		// now the offset after the newline just consumed, which is
		// where that line starts if there is one; the next iteration
		// writes the checkpoint when it reads the line.
		if currentOffset >= nextCheckpoint {
			checkpointDue = true
			nextCheckpoint = currentOffset + step
		}
	}

	stats.LineCount = currentLine
	stats.TotalBytes = currentOffset

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

// buildSeekable indexes a seekable-zstd file from its frame table.
//
// The other file types are walked line by line and checkpointed every
// `index_step_bytes`; this one is walked frame by frame, because a
// frame is the unit a later lookup can decompress on its own. The
// difference shows in the index: `file_type` is `seekable_zstd`, the
// checkpoints carry a frame number, and `frames` holds the line range
// of each frame.
//
// Without analysis the line-length statistics the text path collects
// are left out, as rx-python leaves them out: nothing on the seekable
// path reads them. With analysis the decompressed text goes through the
// same walk and the same detectors as a plain file's, so the analysis
// of a seekable file equals the analysis of its decompressed copy, line
// numbers and offsets included (both are positions in the text).
func buildSeekable(
	src openedSource,
	started time.Time,
	step int64,
	opts BuildOptions,
) (*rxtypes.UnifiedFileIndex, error) {
	sourcePath, info, identity := src.path, src.info, src.identity
	// The seek table sits at the end of the file, so it is read at the
	// file's current size, as the open handle reports it.
	current, err := src.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", sourcePath, err)
	}
	size := current.Size()

	coord := newCoordinator(opts)
	var (
		frames *seekableindex.Result
		stats  *walkStats
	)
	if opts.Progress != nil {
		if progressErr := startSeekableProgress(opts.Progress, src.file, size); progressErr != nil {
			return nil, fmt.Errorf("%s: %w", sourcePath, progressErr)
		}
	}
	if opts.beforeWalk != nil {
		opts.beforeWalk()
	}
	if coord == nil {
		frames, err = seekableindex.BuildAndCopyText(src.file, size, opts.Progress.countWrites(io.Discard))
	} else {
		frames, stats, err = buildFramesAndWalkText(src.file, size, step, coord, opts.Progress)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sourcePath, err)
	}

	format := "zstd"
	frameList := frames.Frames
	permissions, owner := FileOwnership(info)
	idx := &rxtypes.UnifiedFileIndex{
		Version:           Version,
		SourcePath:        sourcePath,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		BuildTimeSeconds:  time.Since(started).Seconds(),
		FileType:          rxtypes.FileTypeSeekableZstd,
		CompressionFormat: &format,
		// A compressed container is not text, whatever it holds. This is
		// what rx-python records for the same file, and the flag
		// describes the bytes on disk rather than the stream inside.
		IsText:                false,
		Permissions:           permissions,
		Owner:                 owner,
		LineIndex:             frames.LineIndex,
		DecompressedSizeBytes: ptrInt64(frames.DecompressedSizeBytes),
		FrameCount:            &frames.FrameCount,
		FrameSizeTarget:       ptrInt64(frames.FrameSizeTarget),
		Frames:                &frameList,
		LineCount:             ptrInt64(frames.LineCount),
	}
	identity.stampInto(idx)
	if info.Size() > 0 && frames.DecompressedSizeBytes > 0 {
		ratio := float64(frames.DecompressedSizeBytes) / float64(info.Size())
		idx.CompressionRatio = &ratio
	}
	if coord != nil {
		// The walk counts the same lines the frame table does; its
		// checkpoints are dropped, since the frame-numbered ones above
		// are the seekable index's own.
		applyLineStats(idx, stats)
		applyAnalysis(idx, coord, stats, opts)
	}
	return idx, nil
}

// startSeekableProgress sets the total of a seekable file's build: the
// length of its text, which its seek table records. The build decodes
// frame after frame by position rather than reading the file as one
// stream, so the text it writes is what the progress counts. The table
// is read from r, the open file, size bytes long.
func startSeekableProgress(progress *Progress, r io.ReaderAt, size int64) error {
	table, err := seekable.ReadSeekTable(r, size)
	if err != nil {
		return fmt.Errorf("read seek table: %w", err)
	}
	if len(table.Frames) > 0 {
		progress.start(table.Frames[len(table.Frames)-1].DecompressedEnd())
	}
	return nil
}

// buildFramesAndWalkText makes one decompression pass over a seekable
// file that both builds its frame table and walks its text through
// walkLines, which feeds every line to coord.
//
// Two goroutines share the pass through an io.Pipe. The one started
// here decodes the frames in order and writes each frame's text into
// the pipe; this goroutine reads the pipe as one continuous stream, the
// same way the text path reads a plain file. A pipe write blocks until
// the reader has taken the bytes, so about one frame is held in memory
// at a time, and decoding the next frame overlaps with walking this
// one.
//
// Errors cross the pipe in both directions so that neither side waits
// for the other forever: a decode failure closes the write end with
// that error, which ends the walk; a walk failure closes the read end,
// which makes the next write fail and ends the decoding. The decode
// error is the one reported when both happen, because it is the cause.
//
// The file is read through r, size bytes long, which the decoding
// goroutine alone reads. progress, when not nil, counts the text as the
// decoder writes it.
func buildFramesAndWalkText(
	r io.ReaderAt,
	size int64,
	step int64,
	coord *analyzer.Coordinator,
	progress *Progress,
) (*seekableindex.Result, *walkStats, error) {
	textReader, textWriter := io.Pipe()

	// framesResult carries the decoding goroutine's outcome back over
	// the channel. The channel is buffered (capacity 1) so the goroutine
	// can always deliver its result and exit, whatever this side does.
	type framesResult struct {
		frames *seekableindex.Result
		err    error
	}
	decoded := make(chan framesResult, 1)
	go func() {
		frames, err := seekableindex.BuildAndCopyText(r, size, progress.countWrites(textWriter))
		// CloseWithError(nil) is a plain Close: the walk reads io.EOF
		// after the last frame. A non-nil error is what the walk's next
		// read returns instead.
		_ = textWriter.CloseWithError(err)
		decoded <- framesResult{frames: frames, err: err}
	}()

	stats, walkErr := walkLines(textReader, step, coord)
	// Unblocks a decoder still writing if the walk stopped early; after
	// a complete walk the decoder has already finished and this is a
	// no-op.
	_ = textReader.CloseWithError(walkErr)

	result := <-decoded
	if result.err != nil {
		return nil, nil, result.err
	}
	if walkErr != nil {
		return nil, nil, fmt.Errorf("walk text: %w", walkErr)
	}
	return result.frames, stats, nil
}

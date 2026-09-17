package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx-go/internal/compression"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Parallel frame-level scan for seekable-zstd files
// ============================================================================

// framesPerBatch is the number of consecutive frames a single worker
// scans in one subprocess pipeline. Batching amortizes rg startup cost
// — 100 matches Python's heuristic (rx-python/src/rx/trace_compressed.py).
//
// A batch is a run of frames, and frames need not end at line breaks:
// rx's own encoder cuts them there, but another encoder can cut
// anywhere, inside a line longer than a frame too. The batch therefore
// owns lines rather than bytes, as scanFrameBatch describes, and any
// file can be batched.
const framesPerBatch = 100

// frameLoc records what the writer learned about one frame of its
// batch: its newline count, which places the frames after it in the
// file.
type frameLoc struct {
	frameIdx  int
	lineCount int // number of '\n' bytes in this frame's decompressed data
	// decoded says the writer decompressed this frame, so lineCount
	// is its real count. A frame inside a line longer than a frame
	// holds no '\n', so a zero count alone cannot say "not read".
	decoded bool
}

// streamSegment is a run of one frame's text that the writer handed to
// ripgrep. ripgrep reports an event by its position in its own input and
// by its line number in that input; the segment holding that position
// turns both into the frame's terms.
type streamSegment struct {
	frame       seekable.FrameInfo
	streamStart int64 // where the run begins in ripgrep's input
	fileStart   int64 // where the run begins in the file's text
	// lineShift turns ripgrep's line number into the line's number
	// inside its frame: the frame's line breaks before fileStart, less
	// the line breaks ripgrep was given before streamStart.
	lineShift int
}

// segmentHolding returns the segment that holds byte streamOffset of
// ripgrep's input. Segments are recorded in input order, so a binary
// search finds the last one starting at or before the offset.
func segmentHolding(segments []streamSegment, streamOffset int64) (streamSegment, bool) {
	i := sort.Search(len(segments), func(i int) bool {
		return segments[i].streamStart > streamOffset
	}) - 1
	if i < 0 {
		return streamSegment{}, false
	}
	return segments[i], true
}

// batchStream is the layout of one batch's input to ripgrep: where
// each run of it came from, and which part holds the lines the batch
// owns. What comes before that part is the lead-in and what comes after
// it the tail: lines of the batches beside this one, written so that
// the window of a match next to the batch's edge is whole, and reported
// only as context.
type batchStream struct {
	segments []streamSegment
	// ownedFrom and ownedTo bound the owned lines in ripgrep's input,
	// as a half-open range of byte positions.
	ownedFrom, ownedTo int64
}

// wholeStream is the layout of an input that is all owned lines.
func wholeStream(segments []streamSegment) batchStream {
	return batchStream{segments: segments, ownedFrom: 0, ownedTo: math.MaxInt64}
}

// owns reports whether the line starting at rgOffset in ripgrep's input
// is one the batch owns.
func (s batchStream) owns(rgOffset int64) bool {
	return rgOffset >= s.ownedFrom && rgOffset < s.ownedTo
}

// batchFeeder writes a batch's text into ripgrep's input and records a
// streamSegment wherever a new run of a frame begins, and where the
// lines the batch owns begin and end. Only the writer goroutine of
// scanFrameBatch touches it until that goroutine is done.
type batchFeeder struct {
	w        io.Writer // ripgrep's input
	written  int64     // bytes written so far
	newlines int       // '\n' bytes written so far
	segments []streamSegment
	// ownedFrom and ownedTo are batchStream's. Until startOwned is
	// called the batch owns nothing, and until the line break armed by
	// endOwnedAfterLineBreaks is written it owns everything after
	// ownedFrom.
	ownedFrom, ownedTo int64
	// endOwnedAtBreak, when above 0, is the count of line breaks
	// written at which the owned lines end.
	endOwnedAtBreak int
}

// newBatchFeeder returns a feeder writing to w that owns no line yet.
func newBatchFeeder(w io.Writer) *batchFeeder {
	return &batchFeeder{w: w, ownedFrom: math.MaxInt64, ownedTo: math.MaxInt64}
}

// startOwned records that the bytes written next begin the lines the
// batch owns.
func (b *batchFeeder) startOwned() {
	b.ownedFrom = b.written
}

// endOwnedAfterLineBreaks records that the owned lines end with the
// n-th line break written from now on. Each write may hold at most one
// line break, at its end, for the end to fall where the break is.
func (b *batchFeeder) endOwnedAfterLineBreaks(n int) {
	b.endOwnedAtBreak = b.newlines + n
}

// stream returns the layout of what was written.
func (b *batchFeeder) stream() batchStream {
	return batchStream{segments: b.segments, ownedFrom: b.ownedFrom, ownedTo: b.ownedTo}
}

// beginRun records that the bytes written next come from frame,
// starting at byte fileStart of the text, after frameNewlines line
// breaks of that frame that ripgrep is not given.
func (b *batchFeeder) beginRun(frame seekable.FrameInfo, fileStart int64, frameNewlines int) {
	b.segments = append(b.segments, streamSegment{
		frame:       frame,
		streamStart: b.written,
		fileStart:   fileStart,
		lineShift:   frameNewlines - b.newlines,
	})
}

// write hands p, which holds newlines line breaks, to ripgrep. An error
// means ripgrep stopped reading: it exited, or the scan was canceled.
func (b *batchFeeder) write(p []byte, newlines int) error {
	if _, err := b.w.Write(p); err != nil {
		return fmt.Errorf("%w: %w", errRipgrepStoppedReading, err)
	}
	b.written += int64(len(p))
	b.newlines += newlines
	if b.endOwnedAtBreak > 0 && b.newlines >= b.endOwnedAtBreak {
		b.ownedTo = b.written
		b.endOwnedAtBreak = 0
	}
	return nil
}

// readSeekTable is a small wrapper around seekable.ReadSeekTable that
// handles the file-open + size-lookup for callers that hold a pinned
// file. The seekable package's API takes an io.ReaderAt for
// test-friendliness; we'd rather not repeat the boilerplate here.
func readSeekTable(src sandbox.Pinned) (*seekable.SeekTable, error) {
	f, err := src.Open()
	if err != nil {
		return nil, fmt.Errorf("seekable: open %s: %w", src.Path(), err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("seekable: stat %s: %w", src.Path(), err)
	}
	return seekable.ReadSeekTable(f, fi.Size())
}

// ProcessSeekable runs the parallel frame scan over a seekable-zstd
// file. Each batch of frames is decompressed and piped through rg
// --json in its own goroutine, bounded by workerLimit().
//
// MatchRaw.LineNumber is frame-local (1-indexed within the match's
// own frame). numberFramesAgainstTheFile then fills AbsoluteLine with
// the file-absolute number, from the per-frame newline counts gathered
// during the scan; a match whose earlier frames were not all read keeps
// AbsoluteLine 0 (unknown).
//
// Offsets in MatchRaw.Offset are absolute byte offsets in the
// decompressed stream — same contract as Python.
//
// If maxResults is non-nil and the result set exceeds it, the matches
// latest in the file are dropped. Batches run in parallel and the cap
// cancels the ones still running, so the matches kept are the earliest
// of those collected, not always the earliest in the file.
//
// src is the file pinned when the trace checked it; every batch reads
// it only if it is still that file.
func ProcessSeekable(
	ctx context.Context,
	src sandbox.Pinned,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
	maxResults *int,
) (matches []MatchRaw, contexts []ContextRaw, elapsed time.Duration, err error) {
	start := time.Now()

	tbl, err := readSeekTable(src)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("ProcessSeekable: %w", err)
	}
	if tbl.NumFrames == 0 {
		return nil, nil, time.Since(start), nil
	}

	// Build the list of frame-index batches.
	var batches [][]int
	for i := 0; i < tbl.NumFrames; i += framesPerBatch {
		end := i + framesPerBatch
		if end > tbl.NumFrames {
			end = tbl.NumFrames
		}
		idxs := make([]int, 0, end-i)
		for j := i; j < end; j++ {
			idxs = append(idxs, j)
		}
		batches = append(batches, idxs)
	}
	// Each batch is one ripgrep run; gated helper, no-op in CLI mode.
	prometheus.RecordParallelTasks(len(batches))

	batchMatches := make([][]MatchRaw, len(batches))
	batchContexts := make([][]ContextRaw, len(batches))
	// Per-frame newline counts, gathered as the frames are decompressed
	// for the scan. They are what places a frame in the file: the lines
	// of every frame before it are the lines that precede it.
	batchFrameLines := make([][]frameLines, len(batches))

	workers := workerLimit()
	// cooperative cancel on max_results cap. Same pattern as
	// ProcessAllChunks (see worker.go's tally channel). When the cap is
	// reached, cancel the outer ctx; in-flight scanFrameBatch workers
	// see gctx.Done() either in exec.CommandContext (subprocess killed)
	// or inside remapBatchEvents' StreamEvents loop. Queued batches see
	// the cancel before they even spawn rg.
	//
	// Without this, a seekable-zstd file with max_results=10 would
	// decompress and scan EVERY frame batch to completion, applying the
	// cap only as a post-sort truncation. For a 10 GB compressed file
	// with 1000 frames, that's 99%+ wasted work.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(gctx)
	g.SetLimit(workers)

	tally := make(chan int, len(batches))
	done := make(chan struct{})
	var capHit bool
	go func() {
		defer close(done)
		total := 0
		for {
			select {
			case n, ok := <-tally:
				if !ok {
					return
				}
				total += n
				if maxResults != nil && total >= *maxResults && !capHit {
					capHit = true
					cancel()
				}
			case <-gctx.Done():
				return
			}
		}
	}()

	for bi := range batches {
		bi := bi
		frameIdxs := batches[bi]
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				// Queued batch saw cancel before starting — skip entirely.
				return nil
			}
			m, c, counted, berr := scanFrameBatch(
				gctx, src, tbl, frameIdxs,
				patternIDs, patternOrder, rgExtraArgs,
				contextBefore, contextAfter,
			)
			batchFrameLines[bi] = counted
			if berr != nil {
				if errors.Is(berr, context.Canceled) {
					// Cooperative cancel — swallow and publish any
					// partial matches we collected before the cancel.
					batchMatches[bi] = m
					batchContexts[bi] = c
					select {
					case tally <- len(m):
					default:
					}
					return nil
				}
				return berr
			}
			batchMatches[bi] = m
			batchContexts[bi] = c
			select {
			case tally <- len(m):
			default:
			}
			return nil
		})
	}
	waitErr := g.Wait()
	close(tally)
	<-done

	// Classify: cooperative cancel is expected, other errors surface.
	// Rewritten via De Morgan's law to satisfy staticcheck QF1001 —
	// the resulting form reads more naturally anyway ("if it's NOT a
	// cooperative cancel, surface the error").
	isCooperativeCancel := errors.Is(waitErr, context.Canceled) && capHit
	if waitErr != nil && !isCooperativeCancel {
		return nil, nil, time.Since(start), waitErr
	}

	for _, m := range batchMatches {
		matches = append(matches, m...)
	}
	for _, c := range batchContexts {
		contexts = append(contexts, c...)
	}
	numberFramesAgainstTheFile(tbl, batchFrameLines, matches, contexts)

	// Put the batches' results in file order. The offset is a position in
	// the whole decompressed stream; LineNumber restarts in every frame,
	// so ordering by it would interleave the frames and the cap below
	// would keep line 1 of each frame rather than the first lines found.
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Offset < matches[j].Offset
	})

	// Final hard cap — the cooperative cancel may overshoot (a batch
	// that started before cancel can still produce matches past the
	// cap). Truncate to the exact cap for the caller's contract. A match
	// cut here can be a line of the window of a match kept, so it stays
	// as a context line.
	if maxResults != nil && len(matches) > *maxResults {
		matches, contexts = matchesAsContext(matches, contexts, len(matches)-*maxResults)
	}
	sort.SliceStable(contexts, func(i, j int) bool {
		return contexts[i].Offset < contexts[j].Offset
	})

	return matches, contexts, time.Since(start), nil
}

// frameDecoder wraps an open *os.File and a pooled zstd decoder for
// the writer goroutine in scanFrameBatch. It is intentionally small
// and package-private — the only callers are the streaming pipe
// writer and the decompressFrameForBatch test seam.
//
// Go note: the pooled decoder (from compression.AcquireDecoder)
// holds ~2 MB of zstd decoding tables. We acquire it once per batch
// and reuse it for every frame in the batch via DecodeAll (stateless).
type frameDecoder struct {
	f   *os.File      // open fd for pread-style ReadAt
	zd  *zstd.Decoder // pooled; release on batch exit
	buf []byte        // scratch for compressed frame bytes, reused
}

// decompressFrameForBatch is the per-frame decompression step used by
// scanFrameBatch's writer goroutine. Exposed as a package-level var so
// tests can wrap it (e.g. to count how many times streaming occurred)
// without modifying production code paths.
//
// Returns caller-owned decompressed bytes for one frame.
var decompressFrameForBatch = func(dec *frameDecoder, frame seekable.FrameInfo) ([]byte, error) {
	// Reuse the scratch buffer when it's large enough, else grow.
	// Saves one allocation per frame on the hot path.
	if int64(cap(dec.buf)) < frame.CompressedSize {
		dec.buf = make([]byte, frame.CompressedSize)
	} else {
		dec.buf = dec.buf[:frame.CompressedSize]
	}
	if _, err := dec.f.ReadAt(dec.buf, frame.CompressedOffset); err != nil {
		return nil, fmt.Errorf("read frame at %d: %w", frame.CompressedOffset, err)
	}
	out, err := dec.zd.DecodeAll(dec.buf, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress frame at %d: %w", frame.CompressedOffset, err)
	}
	return out, nil
}

// numberFramesAgainstTheFile turns frame-relative line numbers into the
// file's own, in place.
//
// Every frame that was decompressed counted its newlines on the way
// past, so the frames before a frame give the lines before it: frame 0
// starts at line 1, and each frame after it starts one line after its
// predecessor's last. A frame that never got read breaks the chain, and
// the matches in every frame after it keep the unknown marker rather
// than a number counted from the wrong place — that is what a cap
// leaves behind when it stops the scan early.
func numberFramesAgainstTheFile(
	tbl *seekable.SeekTable,
	batches [][]frameLines,
	matches []MatchRaw,
	contexts []ContextRaw,
) {
	if tbl == nil || tbl.NumFrames == 0 {
		return
	}
	counted := make([]int, tbl.NumFrames)
	known := make([]bool, tbl.NumFrames)
	for _, batch := range batches {
		for _, fl := range batch {
			if fl.frameIdx >= 0 && fl.frameIdx < tbl.NumFrames {
				counted[fl.frameIdx] = fl.lines
				known[fl.frameIdx] = true
			}
		}
	}

	// First line of each frame, for as long as the chain holds.
	firstLine := make([]int, tbl.NumFrames)
	line := 1
	for i := 0; i < tbl.NumFrames; i++ {
		if !known[i] {
			break
		}
		firstLine[i] = line
		line += counted[i]
	}

	frameOf := func(offset int64) int {
		// The frame whose decompressed range holds this offset.
		lo, hi := 0, tbl.NumFrames-1
		found := -1
		for lo <= hi {
			mid := (lo + hi) / 2
			f := tbl.Frames[mid]
			switch {
			case offset < f.DecompressedOffset:
				hi = mid - 1
			case offset >= f.DecompressedOffset+f.DecompressedSize:
				lo = mid + 1
			default:
				found = mid
				lo, hi = 1, 0
			}
		}
		return found
	}

	for i := range matches {
		frame := frameOf(matches[i].Offset)
		if frame < 0 || firstLine[frame] == 0 {
			continue
		}
		matches[i].AbsoluteLine = firstLine[frame] + matches[i].LineNumber - 1
	}
	for i := range contexts {
		frame := frameOf(contexts[i].Offset)
		if frame < 0 || firstLine[frame] == 0 {
			continue
		}
		contexts[i].AbsoluteLine = firstLine[frame] + contexts[i].LineNumber - 1
	}
}

// frameLines is one frame's newline count, measured while the frame was
// decompressed for the scan.
type frameLines struct {
	frameIdx int
	lines    int
}

// scanFrameBatch scans the lines a batch of consecutive frames owns
// with one rg process, and returns rg's matches and context lines in
// the file's offsets, numbered within their frames.
//
// INVARIANT: every line of the text is scanned whole, by exactly one
// batch, wherever the frame boundaries fall. A batch's input to rg
// runs from just after the first line break at or after the batch's
// first byte (from byte 0 for the first batch) to the first line
// break at or after the end of its frames, that break included (to
// the end of the text for the last batch). Two batches next to each
// other meet at the same line break, so neither waits for the other.
// feedBatchLines says how the input is built.
//
// The writer goroutine decompresses one frame at a time and streams it
// to rg through an io.Pipe, so memory holds about one frame in flight
// (occasionally two, with pipe buffering) rather than the whole batch.
// It records a streamSegment for every run of a frame it writes, and
// remapBatchEvents turns rg's positions and line numbers back into the
// file's with them.
//
// Uses compression.AcquireDecoder / ReleaseDecoder to avoid the ~2 MB
// per-frame decoding-table allocation: one decoder per batch is reused
// across all frames in the batch via stateless DecodeAll calls.
//
// Parity: rx-python/src/rx/trace_compressed.py::process_seekable_zstd_frame_batch
func scanFrameBatch(
	ctx context.Context,
	src sandbox.Pinned,
	tbl *seekable.SeekTable,
	frameIdxs []int,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
) (matches []MatchRaw, contexts []ContextRaw, counted []frameLines, err error) {
	// gated helpers — CLI mode skips collection.
	prometheus.IncActiveWorkers()
	defer prometheus.DecActiveWorkers()
	defer func() { recordWorkerOutcome(err) }()

	// Open the file once per batch — reused by the writer goroutine
	// for every frame's ReadAt. os.File.ReadAt is safe for concurrent
	// use (pread(2) under the hood on Linux/macOS), though we only use
	// it single-threaded in the writer goroutine here.
	f, err := src.Open()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scanFrameBatch: open %s: %w", src.Path(), err)
	}
	defer func() { _ = f.Close() }()

	// One entry per frame of the batch; the writer goroutine fills in
	// each frame's newline count as it decompresses it. The slice is
	// sized up front, and the main goroutine reads it only after the
	// writer is done (the <-writerDone barrier below), so the two never
	// touch it at the same time.
	locs := make([]frameLoc, len(frameIdxs))
	for i, fi := range frameIdxs {
		locs[i] = frameLoc{frameIdx: fi}
	}

	// Build rg argv (same as ProcessChunk).
	rgArgs := newRgArgs()
	if contextBefore > 0 {
		rgArgs = append(rgArgs, "-B", strconv.Itoa(contextBefore))
	}
	if contextAfter > 0 {
		rgArgs = append(rgArgs, "-A", strconv.Itoa(contextAfter))
	}
	for _, pid := range patternOrder {
		rgArgs = append(rgArgs, "-e", patternIDs[pid])
	}
	rgArgs = append(rgArgs, filterIncompatibleRgArgs(rgExtraArgs)...)
	rgArgs = append(rgArgs, "-")

	// io.Pipe: writer goroutine feeds decompressed frames in, rg
	// reads on the other end. Crucially, rgCmd.Stdin = pr does NOT
	// buffer — exec.Cmd reads from the pipe reader as rg demands,
	// so the writer only advances at rg's consumption rate. Memory
	// stays bounded to ~O(1 frame in flight).
	pr, pw := io.Pipe()

	rgCmd := exec.CommandContext(ctx, "rg", rgArgs...)
	rgCmd.Stdin = pr
	var stdout bytes.Buffer
	rgCmd.Stdout = &stdout
	var stderr strings.Builder
	rgCmd.Stderr = &stderr

	// Writer goroutine: write the batch's lines into the pipe. It must
	// ALWAYS close pw so rg sees EOF and exits. pw.CloseWithError hands
	// a decompression failure or a cancel to the reader side, where rg
	// sees its input end and exits with what it already received; a
	// later pw.Close does not overwrite that error.
	//
	// The feeder, like locs, belongs to this goroutine until writerDone
	// closes; the main goroutine reads feeder.segments only after that.
	feeder := newBatchFeeder(pw)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { _ = pw.Close() }()
		ferr := feedBatchLines(ctx, feeder, batchSource{f: f, tbl: tbl}, frameIdxs, locs, contextBefore, contextAfter)
		// errRipgrepStoppedReading means rg exited early (a cap fired or
		// the scan was canceled): nothing to hand on, rg's own exit
		// reports what happened.
		if ferr != nil && !errors.Is(ferr, errRipgrepStoppedReading) {
			_ = pw.CloseWithError(ferr)
		}
	}()

	// Run rg. Cmd.Run waits for rg to exit AFTER its stdin (pr) hits
	// EOF, which happens when the writer goroutine calls pw.Close().
	// If rg exits early (e.g. killed by ctx cancel via
	// exec.CommandContext), pr.Read returns ErrClosedPipe inside the
	// writer's pw.Write, and the writer aborts cleanly.
	runErr := rgCmd.Run()

	// Close the read half before waiting for the writer.
	//
	// An io.Pipe write blocks until someone reads it, and the only
	// reader is the copy exec.Cmd runs into rg's stdin. When rg exits
	// early — killed because a max_results cap fired, or because the
	// request was canceled — that copy stops, and a writer part-way
	// through a frame would block on pw.Write forever, taking the
	// <-writerDone below with it. Closing pr makes that Write return
	// io.ErrClosedPipe instead, which the writer treats as "the reader
	// is gone" and exits. Run has already returned here, so exec is
	// finished with pr and this cannot race it.
	_ = pr.Close()

	// Wait for the writer to finish. After Run returns and pr is
	// closed, writerDone closes promptly on every path. This barrier
	// establishes a happens-before edge for locs and the feeder's
	// layout below.
	<-writerDone

	if runErr != nil {
		// A canceled context trumps every other reading of rg's exit.
		// exec.CommandContext kills rg on cancel, which arrives here as
		// a signal exit (code -1) — expected when a cap fired or the
		// request was abandoned, and not a reason to call the file
		// unreadable. The chunked path classifies it the same way.
		if cErr := ctx.Err(); cErr != nil {
			return matchesFromPartialBatch(ctx, stdout.Bytes(), feeder.stream(), locs, patternOrder, contextAfter)
		}
		var ex *exec.ExitError
		if errors.As(runErr, &ex) {
			code := ex.ExitCode()
			if code != 0 && code != 1 {
				return nil, nil, nil, fmt.Errorf("rg exit %d: %s",
					code, strings.TrimSpace(stderr.String()))
			}
		} else if !errors.Is(runErr, context.Canceled) {
			return nil, nil, nil, fmt.Errorf("rg run: %w", runErr)
		}
	}

	// Parse rg's stdout and remap. rg has finished and its whole output
	// is in memory, so the parse runs to the end even when a cap cancels
	// the scan meanwhile: a parse stopped part-way would keep a match
	// without the context lines rg wrote after it, a window cut short.
	matches, contexts, err = remapBatchEvents(context.WithoutCancel(ctx), stdout.Bytes(), feeder.stream(), patternOrder)
	if err != nil {
		// The output could not be parsed past some point: the batch
		// fails rather than report the matches before it as all.
		return matches, contexts, countedFrames(locs), fmt.Errorf("read rg output: %w", err)
	}
	return matches, contexts, countedFrames(locs), nil
}

// countedFrames reports the newline count measured for each frame the
// writer got through, zero included. A frame the writer never reached
// counted nothing and is left out, so the caller can tell "zero lines"
// from "not read".
func countedFrames(locs []frameLoc) []frameLines {
	out := make([]frameLines, 0, len(locs))
	for _, loc := range locs {
		if loc.decoded {
			out = append(out, frameLines{frameIdx: loc.frameIdx, lines: loc.lineCount})
		}
	}
	return out
}

// matchesFromPartialBatch reads whatever ripgrep managed to write
// before it was killed and hands it back alongside context.Canceled.
// ProcessSeekable keeps those matches and treats the error as the
// cooperative cancel it is, so a cap that fires mid-batch still returns
// the matches the batch had already found.
//
// The kill can land between a match and the contextAfter lines rg
// writes after it. Such a match comes back as a context line rather
// than a match with a shortened window (see unfinishedWindows). The cap
// counts only batches that finished, so leaving it out never takes the
// answer below the cap.
//
// The parse runs on a context stripped of the cancellation, since the
// output is already buffered and the only thing left to do is read it.
func matchesFromPartialBatch(
	ctx context.Context,
	out []byte,
	stream batchStream,
	locs []frameLoc,
	patternOrder []string,
	contextAfter int,
) ([]MatchRaw, []ContextRaw, []frameLines, error) {
	// rg was killed part-way, so its last line may be cut off; a stream
	// that cannot be read to the end is expected here, and the matches
	// read before that point are kept.
	matches, contexts, _ := remapBatchEvents(context.WithoutCancel(ctx), out, stream, patternOrder)
	matches, contexts = matchesAsContext(matches, contexts, unfinishedWindows(matches, contexts, contextAfter))
	return matches, contexts, countedFrames(locs), context.Canceled
}

// unfinishedWindows counts the latest matches of a stopped rg run whose
// trailing windows rg did not finish writing.
//
// rg writes the contextAfter lines after a match right after it, each
// as a context event or, when it matches too, as a match event, so a
// match with at least contextAfter events after it has its whole
// window. Both slices are in the order rg wrote them, which is offset
// order, and a match short of its window has only such matches after
// it.
func unfinishedWindows(matches []MatchRaw, contexts []ContextRaw, contextAfter int) int {
	unfinished := 0
	for i := len(matches) - 1; i >= 0; i-- {
		laterMatches := len(matches) - 1 - i
		laterContexts := len(contexts) - sort.Search(len(contexts), func(j int) bool {
			return contexts[j].Offset > matches[i].Offset
		})
		if laterMatches+laterContexts >= contextAfter {
			break
		}
		unfinished++
	}
	return unfinished
}

// remapBatchEvents parses the rg --json stream emitted for a batch and
// places each event in the file: its offset in the decompressed text,
// its line number within its frame, and the frame that holds it.
// stream, recorded while the batch's input was written, says where each
// run of that input came from and which lines the batch owns; a match
// on a line it does not own comes back as a context line.
//
// ctx propagates from the parent scanner — if the outer context is
// canceled mid-parse (a cap fired, an errgroup sibling failed, or the
// HTTP request was aborted), StreamEvents stops calling the callback
// and returns the context's error, which comes back beside the matches
// parsed so far.
func remapBatchEvents(
	ctx context.Context,
	rgStdout []byte,
	stream batchStream,
	patternOrder []string,
) ([]MatchRaw, []ContextRaw, error) {
	var matches []MatchRaw
	var contexts []ContextRaw
	// A line that is not a valid event stops the parse, as it stops the
	// chunked and the stream paths: every match after it would be
	// missing, so the error goes back to the caller beside the matches
	// parsed so far. rg's output is already complete in memory here, so
	// stopping early leaves no process blocked on a pipe.
	streamErr := StreamEvents(ctx, bytes.NewReader(rgStdout), func(ev *RgEvent, parseErr error) error {
		if parseErr != nil {
			return parseErr
		}
		if ev == nil {
			return nil
		}
		switch ev.Type {
		case RgEventMatch:
			if ev.Match == nil {
				return nil
			}
			seg, ok := segmentHolding(stream.segments, ev.Match.AbsoluteOffset)
			if !ok {
				return nil
			}
			offset := seg.fileStart + ev.Match.AbsoluteOffset - seg.streamStart
			line := MatchRaw{
				Offset:       offset,
				End:          offset + int64(ev.Match.Lines.Size),
				LineNumber:   ev.Match.LineNumber + seg.lineShift,
				LineText:     trimTrailingNewline(ev.Match.Lines.Text),
				IsCompressed: true,
				FrameIndex:   seg.frame.Index,
			}
			if !stream.owns(ev.Match.AbsoluteOffset) {
				// The batch beside this one owns the line and reports
				// the match; here it is a line of a window.
				contexts = append(contexts, matchAsContext(line))
				return nil
			}
			line.Submatches = make([]rxtypes.Submatch, len(ev.Match.Submatches))
			for i, sm := range ev.Match.Submatches {
				line.Submatches[i] = rxtypes.Submatch{Text: sm.Text(), Start: sm.Start, End: sm.End}
			}
			line.PatternIDs = append([]string(nil), patternOrder...)
			matches = append(matches, line)
		case RgEventContext:
			if ev.Context == nil {
				return nil
			}
			seg, ok := segmentHolding(stream.segments, ev.Context.AbsoluteOffset)
			if !ok {
				return nil
			}
			offset := seg.fileStart + ev.Context.AbsoluteOffset - seg.streamStart
			contexts = append(contexts, ContextRaw{
				Offset:     offset,
				End:        offset + int64(ev.Context.Lines.Size),
				LineNumber: ev.Context.LineNumber + seg.lineShift,
				LineText:   trimTrailingNewline(ev.Context.Lines.Text),
			})
		}
		return nil
	})
	return matches, contexts, streamErr
}

// errRipgrepStoppedReading reports that a write into rg's input failed
// because rg no longer reads it: it exited early, or the scan was
// canceled. It ends the writer without an error of its own.
var errRipgrepStoppedReading = errors.New("rg stopped reading its input")

// batchSource is the seekable file a batch reads: the open file and
// its seek table.
type batchSource struct {
	f   *os.File
	tbl *seekable.SeekTable
}

// feedBatchLines writes the lines a batch owns into rg's input, with
// the contextBefore lines before them and the contextAfter lines after
// them, and counts the line breaks of each of the batch's frames into
// locs on the way.
//
// A batch other than the first skips its text up to and including the
// first line break at or after its first byte. Those bytes end a line
// that starts before the batch, or, when the frame before the batch
// ends with a line break, they are the batch's first line, which the
// batch before reads instead. A batch other than the last then reads on
// past its last frame, up to and including the first line break at or
// after the end of its frames: the line break the next batch skips to.
//
// A batch whose frames hold no line break at all owns no line: its
// frames lie inside a line that an earlier batch reads in full.
//
// On a file whose frames end at line breaks, as rx compress writes
// them, a batch thus hands its first line to the batch before and reads
// the next batch's first line in its place: the same amount of text,
// and nothing decoded twice beyond that one line.
//
// The lines around the owned ones are the lead-in (the skipped bytes,
// and before them as much of the frames before the batch as holds the
// contextBefore lines) and the tail (the contextAfter lines after the
// owned ones). The feeder marks where the owned lines begin and end, so
// a match outside them is reported as context only. Without context
// nothing is written but the owned lines; with it, a lead-in usually
// costs decoding the one frame before the batch.
func feedBatchLines(
	ctx context.Context,
	feeder *batchFeeder,
	src batchSource,
	frameIdxs []int,
	locs []frameLoc,
	contextBefore, contextAfter int,
) error {
	// Acquire one decoder per batch; release at exit. Reusing it
	// across all frames in the batch avoids a ~2 MB allocation per
	// frame.
	zd := compression.AcquireDecoder()
	defer compression.ReleaseDecoder(zd)
	dec := &frameDecoder{f: src.f, zd: zd}

	// The lead-in is held back until the batch's first owned line is
	// found and written just before it. A batch whose frames all lie
	// inside one line owns nothing and writes nothing: its lead-in and
	// frames would hand rg part of a line, which rg reports as a line.
	var leadIn []textPiece
	skipping := frameIdxs[0] > 0
	if !skipping {
		feeder.startOwned() // the first batch owns the text from byte 0
	} else if contextBefore > 0 {
		var err error
		if leadIn, err = linesBeforeFrame(ctx, dec, src.tbl, frameIdxs[0], contextBefore); err != nil {
			return err
		}
	}
	for i, fi := range frameIdxs {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame := src.tbl.Frames[fi]
		data, err := decompressFrameForBatch(dec, frame)
		if err != nil {
			return err
		}
		locs[i].lineCount = bytesCountByte(data, '\n')
		locs[i].decoded = true

		from, skippedLineBreaks := 0, 0
		if skipping {
			lineBreak := bytes.IndexByte(data, '\n')
			if lineBreak < 0 {
				lineBreak = len(data) - 1 // all of it lies inside a line an earlier batch reads
			} else {
				skipping = false
			}
			from = lineBreak + 1
			skippedLineBreaks = locs[i].lineCount - bytesCountByte(data[from:], '\n')
			if contextBefore > 0 {
				// The skipped bytes end the line just before the owned
				// ones: lead-in.
				leadIn = append(leadIn, textPiece{frame: frame, data: data[:from]})
			}
			if skipping {
				continue
			}
			for _, piece := range leadIn {
				if err := piece.writeTo(feeder); err != nil {
					return err
				}
			}
			feeder.startOwned()
		}
		if from == len(data) {
			continue // nothing of this frame is the batch's to scan
		}
		feeder.beginRun(frame, frame.DecompressedOffset+int64(from), skippedLineBreaks)
		if err := feeder.write(data[from:], locs[i].lineCount-skippedLineBreaks); err != nil {
			return err
		}
	}

	last := frameIdxs[len(frameIdxs)-1]
	if skipping || last+1 >= src.tbl.NumFrames {
		// Either no line starts in this batch, or no frame follows it
		// and the owned lines run to the end of the text.
		return nil
	}
	feeder.endOwnedAfterLineBreaks(1)
	return feedThroughLineBreaks(ctx, feeder, src, last+1, 1+contextAfter)
}

// textPiece is a run of one frame's decompressed text, from its byte
// from to the end of data.
type textPiece struct {
	frame seekable.FrameInfo
	data  []byte
	from  int
}

// writeTo hands the piece to rg's input as a run of its frame.
func (pc textPiece) writeTo(feeder *batchFeeder) error {
	feeder.beginRun(pc.frame, pc.frame.DecompressedOffset+int64(pc.from), bytesCountByte(pc.data[:pc.from], '\n'))
	return feeder.write(pc.data[pc.from:], bytesCountByte(pc.data[pc.from:], '\n'))
}

// linesBeforeFrame returns, in file order, the text of the lines lines
// that end the text before frame first: the text after the lines-th
// line break counting back from the frame's first byte, or the whole
// text before it when it holds fewer. It decodes the frames before
// first from the last one back, as many as those lines take.
func linesBeforeFrame(
	ctx context.Context,
	dec *frameDecoder,
	tbl *seekable.SeekTable,
	first, lines int,
) ([]textPiece, error) {
	var pieces []textPiece // last frame first
	breaks := 0
	for fi := first - 1; fi >= 0 && breaks < lines; fi-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frame := tbl.Frames[fi]
		data, err := decompressFrameForBatch(dec, frame)
		if err != nil {
			return nil, err
		}
		from := 0
		for i := len(data) - 1; i >= 0; i-- {
			if data[i] != '\n' {
				continue
			}
			breaks++
			if breaks == lines {
				from = i + 1
				break
			}
		}
		pieces = append(pieces, textPiece{frame: frame, data: data, from: from})
	}
	slices.Reverse(pieces)
	return pieces, nil
}

// lineEndReadSize is how much decompressed text feedThroughLineBreaks
// asks for at a time. A log line is far shorter, so reading past a
// batch's last frame usually decodes one zstd block of the next frame
// and stops.
const lineEndReadSize = 16 << 10

// feedThroughLineBreaks writes the text from the first byte of frame
// next up to and including its breaks-th line break into rg's input,
// through as many frames as those lines take. At the end of the text it
// stops without that many.
//
// It decodes the frames as a stream and stops at the line break rather
// than decoding each frame whole, so on a file whose frames end at line
// breaks it costs the lines and about one zstd block per batch.
func feedThroughLineBreaks(
	ctx context.Context,
	feeder *batchFeeder,
	src batchSource,
	next, breaks int,
) error {
	// Concurrency 1 makes the stream decoder synchronous: it decodes a
	// block when Read asks for bytes, rather than decoding ahead in
	// goroutines of its own, so stopping early leaves no work behind.
	zd, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return fmt.Errorf("create zstd stream decoder: %w", err)
	}
	defer zd.Close()

	buf := make([]byte, lineEndReadSize)
	for fi := next; fi < src.tbl.NumFrames && breaks > 0; fi++ {
		frame := src.tbl.Frames[fi]
		// A SectionReader limits the decoder to this frame's compressed
		// bytes and reads them with ReadAt, so it shares no file cursor.
		if resetErr := zd.Reset(io.NewSectionReader(src.f, frame.CompressedOffset, frame.CompressedSize)); resetErr != nil {
			return fmt.Errorf("decompress frame at %d: %w", frame.CompressedOffset, resetErr)
		}
		// No line break of this frame comes before its first byte.
		feeder.beginRun(frame, frame.DecompressedOffset, 0)
		if breaks, err = feedThroughLineBreak(ctx, feeder, zd, buf, breaks); err != nil {
			return err
		}
	}
	return nil // the lines asked for are written, or the text has ended
}

// feedThroughLineBreak copies text from r into rg's input up to and
// including its breaks-th line break, and returns how many of those
// line breaks r ended before. Each write holds at most one line break,
// at its end, which is what lets the feeder mark where the owned lines
// end.
func feedThroughLineBreak(ctx context.Context, feeder *batchFeeder, r io.Reader, buf []byte, breaks int) (int, error) {
	for breaks > 0 {
		if err := ctx.Err(); err != nil {
			return breaks, err
		}
		n, readErr := r.Read(buf)
		data := buf[:n]
		for len(data) > 0 && breaks > 0 {
			lineBreak := bytes.IndexByte(data, '\n')
			if lineBreak < 0 {
				if err := feeder.write(data, 0); err != nil {
					return breaks, err
				}
				break
			}
			if err := feeder.write(data[:lineBreak+1], 1); err != nil {
				return breaks, err
			}
			data = data[lineBreak+1:]
			breaks--
		}
		if errors.Is(readErr, io.EOF) {
			return breaks, nil
		}
		if readErr != nil {
			return breaks, fmt.Errorf("decompress a frame after the batch: %w", readErr)
		}
	}
	return 0, nil
}

// bytesCountByte counts how many `target` bytes appear in data. Used
// to count '\n' per frame for line-number remapping.
//
// Inlined so the hot path isn't disrupted by a cgo-sized bytes.Count.
// bytes.Count would work just as well; we stay verbatim to Python's
// `frame_data.count(b'\n')`.
func bytesCountByte(data []byte, target byte) int {
	n := 0
	for i := 0; i < len(data); i++ {
		if data[i] == target {
			n++
		}
	}
	return n
}

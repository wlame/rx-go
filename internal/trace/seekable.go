package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	// cutAt is where the whole lines of the input end when a damaged
	// frame stopped it: the position just after the last line break
	// written before the damage. What ripgrep was given from there on
	// is the start of a line whose rest lies in the damaged frame, and
	// ripgrep would report it as a line of its own, so nothing that
	// starts there is reported. MaxInt64 when nothing was cut.
	cutAt int64
}

// wholeStream is the layout of an input that is all owned lines.
func wholeStream(segments []streamSegment) batchStream {
	return batchStream{segments: segments, ownedFrom: 0, ownedTo: math.MaxInt64, cutAt: math.MaxInt64}
}

// owns reports whether the line starting at rgOffset in ripgrep's input
// is one the batch owns.
func (s batchStream) owns(rgOffset int64) bool {
	return rgOffset >= s.ownedFrom && rgOffset < s.ownedTo
}

// whole reports whether the line starting at rgOffset in ripgrep's
// input is a whole line of the text, rather than the start of a line a
// damaged frame cut.
func (s batchStream) whole(rgOffset int64) bool {
	return rgOffset < s.cutAt
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
	// lineEnd is the position just after the last line break written,
	// 0 before the first: where the whole lines written so far end.
	lineEnd int64
	// cutAt is batchStream's, set by cut.
	cutAt int64
}

// newBatchFeeder returns a feeder writing to w that owns no line yet.
func newBatchFeeder(w io.Writer) *batchFeeder {
	return &batchFeeder{w: w, ownedFrom: math.MaxInt64, ownedTo: math.MaxInt64, cutAt: math.MaxInt64}
}

// cut records that the text stops being whole after the last line
// break written: a damaged frame follows, and the bytes written after
// that line break are only the start of a line. The writer stops
// writing after a cut.
func (b *batchFeeder) cut() {
	b.cutAt = b.lineEnd
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
	return batchStream{segments: b.segments, ownedFrom: b.ownedFrom, ownedTo: b.ownedTo, cutAt: b.cutAt}
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
	if newlines > 0 {
		b.lineEnd = b.written + int64(bytes.LastIndexByte(p, '\n')) + 1
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
// A damaged frame (seekable.ErrDamagedFrame) does not stop the scan.
// Every line that lies wholly in frames that decompress is searched,
// and the lines that touch a damaged frame are not: the ones in it, the
// one running into it and the one running out of it, which may have
// started in it. The matches found come back beside an error wrapping
// seekable.ErrDamagedFrame that names the damaged frames, and the
// damage is logged as a warning. The line numbers after the first
// damaged frame stay unknown, since its line count is lost.
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

	// The damaged frames each batch met, in the order it met them. Like
	// the slices above, each goroutine writes only its own element, and
	// this goroutine reads them only after g.Wait.
	batchDamage := make([][]*damagedFrameError, len(batches))

	for bi := range batches {
		bi := bi
		g.Go(func() error {
			// A batch is one ripgrep run unless one of its frames is
			// damaged. That run then stops at the last whole line before
			// the damage, and the frames after the damaged one go to a
			// run of their own, so every line that lies wholly in intact
			// frames is searched. Each run starts after the frame that
			// ended the one before, so a batch makes at most one run per
			// frame.
			run := frameRun{frames: batches[bi]}
			for {
				if err := gctx.Err(); err != nil {
					// A queued batch, or the rest of one, saw the cancel
					// before it started: skip it.
					break
				}
				m, c, counted, berr := scanFrameBatch(
					gctx, src, tbl, run,
					patternIDs, patternOrder, rgExtraArgs,
					contextBefore, contextAfter,
				)
				batchFrameLines[bi] = append(batchFrameLines[bi], counted...)
				var damage *damagedFrameError
				if errors.As(berr, &damage) {
					// The matches before the damage are real. Keep them
					// and go on after the damaged frame.
					batchMatches[bi] = append(batchMatches[bi], m...)
					batchContexts[bi] = append(batchContexts[bi], c...)
					batchDamage[bi] = append(batchDamage[bi], damage)
					next, ok := run.after(damage.frame)
					if !ok {
						break
					}
					run = next
					continue
				}
				if berr != nil && !errors.Is(berr, context.Canceled) {
					return berr
				}
				// Done, or a cooperative cancel: keep and publish the
				// matches collected before it.
				batchMatches[bi] = append(batchMatches[bi], m...)
				batchContexts[bi] = append(batchContexts[bi], c...)
				break
			}
			select {
			case tally <- len(batchMatches[bi]):
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
	// A frame can be met by two batches: by the one it belongs to, and
	// by the batch before when that one reads on past its last frame to
	// finish its last line.
	damaged := damagedFramesOf(src.Path(), batchDamage)
	if damaged != nil {
		slog.Default().Warn("seekable_damaged_frames", "path", src.Path(), "error", damaged.Error())
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

	return matches, contexts, time.Since(start), damaged
}

// maxDamagedFramesNamed is how many damaged frames an error names one
// by one; it counts the rest. A file damaged throughout gives an error
// of a bounded length.
const maxDamagedFramesNamed = 10

// damagedFramesOf returns nil when no batch met a damaged frame, and
// otherwise an error wrapping seekable.ErrDamagedFrame that names the
// file and its damaged frames in file order, each once, with the
// decoder's reason for the first of them.
func damagedFramesOf(path string, batches [][]*damagedFrameError) error {
	reasons := map[int]error{}
	for _, batch := range batches {
		for _, damage := range batch {
			if _, seen := reasons[damage.frame]; !seen {
				reasons[damage.frame] = damage.err
			}
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	frames := slices.Sorted(maps.Keys(reasons))
	named := make([]string, 0, maxDamagedFramesNamed)
	for _, frame := range frames[:min(len(frames), maxDamagedFramesNamed)] {
		named = append(named, strconv.Itoa(frame))
	}
	list := strings.Join(named, ", ")
	if more := len(frames) - len(named); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	// The first reason already says "seekable zstd frame is damaged:
	// frame N", so it carries the sentinel for errors.Is.
	return fmt.Errorf("%s: searched around damaged frames %s: %w", path, list, reasons[frames[0]])
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
// Returns caller-owned decompressed bytes for one frame. An error
// wraps seekable.ErrDamagedFrame when the bytes were read and do not
// decompress into the frame's text; a failed read is returned as it is.
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
	return seekable.DecodeFrame(dec.zd, dec.buf, frame)
}

// frameRun is the run of consecutive frames one ripgrep run reads: a
// whole batch, or the frames of a batch after a damaged one.
type frameRun struct {
	frames []int // indexes in the file, ascending
	// followsDamage says the frame just before the run is damaged. The
	// run then starts at the first line break of its frames, as every
	// batch after the first does, and writes no lead-in, because the
	// lines before that line break cannot be read whole.
	followsDamage bool
}

// after returns the run of r's frames that come after frame damaged,
// and false when none does.
func (r frameRun) after(damaged int) (frameRun, bool) {
	// sort.SearchInts finds the first frame numbered damaged+1 or more.
	rest := r.frames[sort.SearchInts(r.frames, damaged+1):]
	if len(rest) == 0 {
		return frameRun{}, false
	}
	return frameRun{frames: rest, followsDamage: true}, true
}

// damagedFrameError is what scanFrameBatch returns when a frame its
// ripgrep run needed is damaged. The matches and context lines returned
// beside it are those of the whole lines before the damage; the frames
// of the batch after the damaged one are left for a run of their own.
type damagedFrameError struct {
	frame int   // the damaged frame's index in the file
	err   error // wraps seekable.ErrDamagedFrame
}

func (e *damagedFrameError) Error() string { return e.err.Error() }

// Unwrap lets errors.Is see seekable.ErrDamagedFrame through it.
func (e *damagedFrameError) Unwrap() error { return e.err }

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
// A frame that is damaged (seekable.ErrDamagedFrame) ends rg's input
// after the last whole line before it, and the run returns the matches
// of the lines rg got beside a *damagedFrameError naming the frame. Any
// other failure to read or decompress the frames fails the run, however
// rg exited.
//
// Parity: rx-python/src/rx/trace_compressed.py::process_seekable_zstd_frame_batch
func scanFrameBatch(
	ctx context.Context,
	src sandbox.Pinned,
	tbl *seekable.SeekTable,
	run frameRun,
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
	locs := make([]frameLoc, len(run.frames))
	for i, fi := range run.frames {
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

	// rg runs under its own context so the reader below can kill it when
	// rg's output cannot be parsed, without canceling the caller's
	// context. exec.CommandContext kills the process when that context
	// ends; the deferred cancel releases its resources on every return.
	rgCtx, killRg := context.WithCancel(ctx)
	defer killRg()
	rgCmd := exec.CommandContext(rgCtx, "rg", rgArgs...)
	rgCmd.Stdin = pr
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scanFrameBatch: rg stdout pipe: %w", err)
	}
	var stderr strings.Builder
	rgCmd.Stderr = &stderr

	// Writer goroutine: write the batch's lines into the pipe. It must
	// ALWAYS close pw so rg sees EOF and exits. pw.CloseWithError hands
	// a read failure or a cancel to the reader side, where rg sees its
	// input end and exits with what it already received; a later
	// pw.Close does not overwrite that error.
	//
	// The feeder, like locs, belongs to this goroutine until writerDone
	// closes; the main goroutine reads feeder.segments only after that.
	//
	// writeErr is the goroutine's outcome. The goroutine sets it before
	// close(writerDone) runs (deferred calls run last-in, first-out), and
	// the main goroutine reads it only after <-writerDone, so the channel
	// close orders the write before the read and no lock is needed. It
	// is read whatever rg's exit says: exec reports the pipe's error only
	// when rg exits 0, and rg exits 1 when the text it got held no match.
	var writeErr error
	feeder := newBatchFeeder(pw)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { _ = pw.Close() }()
		writeErr = feedBatchLines(ctx, feeder, batchSource{f: f, tbl: tbl}, run, locs, contextBefore, contextAfter)
		// errRipgrepStoppedReading means rg exited early (a cap fired or
		// the scan was canceled): nothing to hand on, rg's own exit
		// reports what happened. A damaged frame ends the input after the
		// last whole line, which rg reads to its end as any other input:
		// the plain pw.Close above.
		var damage *damagedFrameError
		if writeErr != nil && !errors.Is(writeErr, errRipgrepStoppedReading) && !errors.As(writeErr, &damage) {
			_ = pw.CloseWithError(writeErr)
		}
	}()

	if startErr := rgCmd.Start(); startErr != nil {
		// Nobody will read pr: closing it ends the writer's next write.
		_ = pr.Close()
		<-writerDone
		return nil, nil, nil, fmt.Errorf("scanFrameBatch: rg start: %w", startErr)
	}

	// Read rg's events as rg writes them. Each one is bounded (see
	// StreamEvents), so the batch holds about one bounded record per
	// matched or context line, never rg's whole output. They are placed
	// in the file after the writer is done, because only then is the
	// layout of the batch's input complete.
	//
	// The read runs on a context stripped of the cancellation: when a cap
	// cancels the scan, exec.CommandContext kills rg and its output ends
	// soon after, and every event rg wrote before that is still read. A
	// read stopped part-way would keep a match without the context lines
	// rg wrote after it, a window cut short.
	events, readErr := readBatchEvents(context.WithoutCancel(ctx), rgStdout)
	if readErr != nil {
		// INVARIANT: rg is never waited on while its stdout is unread. The
		// read stopped early, rg may still have output to write, and with
		// nobody reading it would block on the full pipe for ever.
		killRg()
	}
	runErr := rgCmd.Wait()

	// Close the read half before waiting for the writer.
	//
	// An io.Pipe write blocks until someone reads it, and the only
	// reader is the copy exec.Cmd runs into rg's stdin. When rg exits
	// early — killed because a max_results cap fired, because the
	// request was canceled, or because its output could not be read —
	// that copy stops, and a writer part-way through a frame would block
	// on pw.Write forever, taking the <-writerDone below with it.
	// Closing pr makes that Write return io.ErrClosedPipe instead, which
	// the writer treats as "the reader is gone" and exits. Wait has
	// already returned here, so exec is finished with pr and this cannot
	// race it.
	_ = pr.Close()

	// Wait for the writer to finish. After Wait returns and pr is
	// closed, writerDone closes promptly on every path. This barrier
	// establishes a happens-before edge for locs and the feeder's
	// layout below.
	<-writerDone

	// damage is set when the writer stopped at a damaged frame.
	var damage *damagedFrameError
	errors.As(writeErr, &damage)

	// A canceled context trumps every other reading of rg's exit.
	// exec.CommandContext kills rg on cancel, which arrives here as a
	// signal exit (code -1) — expected when a cap fired or the request
	// was abandoned, and not a reason to call the file unreadable. The
	// chunked path classifies it the same way. rg's output may end in
	// the middle of an event then, so a read error is expected too. A
	// damaged frame met before the cancel still travels up, so the file
	// is reported as not searched in full.
	if ctx.Err() != nil && (runErr != nil || writeErr != nil) {
		m, c, counted, cancelErr := matchesFromPartialBatch(events, feeder.stream(), locs, contextAfter)
		if damage != nil {
			cancelErr = errors.Join(cancelErr, damage)
		}
		return m, c, counted, cancelErr
	}
	if readErr != nil {
		// The output could not be parsed past some point: the batch fails
		// rather than report the matches before it as all. rg was killed
		// because of it, so its exit is a consequence, not the cause.
		return nil, nil, countedFrames(locs), fmt.Errorf("read rg output: %w", readErr)
	}
	if writeErr != nil && damage == nil && !errors.Is(writeErr, errRipgrepStoppedReading) {
		// The frames could not be read: rg got part of the batch, and
		// whatever it exited with describes that part only.
		return nil, nil, countedFrames(locs), fmt.Errorf("feed frames to rg: %w", writeErr)
	}
	if runErr != nil {
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

	matches, contexts = remapBatchEvents(events, feeder.stream())
	if damage != nil {
		return matches, contexts, countedFrames(locs), damage
	}
	return matches, contexts, countedFrames(locs), nil
}

// readBatchEvents reads rg's output for a batch and keeps its match and
// context events, in the order rg wrote them. An error comes back
// beside the events read before it.
func readBatchEvents(ctx context.Context, rgStdout io.Reader) ([]*RgEvent, error) {
	var events []*RgEvent
	err := StreamEvents(ctx, rgStdout, func(ev *RgEvent, parseErr error) error {
		if parseErr != nil {
			return parseErr
		}
		if ev != nil && (ev.Match != nil || ev.Context != nil) {
			events = append(events, ev)
		}
		return nil
	})
	return events, err
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

// matchesFromPartialBatch places whatever ripgrep managed to write
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
func matchesFromPartialBatch(
	events []*RgEvent,
	stream batchStream,
	locs []frameLoc,
	contextAfter int,
) ([]MatchRaw, []ContextRaw, []frameLines, error) {
	matches, contexts := remapBatchEvents(events, stream)
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

// remapBatchEvents places the match and context events rg wrote for a
// batch in the file: each line's offset in the decompressed text, its
// line number within its frame, and the frame that holds it. stream,
// recorded while the batch's input was written, says where each run of
// that input came from and which lines the batch owns; a match on a
// line it does not own comes back as a context line.
func remapBatchEvents(events []*RgEvent, stream batchStream) ([]MatchRaw, []ContextRaw) {
	var matches []MatchRaw
	var contexts []ContextRaw
	for _, ev := range events {
		switch {
		case ev.Match != nil:
			seg, ok := segmentHolding(stream.segments, ev.Match.AbsoluteOffset)
			if !ok || !stream.whole(ev.Match.AbsoluteOffset) {
				continue
			}
			line := rawMatchLine(ev.Match,
				seg.fileStart+ev.Match.AbsoluteOffset-seg.streamStart,
				ev.Match.LineNumber+seg.lineShift)
			line.IsCompressed = true
			line.FrameIndex = seg.frame.Index
			if !stream.owns(ev.Match.AbsoluteOffset) {
				// The batch beside this one owns the line and reports the
				// match; here it is a line of a window.
				contexts = append(contexts, matchAsContext(line))
				continue
			}
			matches = append(matches, line.withSubmatches(ev.Match))
		case ev.Context != nil:
			seg, ok := segmentHolding(stream.segments, ev.Context.AbsoluteOffset)
			if !ok || !stream.whole(ev.Context.AbsoluteOffset) {
				continue
			}
			contexts = append(contexts, rawContextLine(ev.Context,
				seg.fileStart+ev.Context.AbsoluteOffset-seg.streamStart,
				ev.Context.LineNumber+seg.lineShift))
		}
	}
	return matches, contexts
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
//
// A damaged frame (seekable.ErrDamagedFrame) among the batch's frames,
// or among the frames after them that the last line runs into, cuts
// the input after the last whole line written, and feedBatchLines
// returns a *damagedFrameError naming it. A damaged frame among those
// the lead-in needs costs the lead-in only: the batch writes none, and
// the batch that owns that frame reports it. A run that follows a
// damaged frame writes no lead-in either.
func feedBatchLines(
	ctx context.Context,
	feeder *batchFeeder,
	src batchSource,
	run frameRun,
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
	frameIdxs := run.frames
	var leadIn []textPiece
	skipping := frameIdxs[0] > 0
	withLeadIn := skipping && contextBefore > 0 && !run.followsDamage
	if !skipping {
		feeder.startOwned() // the first batch owns the text from byte 0
	} else if withLeadIn {
		var err error
		leadIn, err = linesBeforeFrame(ctx, dec, src.tbl, frameIdxs[0], contextBefore)
		switch {
		case errors.Is(err, seekable.ErrDamagedFrame):
			leadIn, withLeadIn = nil, false
		case err != nil:
			return err
		}
	}
	for i, fi := range frameIdxs {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame := src.tbl.Frames[fi]
		data, err := decompressFrameForBatch(dec, frame)
		if errors.Is(err, seekable.ErrDamagedFrame) {
			feeder.cut()
			return &damagedFrameError{frame: fi, err: err}
		}
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
			if withLeadIn {
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
		text := &frameStream{
			zd:         zd,
			compressed: &readErrorRecorder{r: io.NewSectionReader(src.f, frame.CompressedOffset, frame.CompressedSize)},
			frame:      frame,
		}
		err = text.reset()
		if err == nil {
			// No line break of this frame comes before its first byte.
			feeder.beginRun(frame, frame.DecompressedOffset, 0)
			breaks, err = feedThroughLineBreak(ctx, feeder, text, buf, breaks)
		}
		if errors.Is(err, seekable.ErrDamagedFrame) {
			feeder.cut()
			return &damagedFrameError{frame: fi, err: err}
		}
		if err != nil {
			return err
		}
	}
	return nil // the lines asked for are written, or the text has ended
}

// frameStream reads one frame's text through a stream decoder and tells
// a damaged frame from a file that could not be read: an error from the
// decoder wraps seekable.ErrDamagedFrame, unless reading the frame's
// compressed bytes failed, and then it is that read's error.
type frameStream struct {
	zd         *zstd.Decoder
	compressed *readErrorRecorder
	frame      seekable.FrameInfo
}

// reset points the decoder at the frame's compressed bytes.
func (s *frameStream) reset() error {
	return s.classify(s.zd.Reset(s.compressed))
}

// Read reads the frame's text. Go note: having this method makes a
// *frameStream an io.Reader, which is what feedThroughLineBreak takes.
func (s *frameStream) Read(p []byte) (int, error) {
	n, err := s.zd.Read(p)
	if errors.Is(err, io.EOF) {
		return n, err
	}
	return n, s.classify(err)
}

// classify turns an error of the decoder into the frame's error.
func (s *frameStream) classify(err error) error {
	switch {
	case err == nil:
		return nil
	case s.compressed.err != nil:
		return fmt.Errorf("read frame at %d: %w", s.frame.CompressedOffset, s.compressed.err)
	default:
		return fmt.Errorf("%w: frame %d: %w", seekable.ErrDamagedFrame, s.frame.Index, err)
	}
}

// readErrorRecorder passes reads through to r and keeps the first error
// other than io.EOF that r returned. The stream decoder reads it with
// concurrency 1, so on the caller's goroutine, and the field needs no
// lock.
type readErrorRecorder struct {
	r   io.Reader
	err error
}

func (rr *readErrorRecorder) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && rr.err == nil {
		rr.err = err
	}
	return n, err
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

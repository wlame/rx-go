package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx-go/internal/compression"
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
// Batching is SAFE only when frames are line-aligned (each frame ends
// with '\n'). If frames split lines across boundaries, batching would
// produce mangled line numbers across frame joins. rx-go's own encoder
// emits line-aligned frames; files produced by t2sz --no-newline-align
// are detected at runtime and fall back to batch=1.
const framesPerBatch = 100

// frameLoc captures where a frame's decompressed bytes land in a
// concatenated batch buffer, plus metadata needed to translate rg
// batch-local line numbers back to file-absolute line numbers.
type frameLoc struct {
	frameIdx   int
	batchStart int64 // offset in concat buffer where this frame starts
	info       seekable.FrameInfo
	lineCount  int // number of '\n' bytes in this frame's decompressed data
}

// readSeekTable is a small wrapper around seekable.ReadSeekTable that
// handles the file-open + size-lookup for callers that only have a
// path string. The seekable package's API takes an io.ReaderAt for
// test-friendliness; we'd rather not repeat the boilerplate here.
func readSeekTable(path string) (*seekable.SeekTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("seekable: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("seekable: stat %s: %w", path, err)
	}
	return seekable.ReadSeekTable(f, fi.Size())
}

// ProcessSeekable runs the parallel frame scan over a seekable-zstd
// file. Each batch of frames is decompressed and piped through rg
// --json in its own goroutine, bounded by workerLimit().
//
// Line numbers in the returned MatchRaw are CURRENTLY batch-local
// (1-indexed against the concatenated batch, not the full file). That
// matches Python's behavior when the unified index for the file
// hasn't been built yet. Once M3's unified-index builder lands, the
// engine post-processes these into file-absolute line numbers using
// the frame first-line table.
//
// Offsets in MatchRaw.Offset are absolute byte offsets in the
// decompressed stream — same contract as Python.
//
// If maxResults is non-nil and the post-sort result set exceeds it,
// the tail is truncated.
func ProcessSeekable(
	ctx context.Context,
	path string,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
	maxResults *int,
) (matches []MatchRaw, contexts []ContextRaw, elapsed time.Duration, err error) {
	start := time.Now()

	tbl, err := readSeekTable(path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("ProcessSeekable: %w", err)
	}
	if tbl.NumFrames == 0 {
		return nil, nil, time.Since(start), nil
	}

	// Detect line-alignment by decompressing frame 0. A line-aligned
	// file ends every frame with '\n'.
	dec := seekable.NewDecoder()
	firstFrame, err := dec.DecompressFrame(path, 0, tbl)
	if err != nil {
		return nil, nil, time.Since(start), fmt.Errorf("ProcessSeekable: decompress frame 0: %w", err)
	}
	batchSize := framesPerBatch
	if len(firstFrame) == 0 || firstFrame[len(firstFrame)-1] != '\n' {
		batchSize = 1
	}

	// Build the list of frame-index batches.
	var batches [][]int
	for i := 0; i < tbl.NumFrames; i += batchSize {
		end := i + batchSize
		if end > tbl.NumFrames {
			end = tbl.NumFrames
		}
		idxs := make([]int, 0, end-i)
		for j := i; j < end; j++ {
			idxs = append(idxs, j)
		}
		batches = append(batches, idxs)
	}

	batchMatches := make([][]MatchRaw, len(batches))
	batchContexts := make([][]ContextRaw, len(batches))

	workers := workerLimit()
	// R5-B3: cooperative cancel on max_results cap. Same pattern as
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
			m, c, berr := scanFrameBatch(
				gctx, path, tbl, frameIdxs,
				patternIDs, patternOrder, rgExtraArgs,
				contextBefore, contextAfter,
			)
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

	// Stabilize order across parallel batches (Python parity).
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].LineNumber < matches[j].LineNumber
	})
	sort.SliceStable(contexts, func(i, j int) bool {
		return contexts[i].LineNumber < contexts[j].LineNumber
	})

	// Final hard cap — the cooperative cancel may overshoot (a batch
	// that started before cancel can still produce matches past the
	// cap). Truncate to the exact cap for the caller's contract.
	if maxResults != nil && len(matches) > *maxResults {
		matches = matches[:*maxResults]
	}

	return matches, contexts, time.Since(start), nil
}

// frameDecoder wraps an open *os.File and a pooled zstd decoder for
// the writer goroutine in scanFrameBatch. It is intentionally small
// and package-private — the only callers are the streaming pipe
// writer and the decompressFrameForBatch test seam.
//
// Go note: the pooled decoder (from compression.AcquireDecoder, Task 1)
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

// scanFrameBatch decompresses a batch of consecutive frames and
// streams them through an io.Pipe to rg, avoiding materialization of
// the whole batch in memory. The writer goroutine decompresses one
// frame at a time and writes it to the pipe; rg reads from the other
// end concurrently. Memory footprint is O(1) frame in flight
// (occasionally 2 due to pipe buffering) instead of O(N × frame_size).
//
// Match line numbers are remapped from batch-local to frame-local
// (via linesBefore in the frameLoc table). Match byte offsets are
// remapped from pipe-cumulative to absolute decompressed file offsets
// via binary search. Both remaps use the pre-computed locs slice.
//
// Uses compression.AcquireDecoder / ReleaseDecoder (Task 1) to avoid
// the ~2 MB per-frame decoding-table allocation — one decoder per
// batch is reused across all frames in the batch via stateless
// DecodeAll calls.
//
// Parity: rx-python/src/rx/trace_compressed.py::process_seekable_zstd_frame_batch
// and another-rx-go/internal/engine/compressed.go:315-395 (the
// reference io.Pipe implementation we ported from).
func scanFrameBatch(
	ctx context.Context,
	path string,
	tbl *seekable.SeekTable,
	frameIdxs []int,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
) (matches []MatchRaw, contexts []ContextRaw, err error) {
	// Stage 9 Round 2 S6: gated helpers — CLI mode skips collection.
	prometheus.IncActiveWorkers()
	defer prometheus.DecActiveWorkers()
	defer func() {
		if err == nil {
			prometheus.IncWorkerTasksCompleted()
		} else {
			prometheus.IncWorkerTasksFailed()
		}
	}()

	// Open the file once per batch — reused by the writer goroutine
	// for every frame's ReadAt. os.File.ReadAt is safe for concurrent
	// use (pread(2) under the hood on Linux/macOS), though we only use
	// it single-threaded in the writer goroutine here.
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("scanFrameBatch: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Pre-compute the frame location table. Each locs[i] records:
	//   - batchStart: cumulative bytes of THIS frame's start in the
	//     pipe stream (0 for the first frame, sum of prior
	//     DecompressedSize for subsequent frames).
	//   - info: the FrameInfo, so DecompressedOffset is the file-
	//     absolute offset anchor for this frame.
	//   - lineCount: '\n' count in the frame's decompressed bytes.
	//     UNKNOWN at this point (we haven't decompressed yet). Filled
	//     lazily by the writer goroutine as it processes each frame.
	//
	// We size locs up front so the writer's index-into-locs writes
	// don't race with the later remap read — but remap only happens
	// after the errgroup has Wait()ed, establishing a happens-before
	// edge. See the "Wait()" barrier below.
	locs := make([]frameLoc, len(frameIdxs))
	var runningStart int64
	for i, fi := range frameIdxs {
		locs[i] = frameLoc{
			frameIdx:   fi,
			batchStart: runningStart,
			info:       tbl.Frames[fi],
			// lineCount filled by writer goroutine
		}
		runningStart += tbl.Frames[fi].DecompressedSize
	}

	// Build rg argv (same as ProcessChunk).
	rgArgs := []string{"--json", "--no-heading", "--color=never"}
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

	// Writer goroutine: decompress frames sequentially into the pipe.
	// Must ALWAYS close pw so rg sees EOF and exits. pw.CloseWithError
	// propagates any decompression failure to the reader side, where
	// rg will exit cleanly with whatever data it already received.
	//
	// We launch this BEFORE rgCmd.Start() because exec.Cmd with
	// Stdin=*io.PipeReader works as long as the reader stays open
	// until rg has consumed its stdin; starting writer first keeps
	// the semantics symmetric with the other-rx-go reference.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { _ = pw.Close() }()

		// Acquire one decoder per batch; release at exit. Reusing it
		// across all frames in the batch is the main win of Task 1.
		zd := compression.AcquireDecoder()
		defer compression.ReleaseDecoder(zd)

		dec := &frameDecoder{f: f, zd: zd}
		for i, fi := range frameIdxs {
			if cerr := ctx.Err(); cerr != nil {
				_ = pw.CloseWithError(cerr)
				return
			}
			data, derr := decompressFrameForBatch(dec, tbl.Frames[fi])
			if derr != nil {
				_ = pw.CloseWithError(derr)
				return
			}
			// Record '\n' count now that we have the decompressed
			// bytes. Safe because the main goroutine only reads
			// locs AFTER rgCmd.Wait() returns (happens-before edge
			// established by reading from writerDone below).
			locs[i].lineCount = bytesCountByte(data, '\n')
			if _, werr := pw.Write(data); werr != nil {
				// Reader side closed — rg exited early (e.g. ctx
				// cancel or cap-cancel). Stop silently; no error
				// to propagate, rg's Wait will surface the cancel.
				return
			}
		}
	}()

	// Run rg. Cmd.Run waits for rg to exit AFTER its stdin (pr) hits
	// EOF, which happens when the writer goroutine calls pw.Close().
	// If rg exits early (e.g. killed by ctx cancel via
	// exec.CommandContext), pr.Read returns ErrClosedPipe inside the
	// writer's pw.Write, and the writer aborts cleanly.
	runErr := rgCmd.Run()

	// Wait for the writer to finish. After Run returns, writerDone
	// closes very quickly (either the writer already finished and
	// closed pw, or rg's exit caused pw.Write to error out — both
	// paths terminate the goroutine). This barrier establishes a
	// happens-before edge for locs[i].lineCount reads below.
	<-writerDone

	if runErr != nil {
		var ex *exec.ExitError
		if errors.As(runErr, &ex) {
			code := ex.ExitCode()
			if code != 0 && code != 1 {
				return nil, nil, fmt.Errorf("rg exit %d: %s",
					code, strings.TrimSpace(stderr.String()))
			}
		} else if !errors.Is(runErr, context.Canceled) {
			return nil, nil, fmt.Errorf("rg run: %w", runErr)
		}
	}

	// Parse rg's stdout (buffered — we already have it all) and remap.
	// Thread the parent ctx so a cancellation during parsing (e.g. the
	// outer errgroup was canceled because a sibling batch failed) can
	// abort the StreamEvents loop. See Stage 8 Reviewer 2 High #9.
	matches, contexts = remapBatchEvents(ctx, stdout.Bytes(), locs, patternOrder)
	return matches, contexts, nil
}

// remapBatchEvents parses the rg --json stream emitted for a batch of
// concatenated frames and translates each event's absolute_offset into
// the file's decompressed coordinate system.
//
// ctx propagates from the parent scanner — if the outer context is
// canceled mid-parse (e.g. an errgroup sibling failed, or the HTTP
// request was aborted), StreamEvents will stop calling the callback
// and return promptly. Prior to Stage 8 this used context.Background()
// which meant cancellation signals never reached here; the buffer is
// small and finite so no hangs were observed, but plumbing the parent
// ctx is the correct idiom.
func remapBatchEvents(
	ctx context.Context,
	rgStdout []byte,
	locs []frameLoc,
	patternOrder []string,
) ([]MatchRaw, []ContextRaw) {
	var matches []MatchRaw
	var contexts []ContextRaw
	_ = StreamEvents(ctx, bytes.NewReader(rgStdout), func(ev *RgEvent, parseErr error) error {
		if parseErr != nil || ev == nil {
			return nil
		}
		switch ev.Type {
		case RgEventMatch:
			if ev.Match == nil {
				return nil
			}
			info, offInFrame, linesBefore, ok := locateInBatch(locs, ev.Match.AbsoluteOffset)
			if !ok {
				return nil
			}
			absOff := info.DecompressedOffset + offInFrame
			// rg's line_number is 1-based within the batch.
			// Batch-local line N corresponds to (lines_in_previous_frames + intra-frame line).
			// Until first_line is wired, we report the batch-local number;
			// the engine will refine these when the unified index is
			// available for the file.
			absLine := ev.Match.LineNumber - linesBefore
			if absLine < 1 {
				absLine = ev.Match.LineNumber
			}
			subs := make([]rxtypes.Submatch, len(ev.Match.Submatches))
			for i, sm := range ev.Match.Submatches {
				subs[i] = rxtypes.Submatch{Text: sm.Text(), Start: sm.Start, End: sm.End}
			}
			matches = append(matches, MatchRaw{
				Offset:       absOff,
				LineNumber:   absLine,
				LineText:     trimTrailingNewline(ev.Match.Lines.Text),
				Submatches:   subs,
				PatternIDs:   append([]string(nil), patternOrder...),
				IsCompressed: true,
			})
		case RgEventContext:
			if ev.Context == nil {
				return nil
			}
			info, offInFrame, linesBefore, ok := locateInBatch(locs, ev.Context.AbsoluteOffset)
			if !ok {
				return nil
			}
			absOff := info.DecompressedOffset + offInFrame
			absLine := ev.Context.LineNumber - linesBefore
			if absLine < 1 {
				absLine = ev.Context.LineNumber
			}
			contexts = append(contexts, ContextRaw{
				Offset:     absOff,
				LineNumber: absLine,
				LineText:   trimTrailingNewline(ev.Context.Lines.Text),
			})
		}
		return nil
	})
	return matches, contexts
}

// locateInBatch maps a batch-local offset to (frameInfo, offset-within-frame,
// lines-before-this-frame). Returns ok=false if the offset is past the
// last frame (shouldn't normally happen unless rg invents numbers).
func locateInBatch(locs []frameLoc, batchOffset int64) (seekable.FrameInfo, int64, int, bool) {
	var running int64
	linesBefore := 0
	for _, l := range locs {
		next := running + l.info.DecompressedSize
		if batchOffset < next {
			return l.info, batchOffset - running, linesBefore, true
		}
		running = next
		linesBefore += l.lineCount
	}
	return seekable.FrameInfo{}, 0, 0, false
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

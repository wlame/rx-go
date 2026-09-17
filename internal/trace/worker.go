package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Hook firer interface
// ============================================================================

// FileInfo is the bundle of per-file scan metadata passed to HookFirer.
// Kept separate from rxtypes.FileScannedPayload so the trace package
// doesn't know about JSON shape — that concern lives in internal/hooks.
type FileInfo struct {
	FileSizeBytes int64
	ScanTimeMS    int
	MatchesCount  int
}

// MatchInfo is the per-match bundle passed to HookFirer.OnMatch.
// Separate from rxtypes.Match so the hook package can format payloads
// without importing the full trace.Match shape.
type MatchInfo struct {
	Pattern string
	// Offset is the byte offset of the matched line in the file's text.
	Offset int64
	// LineNumber is the line's 1-based number in the file, or -1 when
	// the scan could not count it; never a chunk-relative number.
	LineNumber int64
}

// HookFirer is the interface the trace engine uses to notify webhooks
// as matches/files complete. It's intentionally abstracted so the
// trace package can be tested without any HTTP stack in scope.
//
// The webhook implementation is hooks.Dispatcher in internal/hooks.
// The default is NoopHookFirer, which is also what CLI `rx trace` uses
// when no hook is configured (no RX_HOOK_* env vars set).
//
// Implementations are fire-and-forget: a single POST per event, a 3 s
// timeout and no retries. The engine does NOT
// block on Hook calls; implementations must enqueue work on a channel
// or spawn a goroutine before returning.
type HookFirer interface {
	// OnFile is called once per file after its scan completes.
	OnFile(ctx context.Context, path string, info FileInfo)
	// OnMatch is called once per match of the result, after the scan,
	// in the result's order, with the result's absolute line number (-1
	// where it is unknown). It can be called many times in a row;
	// implementations that cannot buffer should drop events rather
	// than block the engine.
	OnMatch(ctx context.Context, path string, match MatchInfo)
}

// NoopHookFirer is the zero-value / default implementation. It drops
// every event silently. Used by tests and by the CLI when no hooks
// are configured.
type NoopHookFirer struct{}

// OnFile implements HookFirer.
func (NoopHookFirer) OnFile(context.Context, string, FileInfo) {}

// OnMatch implements HookFirer.
func (NoopHookFirer) OnMatch(context.Context, string, MatchInfo) {}

// ============================================================================
// Chunk processing — the hot loop
// ============================================================================

// MatchRaw is the worker's intermediate result. One entry per matched
// line per chunk — before the engine applies pattern-identification to
// produce final rxtypes.Match records.
//
// AbsoluteOffset is the BYTE offset of the matched line's first byte
// in the ORIGINAL file (not in the chunk's substream). The worker
// computes this by adding task.Offset to rg's reported chunk-local
// absolute_offset, then filters out matches that lie outside the
// task's designated range to avoid duplicate hits at chunk boundaries.
// The filter is a single per-match range-containment check at the
// accept point, with no cross-worker coordination and no post-merge
// dedup pass.
type MatchRaw struct {
	Offset int64
	// End is where the line ends in the file's text: the offset one past
	// its last byte, its line break included, which is where the next
	// line starts. Context windows link lines through it.
	End        int64
	LineNumber int
	// AbsoluteLine is the line's number in the whole file when the
	// scanner knew it, and 0 when it did not. The chunked path leaves
	// it 0 and the engine derives the number from the per-chunk
	// newline counts; the seekable path fills it in from the per-frame
	// counts, which are the only way to place a frame in the file.
	AbsoluteLine int
	LineText     string
	Submatches   []rxtypes.Submatch
	PatternIDs   []string // all pattern IDs (assigned by engine post-hoc)
	IsCompressed bool
	// FrameIndex is the seekable-zstd frame that holds the match. Only
	// the seekable path sets it; elsewhere it is 0 and means nothing.
	FrameIndex int
}

// ContextRaw mirrors MatchRaw for context lines.
type ContextRaw struct {
	Offset       int64
	End          int64 // see MatchRaw.End
	LineNumber   int
	AbsoluteLine int // see MatchRaw.AbsoluteLine; 0 when unknown
	LineText     string
}

// matchAsContext reports a match as a line of the windows around it.
//
// A result cap can leave a scan holding matches it does not report: a
// match the cap cut, or one whose own window the scan stopped before
// reading. Each is still a line of an earlier match's window, so it is
// kept as a context line, with the same offset, numbers and text a
// trace without the cap gives that line.
func matchAsContext(m MatchRaw) ContextRaw {
	return ContextRaw{
		Offset:       m.Offset,
		End:          m.End,
		LineNumber:   m.LineNumber,
		AbsoluteLine: m.AbsoluteLine,
		LineText:     m.LineText,
	}
}

// matchesAsContext moves the last n matches into contexts, as context
// lines, and returns both slices.
func matchesAsContext(matches []MatchRaw, contexts []ContextRaw, n int) ([]MatchRaw, []ContextRaw) {
	keep := len(matches) - n
	for _, m := range matches[keep:] {
		contexts = append(contexts, matchAsContext(m))
	}
	return matches[:keep], contexts
}

// recordWorkerOutcome counts one finished worker task (a chunk, a
// compressed stream or a batch of seekable frames) by the error it
// returned: completed when there was none, failed when there was one,
// and neither when it was context.Canceled. A cancel is how a
// max_results cap, or a request abandoned by its caller, stops the work
// still running; nothing failed. The helpers are no-ops in CLI mode.
func recordWorkerOutcome(err error) {
	switch {
	case err == nil:
		prometheus.IncWorkerTasksCompleted()
	case errors.Is(err, context.Canceled):
		// Stopped on purpose: counted as neither.
	default:
		prometheus.IncWorkerTasksFailed()
	}
}

// ChunkRequest is one unit of work for ProcessChunk.
type ChunkRequest struct {
	Task          FileTask
	PatternIDs    map[string]string
	PatternOrder  []string
	RgExtraArgs   []string
	ContextBefore int
	ContextAfter  int
	// Budget is the shared match cap for the whole scan, or nil when
	// the caller set no cap. See MatchBudget.
	Budget *MatchBudget
}

// ChunkResult is what one chunk scan produces.
type ChunkResult struct {
	Matches  []MatchRaw
	Contexts []ContextRaw
	// Newlines counts the '\n' bytes in the chunk. Chunks are
	// newline-aligned, so summing this over the chunks before a chunk
	// gives the number of lines before it — which is what turns
	// ripgrep's chunk-relative line numbers into file-absolute ones
	// without a second pass over the file.
	Newlines int64
	// Complete is true when the whole chunk reached ripgrep. A chunk
	// canceled part-way through (a cap fired, the request was
	// aborted) stops early, so its Newlines is a partial count and the
	// chunks after it cannot be numbered from it.
	Complete bool
	Elapsed  time.Duration
}

// ProcessChunk runs `rg --json` over a single chunk of a file, feeding
// the chunk bytes in via stdin using os.File.ReadAt (native chunking,
// no `dd` subprocess). It parses the rg event stream,
// filters out matches that don't belong to this chunk's range, and
// returns the rich MatchRaw + ContextRaw slices.
//
// Deduplication rule (the same rule rx-python applies):
//
//	a match at absolute offset O is KEPT iff
//	task.Offset <= O < task.Offset + task.Count.
//
// This is a single per-match check at the accept point — no
// cross-worker coordination, no post-merge dedup pass. Correctness
// depends on the chunker producing a PARTITION of the file (adjacent,
// newline-aligned, non-overlapping) — which CreateFileTasks in
// chunker.go guarantees. If the chunker ever produced overlapping
// tasks, this filter would NOT dedup them (see
// worker_boundary_test.go::OverlappingChunksExposeDependency) —
// that would be a chunker bug, not something to patch at merge time.
//
// elapsed is time.Since(start) measured around the whole chunk pipeline.
func ProcessChunk(ctx context.Context, req ChunkRequest) (res ChunkResult, err error) {
	// Locals for the fields the pipeline below reads repeatedly.
	task := req.Task
	patternIDs, patternOrder := req.PatternIDs, req.PatternOrder
	rgExtraArgs := req.RgExtraArgs
	contextBefore, contextAfter := req.ContextBefore, req.ContextAfter

	start := time.Now()
	// gate all metric updates behind the package
	// enabled switch — CLI-mode callers pay no cost here.
	prometheus.IncActiveWorkers()
	defer prometheus.DecActiveWorkers()
	defer func() { recordWorkerOutcome(err) }()

	// Build the rg argv. We filter out args that would change the
	// output shape (--byte-offset, --only-matching) — Python does
	// the same in trace_worker.py.
	rgArgs := newRgArgs()
	if contextBefore > 0 {
		rgArgs = append(rgArgs, "-B", strconv.Itoa(contextBefore))
	}
	if contextAfter > 0 {
		rgArgs = append(rgArgs, "-A", strconv.Itoa(contextAfter))
	}
	// Emit patterns in the explicit order supplied (p1, p2, ...).
	// Using `patternIDs` iteration would break because Go maps randomize.
	for _, pid := range patternOrder {
		rgArgs = append(rgArgs, "-e", patternIDs[pid])
	}
	rgArgs = append(rgArgs, filterIncompatibleRgArgs(rgExtraArgs)...)
	rgArgs = append(rgArgs, "-") // read from stdin

	// Open the source file. ReadAt is goroutine-safe and doesn't move
	// a shared cursor, so we can stream in one goroutine while parsing
	// events in another.
	src, err := os.Open(task.FilePath)
	if err != nil {
		return ChunkResult{Elapsed: time.Since(start)}, fmt.Errorf("ProcessChunk: open %s: %w", task.FilePath, err)
	}
	defer func() { _ = src.Close() }()

	// ripgrep only sees the bytes it is given, so a match near the start
	// of the chunk would lose the lines before it that lie in the chunk
	// before. The chunk's input therefore starts contextBefore lines
	// early, and ends contextAfter lines late (see feedChunk). Those
	// lines belong to the neighboring chunks: they serve as context
	// here and are never reported as this chunk's matches.
	leadIn, err := readLinesBefore(src, task.Offset, contextBefore)
	if err != nil {
		return ChunkResult{Elapsed: time.Since(start)}, fmt.Errorf("ProcessChunk: read the lines before the chunk: %w", err)
	}
	input := chunkInput{
		task:      task,
		leadIn:    leadIn,
		leadStart: task.Offset - int64(len(leadIn)),
		// ripgrep numbers the lines of its own input, the lead-in's
		// first. Discounting the lead-in's lines gives the chunk-relative
		// number, which is 1 for the chunk's first line and 0 or below
		// for the lines before it.
		leadLines: bytes.Count(leadIn, newlineBytes),
	}

	// rg runs under its own context so the pipeline can kill it when one
	// of its goroutines fails, without canceling the caller's context.
	// exec.CommandContext kills the process when that context ends; the
	// deferred cancel releases the context's resources on every return.
	rgCtx, killRg := context.WithCancel(ctx)
	defer killRg()
	rgCmd := exec.CommandContext(rgCtx, "rg", rgArgs...)
	// Separate the subprocess cost from the rest of the request, so a
	// slow scan can be told apart from slow bookkeeping around it.
	rgStart := time.Now()
	defer func() { prometheus.RecordRipgrepProcessing(time.Since(rgStart)) }()

	// stdin pipe carries the chunk bytes from our ReadAt loop into rg.
	rgStdin, err := rgCmd.StdinPipe()
	if err != nil {
		return ChunkResult{Elapsed: time.Since(start)}, fmt.Errorf("ProcessChunk: rg stdin pipe: %w", err)
	}
	// stdout is the rg --json event stream we parse.
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return ChunkResult{Elapsed: time.Since(start)}, fmt.Errorf("ProcessChunk: rg stdout pipe: %w", err)
	}
	// stderr captured to a buffered string — surfaces rg errors
	// (invalid regex, etc.) when the worker returns an error.
	var stderrBuf strings.Builder
	rgCmd.Stderr = &stderrBuf

	if startErr := rgCmd.Start(); startErr != nil {
		return ChunkResult{Elapsed: time.Since(start)}, fmt.Errorf("ProcessChunk: rg start: %w", startErr)
	}

	// errgroup here lets us surface stdin-copy errors alongside the
	// event-parse errors. Both must finish before we Wait() on rg.
	//
	// INVARIANT: rg is never waited on while its stdout is unread. When
	// either goroutine fails, errgroup cancels gctx and the other one
	// stops too: the parser stops reading rg's stdout. rg may still have
	// output to write, and with nobody reading it would block on the
	// full pipe for ever, so stopOnFailure kills it. Killing rg also
	// unblocks a feeder stuck writing to rg's stdin (the write fails
	// with a broken pipe).
	g, gctx := errgroup.WithContext(ctx)
	stopOnFailure := func(step func() error) func() error {
		return func() error {
			err := step()
			if err != nil {
				killRg()
			}
			return err
		}
	}

	// Goroutine 1: pump the chunk's input into rg stdin, counting the
	// chunk's own newlines on the way past. The count costs nothing
	// extra — these bytes are already in hand — and it is what lets the
	// engine report absolute line numbers for a chunked file without
	// reading it twice.
	//
	// fed is written here and read after g.Wait() below, which is the
	// happens-before edge that makes a plain variable safe.
	var fed chunkFeed
	g.Go(stopOnFailure(func() error {
		defer func() { _ = rgStdin.Close() }()
		var feedErr error
		fed, feedErr = feedChunk(src, input, contextAfter, rgStdin)
		// A canceled scan stops on purpose, and rg exiting early
		// surfaces as a broken pipe. Both are a clean shutdown, not a
		// failure.
		if feedErr == nil || gctx.Err() != nil {
			return nil
		}
		if errors.Is(feedErr, errRipgrepStoppedReading) && isBrokenPipe(feedErr) {
			return nil
		}
		return fmt.Errorf("stdin copy: %w", feedErr)
	}))

	// Goroutine 2: parse rg --json events and collect matches.
	var mu struct {
		matches  []MatchRaw
		contexts []ContextRaw
	}
	// The gate charges the shared cap for a match once rg has written
	// the trailing context of that match. Only goroutine 2 touches it
	// until g.Wait() returns below.
	gate := newTrailingWindowGate(req.Budget, contextAfter)
	g.Go(stopOnFailure(func() error {
		return StreamEvents(gctx, rgStdout, func(ev *RgEvent, parseErr error) error {
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
				// rg's absolute_offset is relative to its OWN input,
				// which starts with the lead-in. input.fileOffset turns
				// it into a position in the file.
				absOff := input.fileOffset(ev.Match.AbsoluteOffset)
				line := MatchRaw{
					Offset:     absOff,
					End:        absOff + int64(ev.Match.Lines.Size),
					LineNumber: input.chunkLine(ev.Match.LineNumber),
					LineText:   trimTrailingNewline(ev.Match.Lines.Text),
				}
				// Dedup filter: only keep matches whose absolute start
				// offset falls within THIS chunk's assigned half-open
				// range [task.Offset, task.EndOffset()). A match in the
				// lead-in or the tail belongs to the chunk beside this
				// one, which reports it; here it is only a line of the
				// windows around this chunk's matches.
				//
				// This invariant is local to each worker — a duplicate
				// match would be a chunker bug, not something this
				// filter needs to patch at merge time. rx-python
				// applies the same rule.
				if !input.owns(absOff) {
					mu.contexts = append(mu.contexts, matchAsContext(line))
					gate.reached(ev.Match.LineNumber)
					return nil
				}
				subs := make([]rxtypes.Submatch, len(ev.Match.Submatches))
				for i, sm := range ev.Match.Submatches {
					subs[i] = rxtypes.Submatch{
						Text:  sm.Text(),
						Start: sm.Start,
						End:   sm.End,
					}
				}
				line.Submatches = subs
				// pattern IDs are the FULL set — engine.identify
				// narrows this down post-hoc per Python parity.
				line.PatternIDs = append([]string(nil), patternOrder...)
				mu.matches = append(mu.matches, line)
				// Charge the shared cap as soon as the match's window
				// is read (at once without -A), so the worker that
				// spends the last of it stops every sibling within a
				// few lines instead of at the end of its chunk.
				gate.matched(ev.Match.LineNumber)
			case RgEventContext:
				if ev.Context == nil {
					return nil
				}
				gate.reached(ev.Context.LineNumber)
				// A context line is kept wherever it lies: one in the
				// lead-in or the tail is what completes the window of a
				// match next to the chunk's edge.
				absOff := input.fileOffset(ev.Context.AbsoluteOffset)
				mu.contexts = append(mu.contexts, ContextRaw{
					Offset:     absOff,
					End:        absOff + int64(ev.Context.Lines.Size),
					LineNumber: input.chunkLine(ev.Context.LineNumber),
					LineText:   trimTrailingNewline(ev.Context.Lines.Text),
				})
			default:
				// begin/end/summary — ignored for per-chunk processing.
			}
			return nil
		})
	}))

	// Wait for both goroutines, THEN Wait on rg. By the time g.Wait
	// returns, either the parser read rg's stdout to the end (rg has
	// finished writing and is exiting) or a goroutine failed and
	// stopOnFailure killed rg, so rgCmd.Wait cannot block on a pipe
	// nobody reads.
	groupErr := g.Wait()
	waitErr := rgCmd.Wait()

	// A run that a cancel stopped (the cap, or the caller) may hold
	// matches whose windows it never finished reading; they were not
	// counted, so they are reported as lines of the windows around them.
	// A run that read its whole input finishes every window it can, and
	// those matches count now.
	if ctx.Err() != nil {
		mu.matches, mu.contexts = matchesAsContext(mu.matches, mu.contexts, gate.uncounted())
	} else {
		gate.inputEnded()
	}

	elapsed := time.Since(start)
	result := ChunkResult{
		Matches:  mu.matches,
		Contexts: mu.contexts,
		Newlines: fed.newlines,
		Complete: fed.copied == task.Count,
		Elapsed:  elapsed,
	}

	outcomeErr := classifyChunkOutcome(ctx, chunkOutcome{
		groupErr:   groupErr,
		waitErr:    waitErr,
		stderr:     strings.TrimSpace(stderrBuf.String()),
		patternIDs: patternIDs,
		order:      patternOrder,
	})
	// A canceled run keeps what it found (ProcessAllChunks reports it);
	// a failed one reports nothing, since its matches cannot be trusted
	// to be all there are.
	if outcomeErr != nil && !errors.Is(outcomeErr, context.Canceled) {
		return ChunkResult{Elapsed: elapsed}, outcomeErr
	}
	return result, outcomeErr
}

// chunkOutcome is what ProcessChunk knows once rg and both of its
// goroutines have finished: the first goroutine failure, rg's exit, and
// what rg wrote on stderr.
type chunkOutcome struct {
	groupErr   error
	waitErr    error
	stderr     string
	patternIDs map[string]string
	order      []string
}

// classifyChunkOutcome turns a finished chunk run into ProcessChunk's
// error, nil when the run succeeded. The checks go from the cause that
// explains the most to the least:
//
//  1. The caller's context ended (a max_results cap, or the request
//     ended): the context's error, whatever rg's exit says, because rg
//     was killed on purpose. ProcessAllChunks swallows context.Canceled.
//  2. rg failed on its own (exit 2 and up): a pattern it cannot compile
//     dooms the whole request and gets its own error; any other is
//     "rg exit N".
//  3. A goroutine failed (rg's output could not be parsed, or the input
//     could not be read): its error. stopOnFailure killed rg because
//     of it, so rg's "killed" exit is a consequence, not the cause.
//  4. Any other abnormal exit of rg, such as a kill from outside.
//
// rg's exit 1 means "no match" and is a success.
func classifyChunkOutcome(ctx context.Context, out chunkOutcome) error {
	if out.waitErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	isExitStatus := errors.As(out.waitErr, &exitErr)
	// ExitCode is -1 when a signal ended rg; that is not rg's own failure.
	if isExitStatus && exitErr.ExitCode() > 1 {
		if isRegexParseError(out.stderr) {
			return invalidPatternError(out.stderr, out.patternIDs, out.order)
		}
		return fmt.Errorf("rg exit %d: %s", exitErr.ExitCode(), out.stderr)
	}
	if out.groupErr != nil && !errors.Is(out.groupErr, context.Canceled) {
		return out.groupErr
	}
	switch {
	case out.waitErr == nil:
		return nil
	case isExitStatus && exitErr.ExitCode() < 0:
		return fmt.Errorf("rg exit %d: %s", exitErr.ExitCode(), out.stderr)
	case isExitStatus, errors.Is(out.waitErr, context.Canceled):
		// Exit 0 or 1, or a cancel already accounted for.
		return nil
	default:
		return fmt.Errorf("rg wait: %w", out.waitErr)
	}
}

// ProcessAllChunks runs ProcessChunk over every task in parallel,
// bounded by RX_WORKERS (falling back to runtime.NumCPU()).
//
// Returns the raw per-chunk results grouped by task.TaskID (the slice
// position matches the input task slice position). Callers merge these
// into the final response shape.
//
// Cancellation semantics: if the context is canceled or one chunk
// returns an error, all sibling chunks receive a cancellation via the
// errgroup and abandon their rg subprocesses. Partial results from the
// completed chunks are still returned (via the pointer-slice layout)
// alongside the first error surfaced.
//
// # Bounded-read contract
//
// When maxResults is non-nil, this function COOPERATIVELY CANCELS the
// remaining chunks as soon as the total match count from already-finished
// chunks reaches the cap. Mechanism:
//
//  1. After each g.Go worker finishes, it publishes its per-chunk count
//     via the tally channel (buffered so workers never block).
//  2. A dedicated accumulator goroutine drains tally, keeps a running
//     total, and calls cancel() on the errgroup context once the total
//     crosses the cap. Already-running rg subprocesses see the cancel
//     via exec.CommandContext and exit within milliseconds.
//  3. Newly-scheduled chunks (still in g.SetLimit's queue) see gctx
//     already canceled and return immediately without spawning rg.
//  4. Canceled chunks return ctx.Err() which we SWALLOW here — the
//     cancellation is intentional, not a failure mode. Errors for other
//     reasons (bad regex, I/O) still surface as before.
//
// Without this, ProcessAllChunks would scan all chunks to completion
// regardless of max_results, defeating the whole point of the limit
// on large files. Measured impact before the fix: a 1.3 GB file split
// into 20 chunks spawned 20 rg subprocesses and read the full file
// even when max_results=10. After the fix: typically 1-3 rg subprocesses
// spawn (one per the chunks scheduled before the cap fires), and rg
// exits on first EOF after we close its stdin.
//
// The final post-hoc sort + truncation in engine.go still applies —
// this cooperation just ensures we don't do unnecessary work past the
// cap. Under-shoot is possible if a chunk returning 0 matches finished
// first; the code keeps scheduling until matches >= cap.
func ProcessAllChunks(
	ctx context.Context,
	tasks []FileTask,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
	maxResults *int,
) ([]ChunkResult, error) {
	results := make([]ChunkResult, len(tasks))
	if len(tasks) == 0 {
		return results, nil
	}

	workers := workerLimit()
	// Derive an explicit cancel so the budget can terminate in-flight
	// workers when the cap is reached. errgroup.WithContext gives us
	// cancel-on-error; we need cancel-on-success-too.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(gctx)
	g.SetLimit(workers)

	// One budget shared by every worker on this file. Nil when the
	// caller set no cap, in which case charging it is a no-op.
	var budget *MatchBudget
	if maxResults != nil {
		budget = NewMatchBudget(*maxResults, cancel)
	}

	for i := range tasks {
		i := i // capture for closure
		task := tasks[i]
		g.Go(func() error {
			// Fast-path: already canceled — skip the expensive ProcessChunk.
			// This saves spawning an rg subprocess only to have it killed
			// immediately. The cancel check inside ProcessChunk would
			// also exit promptly, but cheaper to skip entirely.
			if err := gctx.Err(); err != nil {
				return nil
			}
			res, err := ProcessChunk(gctx, ChunkRequest{
				Task:          task,
				PatternIDs:    patternIDs,
				PatternOrder:  patternOrder,
				RgExtraArgs:   rgExtraArgs,
				ContextBefore: contextBefore,
				ContextAfter:  contextAfter,
				Budget:        budget,
			})
			// Record whatever the chunk produced either way: a chunk
			// canceled by the cap still holds the matches it found
			// before the cancel, and its Complete flag tells the
			// caller its newline count is partial.
			results[i] = res
			if err != nil {
				// context.Canceled from rg being killed by
				// exec.CommandContext on our cancel() is an EXPECTED
				// termination — cooperative cancel on max_results cap,
				// not a failure. Swallow it so g.Wait doesn't report it.
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
			return nil
		})
	}
	waitErr := g.Wait()

	// Classify the final error. Three shapes:
	//  a) nil                        — all good
	//  b) context.Canceled + cap spent — cooperative cancel, swallow
	//  c) context.Canceled, cap unspent — external cancel (outer ctx), surface
	//  d) other                      — real error (bad regex, I/O), surface
	if waitErr != nil && errors.Is(waitErr, context.Canceled) && budget.Exhausted() {
		return results, nil
	}
	if waitErr != nil {
		return results, waitErr
	}
	return results, nil
}

// workerLimit returns the effective concurrency cap.
//
// Precedence:
//  1. RX_WORKERS env var (new Go addition; no Python equivalent).
//  2. RX_MAX_SUBPROCESSES env var (Python parity).
//  3. runtime.NumCPU().
//
// The hard lower bound is 1. The upper bound is config.MaxSubprocesses()
// to keep subprocess pressure sane even on beefy machines.
func workerLimit() int {
	if v := config.GetIntEnv("RX_WORKERS", 0); v > 0 {
		return v
	}
	ms := config.MaxSubprocesses()
	if ms < 1 {
		ms = 1
	}
	nc := runtime.NumCPU()
	if nc < 1 {
		nc = 1
	}
	if nc < ms {
		return nc
	}
	return ms
}

// filterIncompatibleRgArgs strips the rg flags that would corrupt the
// --json output we rely on. Python does the same in trace_worker.py.
//
// --byte-offset and --only-matching both override the default event
// shape and break our parser's assumptions.
func filterIncompatibleRgArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--byte-offset" || a == "--only-matching" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// trimTrailingNewline strips at most one trailing '\n' or '\r\n'.
// ripgrep always includes the newline in its `lines.text` payload;
// Python and rx-go both rstrip('\n') before storing the line text.
func trimTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

// isBrokenPipe detects the "broken pipe" / "file already closed" case
// that appears when rg exits before stdin is fully written — e.g. on
// context cancellation or when rg hits an internal limit.
func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	// os package surfaces ErrClosed; os/exec returns raw syscall.EPIPE.
	if errors.Is(err, os.ErrClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "file already closed") ||
		strings.Contains(msg, "EPIPE")
}

// newRgArgs returns the arguments every rx search starts ripgrep with,
// as a fresh slice the caller appends to.
//
// --no-config keeps RIPGREP_CONFIG_PATH out of the answer. A user's
// ripgreprc can hold --fixed-strings, --smart-case or --max-columns,
// each of which changes what rg reports, and an rx answer must not
// depend on who runs it. --json is the event stream the parser reads;
// --no-heading and --color=never keep it free of decoration.
func newRgArgs() []string {
	return []string{"--no-config", "--json", "--no-heading", "--color=never"}
}

// ErrInvalidPattern reports a pattern ripgrep refused to compile. It is
// fatal for the whole request: no file can be searched with a pattern
// that does not parse, so callers must surface it (CLI exit 2, HTTP 400)
// rather than record the file as skipped.
var ErrInvalidPattern = errors.New("invalid regex pattern")

// isRegexParseError recognizes ripgrep's own wording for a pattern it
// could not compile. rg exits 2 for this and prints, on stderr:
//
//	regex parse error:
//	    (?:a()
//	    ^
//	error: unclosed group
//
// Matching on the text is the only option: rg uses exit status 2 for
// every fatal error, not just this one.
func isRegexParseError(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "regex parse error") ||
		strings.Contains(lowered, "error parsing regex")
}

// invalidPatternError reports a pattern ripgrep refused, in terms of
// what the caller typed.
//
// rg compiles the patterns it is given as one alternation, so a pattern
// of `(bad` comes back as a complaint about `(?:(bad)` with the caret
// pointing into a wrapper the caller never wrote. The reason rg gives
// is worth keeping; the wrapper is not.
func invalidPatternError(stderr string, patternIDs map[string]string, patternOrder []string) error {
	reason := regexFailureReason(stderr)
	patterns := make([]string, 0, len(patternOrder))
	for _, pid := range patternOrder {
		patterns = append(patterns, strconv.Quote(patternIDs[pid]))
	}
	switch len(patterns) {
	case 0:
		return fmt.Errorf("%w: %s", ErrInvalidPattern, reason)
	case 1:
		return fmt.Errorf("%w %s: %s", ErrInvalidPattern, patterns[0], reason)
	default:
		return fmt.Errorf("%w: %s (patterns: %s)",
			ErrInvalidPattern, reason, strings.Join(patterns, ", "))
	}
}

// regexFailureReason pulls the explanation out of ripgrep's parse
// error, which ends with a line of the form "error: unclosed group".
// Anything unrecognized falls back to the whole message on one line, so
// no detail is lost when rg changes its wording.
func regexFailureReason(stderr string) string {
	reason := ""
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if after, found := strings.CutPrefix(line, "error: "); found {
			reason = after
		}
	}
	if reason != "" {
		return reason
	}
	compact := strings.Join(strings.Fields(stderr), " ")
	return strings.TrimPrefix(compact, "rg: ")
}

// newlineBytes is the separator the chunk counter looks for. Declared
// once so the per-buffer count in ProcessChunk allocates nothing.
var newlineBytes = []byte{'\n'}

package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// Hook firer interface (stub for M3; real impl lands in M4)
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
	Pattern    string
	Offset     int64
	LineNumber int64
}

// HookFirer is the interface the trace engine uses to notify webhooks
// as matches/files complete. It's intentionally abstracted so the
// trace package can be tested without any HTTP stack in scope.
//
// Concrete implementations live in internal/hooks (M4). The default
// for M3 is NoopHookFirer, which is also what CLI `rx trace` uses by
// default (no RX_HOOK_* env vars set).
//
// Decision 6.9.2 binds the implementation style: fire-and-forget,
// single POST per event, 3 s timeout, no retries. The engine does NOT
// block on Hook calls; implementations must enqueue work on a channel
// or spawn a goroutine before returning.
type HookFirer interface {
	// OnFile is called once per file after its scan completes.
	OnFile(ctx context.Context, path string, info FileInfo)
	// OnMatch is called once per match. Can be very hot; implementations
	// that cannot buffer should drop events rather than block the scan.
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
// The filter pattern is borrowed from
// another-rx-go/internal/engine/worker.go:109-118 — a single
// per-match range-containment check at the accept point, no
// cross-worker coordination and no post-merge dedup pass.
type MatchRaw struct {
	Offset       int64
	LineNumber   int
	LineText     string
	Submatches   []rxtypes.Submatch
	PatternIDs   []string // all pattern IDs (assigned by engine post-hoc)
	IsCompressed bool
}

// ContextRaw mirrors MatchRaw for context lines.
type ContextRaw struct {
	Offset     int64
	LineNumber int
	LineText   string
}

// ProcessChunk runs `rg --json` over a single chunk of a file, feeding
// the chunk bytes in via stdin using os.File.ReadAt (Decision 5.1:
// native chunking, no `dd` subprocess). It parses the rg event stream,
// filters out matches that don't belong to this chunk's range, and
// returns the rich MatchRaw + ContextRaw slices.
//
// Deduplication rule (matches Python verbatim, Decision 6.9.5;
// borrowed from another-rx-go/internal/engine/worker.go:116):
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
func ProcessChunk(
	ctx context.Context,
	task FileTask,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
) (matches []MatchRaw, contexts []ContextRaw, elapsed time.Duration, err error) {
	start := time.Now()
	// Stage 9 Round 2 S6: gate all metric updates behind the package
	// enabled switch — CLI-mode callers pay no cost here.
	prometheus.IncActiveWorkers()
	defer prometheus.DecActiveWorkers()
	defer func() {
		if err == nil {
			prometheus.IncWorkerTasksCompleted()
		} else {
			prometheus.IncWorkerTasksFailed()
		}
	}()

	// Build the rg argv. We filter out args that would change the
	// output shape (--byte-offset, --only-matching) — Python does
	// the same in trace_worker.py.
	rgArgs := []string{"--json", "--no-heading", "--color=never"}
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

	rgCmd := exec.CommandContext(ctx, "rg", rgArgs...)

	// stdin pipe carries the chunk bytes from our ReadAt loop into rg.
	rgStdin, err := rgCmd.StdinPipe()
	if err != nil {
		return nil, nil, time.Since(start), fmt.Errorf("ProcessChunk: rg stdin pipe: %w", err)
	}
	// stdout is the rg --json event stream we parse.
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return nil, nil, time.Since(start), fmt.Errorf("ProcessChunk: rg stdout pipe: %w", err)
	}
	// stderr captured to a buffered string — surfaces rg errors
	// (invalid regex, etc.) when the worker returns an error.
	var stderrBuf strings.Builder
	rgCmd.Stderr = &stderrBuf

	if startErr := rgCmd.Start(); startErr != nil {
		return nil, nil, time.Since(start), fmt.Errorf("ProcessChunk: rg start: %w", startErr)
	}

	// Open the source file. ReadAt is goroutine-safe and doesn't move
	// a shared cursor, so we can stream in one goroutine while parsing
	// events in another.
	src, err := os.Open(task.FilePath)
	if err != nil {
		_ = rgStdin.Close()
		_ = rgStdout.Close()
		_ = rgCmd.Wait()
		return nil, nil, time.Since(start), fmt.Errorf("ProcessChunk: open %s: %w", task.FilePath, err)
	}
	defer func() { _ = src.Close() }()

	// Feed chunk bytes → rg stdin in a goroutine. io.Copy + io.LimitReader
	// wrapping an *os.File offset by SectionReader gives us:
	//  - ReadAt-backed reads (no shared cursor)
	//  - Exact byte-count limit (task.Count)
	//  - Automatic EOF when the range is exhausted
	section := io.NewSectionReader(src, task.Offset, task.Count)

	// errgroup here lets us surface stdin-copy errors alongside the
	// event-parse errors. Both must finish before we Wait() on rg.
	g, gctx := errgroup.WithContext(ctx)

	// Goroutine 1: pump chunk bytes into rg stdin.
	g.Go(func() error {
		defer func() { _ = rgStdin.Close() }()
		// 64 KB copy buffer is a good tradeoff — larger wastes memory
		// per concurrent worker; smaller makes more syscalls.
		buf := make([]byte, 64*1024)
		_, copyErr := io.CopyBuffer(rgStdin, section, buf)
		// If the caller canceled, we might get "broken pipe" back
		// from rgStdin.Write as rg exits early. Swallow that — it's a
		// clean shutdown, not a real error.
		if copyErr != nil && !isBrokenPipe(copyErr) && gctx.Err() == nil {
			return fmt.Errorf("stdin copy: %w", copyErr)
		}
		return nil
	})

	// Goroutine 2: parse rg --json events and collect matches.
	var mu struct {
		matches  []MatchRaw
		contexts []ContextRaw
	}
	g.Go(func() error {
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
				// rg's absolute_offset is relative to its OWN input
				// (i.e. chunk-local). Translate to file-absolute by
				// adding the chunk's starting byte in the source file.
				absOff := task.Offset + ev.Match.AbsoluteOffset
				// Dedup filter: only keep matches whose absolute start
				// offset falls within THIS chunk's assigned half-open
				// range [task.Offset, task.EndOffset()). Matches at or
				// past the next chunk's start belong to that chunk —
				// rg may legitimately report such matches because we
				// feed it a byte stream that ends on a newline
				// boundary aligned for the NEXT chunk's start.
				//
				// This invariant is local to each worker — a duplicate
				// match would be a chunker bug, not something this
				// filter needs to patch at merge time. Borrowed from
				// another-rx-go/internal/engine/worker.go:116; matches
				// Python decision 6.9.5 verbatim.
				if absOff < task.Offset || absOff >= task.EndOffset() {
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
				mu.matches = append(mu.matches, MatchRaw{
					Offset:     absOff,
					LineNumber: ev.Match.LineNumber,
					LineText:   trimTrailingNewline(ev.Match.Lines.Text),
					Submatches: subs,
					// pattern IDs are the FULL set — engine.identify
					// narrows this down post-hoc per Python parity.
					PatternIDs: append([]string(nil), patternOrder...),
				})
			case RgEventContext:
				if ev.Context == nil {
					return nil
				}
				// Same range-containment dedup as the match branch —
				// context lines reported by rg outside THIS chunk's
				// assigned range belong to an adjacent chunk's output.
				absOff := task.Offset + ev.Context.AbsoluteOffset
				if absOff < task.Offset || absOff >= task.EndOffset() {
					return nil
				}
				mu.contexts = append(mu.contexts, ContextRaw{
					Offset:     absOff,
					LineNumber: ev.Context.LineNumber,
					LineText:   trimTrailingNewline(ev.Context.Lines.Text),
				})
			default:
				// begin/end/summary — ignored for per-chunk processing.
			}
			return nil
		})
	})

	// Wait for both goroutines, THEN Wait on rg. Order matters: if the
	// parser errors out, we need stdin closed to let rg exit, otherwise
	// we'd deadlock on rgCmd.Wait(). errgroup.Wait() doesn't close
	// stdin for us; the stdin goroutine's defer does that reliably.
	groupErr := g.Wait()
	waitErr := rgCmd.Wait()

	elapsed = time.Since(start)

	// rg exits 1 when no matches found — that's NOT an error for us.
	// Exit 2 is a real error (bad regex, etc.).
	//
	// When exec.CommandContext kills rg on ctx cancellation, ExitCode()
	// returns -1 (signal, not exit status). Stage 9 Round 5 R5-B2: this
	// is EXPECTED during cooperative cancel on max_results cap. If the
	// outer ctx is canceled, classify the error as context.Canceled
	// rather than leaking "rg exit -1" to the caller. The ProcessAllChunks
	// swallow logic depends on this shape.
	if waitErr != nil {
		// ctx-canceled trumps all other classifications. If the parent
		// context is canceled, any rg exit (killed or otherwise) is
		// expected and we return context.Canceled so the errgroup
		// signaling works correctly.
		if ctx.Err() != nil {
			return mu.matches, mu.contexts, elapsed, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code := exitErr.ExitCode()
			if code != 0 && code != 1 {
				return nil, nil, elapsed, fmt.Errorf(
					"rg exit %d: %s", code, strings.TrimSpace(stderrBuf.String()))
			}
		} else if !errors.Is(waitErr, context.Canceled) {
			return nil, nil, elapsed, fmt.Errorf("rg wait: %w", waitErr)
		}
	}
	if groupErr != nil && !errors.Is(groupErr, context.Canceled) {
		return nil, nil, elapsed, groupErr
	}

	return mu.matches, mu.contexts, elapsed, nil
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
// # Bounded-read contract (Stage 9 Round 5, R5-B2)
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
) ([][]MatchRaw, [][]ContextRaw, error) {
	allMatches := make([][]MatchRaw, len(tasks))
	allContexts := make([][]ContextRaw, len(tasks))
	if len(tasks) == 0 {
		return allMatches, allContexts, nil
	}

	workers := workerLimit()
	// Derive an explicit cancel so the tally goroutine can terminate
	// in-flight workers when the cap is reached. errgroup.WithContext
	// gives us cancel-on-error; we need cancel-on-success-too.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(gctx)
	g.SetLimit(workers)

	// tally is buffered so workers never block on send. Size = len(tasks)
	// is the theoretical maximum (one send per chunk); in practice most
	// writes complete instantly as the goroutine drains concurrently.
	tally := make(chan int, len(tasks))
	// done signals the accumulator to exit. Closed exactly once via
	// defer below.
	done := make(chan struct{})
	// capHit records whether the cap fired — used by the caller to
	// distinguish cooperative-cancel from external-cancel when
	// classifying errors returned from g.Wait().
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
				if maxResults != nil && total >= *maxResults {
					// Fire cancel exactly once — further writes to tally
					// still succeed (channel is buffered) and will be
					// drained by this loop until the channel closes.
					if !capHit {
						capHit = true
						cancel()
					}
				}
			case <-gctx.Done():
				// Outer ctx canceled — drain remaining sends so blocked
				// workers (if any) can return. The buffered channel
				// means they won't actually be blocked, but we still
				// need to exit cleanly when Wait returns below.
				return
			}
		}
	}()

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
			matches, contexts, _, err := ProcessChunk(
				gctx, task, patternIDs, patternOrder,
				rgExtraArgs, contextBefore, contextAfter,
			)
			if err != nil {
				// context.Canceled from rg subprocess being killed by
				// exec.CommandContext on our cancel() is an EXPECTED
				// termination — cooperative cancel on max_results cap,
				// not a failure. Swallow here so g.Wait doesn't return
				// it as the error. Partial per-chunk results (matches)
				// may still be populated; we record them below.
				if errors.Is(err, context.Canceled) {
					// Still publish any matches we collected before
					// cancellation — the cap-tally loop can count
					// them, though at this point it won't change the
					// outcome since cancel has already fired.
					allMatches[i] = matches
					allContexts[i] = contexts
					select {
					case tally <- len(matches):
					default:
						// Channel is sized len(tasks); this cannot
						// overflow under normal conditions. `default`
						// is a defensive no-op.
					}
					return nil
				}
				return err
			}
			allMatches[i] = matches
			allContexts[i] = contexts
			// Publish match count to the accumulator. Non-blocking
			// because tally is buffered at len(tasks).
			select {
			case tally <- len(matches):
			default:
			}
			return nil
		})
	}
	waitErr := g.Wait()
	// Close tally so the accumulator goroutine exits. Must happen AFTER
	// Wait so we don't close while workers are still writing.
	close(tally)
	<-done

	// Classify the final error. Three shapes:
	//  a) nil                — all good
	//  b) context.Canceled + capHit  — cooperative cancel, swallow (not an error)
	//  c) context.Canceled + !capHit — external cancel (outer ctx), surface
	//  d) other              — real error (bad regex, I/O), surface
	if waitErr != nil && errors.Is(waitErr, context.Canceled) && capHit {
		return allMatches, allContexts, nil
	}
	if waitErr != nil {
		return allMatches, allContexts, waitErr
	}
	return allMatches, allContexts, nil
}

// workerLimit returns the effective concurrency cap.
//
// Precedence (matches stage-6 plan §6.5.10):
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

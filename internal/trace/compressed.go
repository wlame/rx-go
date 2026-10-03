package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wlame/rx-go/internal/compression"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
)

// ErrUnsupportedCompression is returned by ProcessCompressed when asked
// to process FormatNone (caller bug) or a format the reader layer
// doesn't know. The actual format whitelist is enforced by
// compression.NewReader — this helper is kept exported for API
// compatibility with tests and downstream packages that assert on it.
var ErrUnsupportedCompression = errors.New("unsupported compression format")

// ErrIncompleteStream reports that a compressed file ended before its
// decompressor expected it to — a truncated archive, or one corrupted
// past the point rx reached. ProcessCompressed returns it together with
// the matches it did read, so a caller can report both what was found
// and the fact that the file was not searched to the end.
var ErrIncompleteStream = errors.New("compressed stream ended early")

// ProcessCompressed runs the full scan pipeline for a non-seekable
// compressed file: read file → pure-Go decompressor pipe → rg --json.
//
// STATIC BINARY:
//
// Decompression runs in-process. We read the source file ourselves,
// wrap it in compression.NewReader (which uses compress/gzip,
// compress/bzip2, github.com/ulikunitz/xz, and
// github.com/klauspost/compress/zstd — all pure Go), and pipe the
// resulting io.Reader directly into rg's stdin. No external `gzip`,
// `xz`, `bzip2` or `zstd` binary is needed, so the scan works on
// distroless, busybox and slim Docker images that lack them. The only
// subprocess is rg itself, which is expected to be on PATH (it's the
// core of the app).
//
// Offset semantics:
//   - MatchRaw.Offset is the DECOMPRESSED byte offset (matches Python).
//   - Line numbers are 1-indexed and refer to the decompressed stream.
//
// maxResults caps the number of returned matches. After that many the
// worker reads on only until rg has written the trailing context of the
// last one (contextAfter lines), then stops rg via context cancel; a
// match read in that time is returned as a context line, since it is a
// line of that window. Nil maxResults means "no limit".
//
// source is the file pinned when the trace checked it; it is read only
// if it is still that file.
func ProcessCompressed(
	ctx context.Context,
	source sandbox.Pinned,
	format compression.Format,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
	maxResults *int,
) (matches []MatchRaw, contexts []ContextRaw, elapsed time.Duration, err error) {
	// incomplete is set when the decompressor stops early. It travels
	// back as the returned error while the matches travel back beside
	// it, so the caller can keep the data and still know it is partial.
	var incomplete error
	start := time.Now()
	// gated helpers — CLI mode skips collection.
	prometheus.IncActiveWorkers()
	defer prometheus.DecActiveWorkers()
	defer func() { recordWorkerOutcome(err) }()

	if format == compression.FormatNone {
		return nil, nil, 0, fmt.Errorf("%w: ProcessCompressed called with FormatNone on %s",
			ErrUnsupportedCompression, source.Path())
	}

	// Open the source file. The decompressor wraps this reader; no
	// subprocess is spawned. Open refuses a path that no longer leads to
	// the file the trace checked.
	src, err := source.Open()
	if err != nil {
		return nil, nil, time.Since(start), fmt.Errorf("open %s: %w", source.Path(), err)
	}
	// R2M2 contract: compression.NewReader takes ownership of src on
	// success — its returned wrapper's Close will close src. If
	// NewReader returns an error BEFORE adopting src (construction
	// failed), we must close src ourselves. The branches below
	// handle both cases explicitly rather than relying on a defer
	// that could double-close.
	dec, err := compression.NewReader(src, format)
	if err != nil {
		_ = src.Close()
		return nil, nil, time.Since(start), fmt.Errorf("%w: %s (%v)",
			ErrUnsupportedCompression, format, err)
	}
	defer func() {
		// Close releases the decoder's internal state AND the source
		// file handle via the chainCloser. One defer, both closes.
		_ = dec.Close()
	}()

	// Per-call child context so we can cancel rg cleanly on early exit
	// (maxResults hit or error).
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Build rg argv same way as ProcessChunk.
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

	rgCmd := exec.CommandContext(childCtx, "rg", rgArgs...)

	// Wire the decompressor's output to rg's stdin. We use a pipe so
	// rg can stream-process while we continue reading; an io.Copy
	// goroutine pumps bytes from `dec` into rg's stdin.
	rgStdin, err := rgCmd.StdinPipe()
	if err != nil {
		return nil, nil, time.Since(start), fmt.Errorf("rg stdin pipe: %w", err)
	}
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		_ = rgStdin.Close()
		return nil, nil, time.Since(start), fmt.Errorf("rg stdout pipe: %w", err)
	}
	var rgStderr strings.Builder
	rgCmd.Stderr = &rgStderr

	if err := rgCmd.Start(); err != nil {
		_ = rgStdin.Close()
		return nil, nil, time.Since(start), fmt.Errorf("rg start: %w", err)
	}

	// Goroutine: pump decompressed bytes into rg's stdin. Close stdin
	// when done so rg knows EOF.
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(rgStdin, dec)
		// Closing rgStdin is what tells rg "no more input" — must
		// happen after the copy, not deferred by the caller.
		_ = rgStdin.Close()
		copyDone <- copyErr
	}()

	var outMatches []MatchRaw
	var outContexts []ContextRaw
	matchCount := 0
	// lastWindowLine is 0 until the cap is reached, and then the last
	// line of the window of the last match counted: the line after which
	// the pass stops.
	lastWindowLine := 0

	streamErr := StreamEvents(childCtx, rgStdout, func(ev *RgEvent, parseErr error) error {
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
			// The whole stream goes through one rg, so its offsets and
			// line numbers are already the decompressed text's.
			line := rawMatchLine(ev.Match, ev.Match.AbsoluteOffset, ev.Match.LineNumber)
			line.IsCompressed = true
			if lastWindowLine > 0 {
				// Past the cap: a line of the last match's window.
				outContexts = append(outContexts, matchAsContext(line))
				return stopAfterWindow(ev.Match.LineNumber, lastWindowLine)
			}
			outMatches = append(outMatches, line.withSubmatches(ev.Match, patternOrder))
			matchCount++
			if maxResults != nil && matchCount >= *maxResults {
				lastWindowLine = ev.Match.LineNumber + contextAfter
				return stopAfterWindow(ev.Match.LineNumber, lastWindowLine)
			}
		case RgEventContext:
			if ev.Context == nil {
				return nil
			}
			outContexts = append(outContexts, rawContextLine(ev.Context, ev.Context.AbsoluteOffset, ev.Context.LineNumber))
			if lastWindowLine > 0 {
				return stopAfterWindow(ev.Context.LineNumber, lastWindowLine)
			}
		}
		return nil
	})

	// INVARIANT: rg is never waited on while its stdout is unread. A nil
	// streamErr means the parser read rg's stdout to the end, so rg has
	// written everything and is exiting. Any other return means the
	// parser stopped early, and rg may still have output to write: with
	// nobody reading, it would block on the full pipe and rgCmd.Wait
	// below would never return. Canceling childCtx makes
	// exec.CommandContext kill rg, and the io.Copy goroutine then
	// unblocks with a broken pipe on its next write.
	if streamErr != nil {
		cancel()
	}
	// io.EOF from inside the callback is the cap's "stop early" signal,
	// not a failure.
	if errors.Is(streamErr, io.EOF) {
		streamErr = nil
	}

	// Reap rg and wait for the copy goroutine so we don't leak either.
	rgWaitErr := rgCmd.Wait()
	copyErr := <-copyDone

	elapsed = time.Since(start)

	// Classify rg exit codes as in ProcessChunk.
	if rgWaitErr != nil && !isSubprocessCancelled(rgWaitErr) {
		var ex *exec.ExitError
		if errors.As(rgWaitErr, &ex) {
			code := ex.ExitCode()
			if code != 0 && code != 1 {
				return nil, nil, elapsed, fmt.Errorf(
					"rg exit %d: %s", code, strings.TrimSpace(rgStderr.String()))
			}
		} else {
			return nil, nil, elapsed, fmt.Errorf("rg wait: %w", rgWaitErr)
		}
	}
	// Copy errors: EPIPE on cancel is expected; a genuine I/O error
	// (e.g. corrupt stream mid-read) surfaces here. Python logs a
	// warning and returns empty (no hard error). We match that: log
	// the corruption at Warn so operators can see it in their logs,
	// but don't fail the call. See R2M3 regression-guard test in
	// compressed_test.go.
	//
	// Filter out the normal early-termination noise: when we cancel
	// rg after hitting maxResults, the io.Copy goroutine sees EPIPE
	// or "io: read/write on closed pipe" on its next write — those
	// are EXPECTED and should not be logged as corruption.
	if copyErr != nil && !isCopyTerminationNoise(copyErr) {
		slog.Default().Warn("compressed_stream_copy_error",
			"path", source.Path(),
			"format", string(format),
			"error", copyErr.Error(),
		)
		incomplete = fmt.Errorf("%w: %s: %w", ErrIncompleteStream, source.Path(), copyErr)
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		return nil, nil, elapsed, streamErr
	}

	// The matches read before the stream broke are real, so they are
	// returned alongside the error. A truncated archive used to produce
	// a partial result that looked complete; the caller now has both the
	// data and the fact that there is more it could not reach.
	return outMatches, outContexts, elapsed, incomplete
}

// isCopyTerminationNoise reports whether an error from the io.Copy
// goroutine is expected-and-benign (EPIPE / closed-pipe on cancel)
// rather than a real corruption signal. We don't want to drown
// operators in Warn logs every time a maxResults cap triggers a
// cancel-and-drain — those are normal pipeline terminations.
func isCopyTerminationNoise(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "closed pipe") ||
		strings.Contains(msg, "file already closed")
}

// isSubprocessCancelled returns true for the set of errors that
// indicate a subprocess was terminated by our own context cancellation
// (e.g. via cancel() when maxResults is reached).
func isSubprocessCancelled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	// os/exec surfaces "signal: killed" when we SIGKILL the child —
	// that's our normal cancellation mechanism.
	return strings.Contains(msg, "signal: killed") ||
		strings.Contains(msg, "signal: terminated") ||
		strings.Contains(msg, "broken pipe")
}

// stopAfterWindow tells the event loop of a capped pass to stop once
// rg has reported line lastWindowLine, the end of the last counted
// match's window. io.EOF is the loop's "stop early" signal; nil reads
// on.
func stopAfterWindow(line, lastWindowLine int) error {
	if line >= lastWindowLine {
		return io.EOF
	}
	return nil
}

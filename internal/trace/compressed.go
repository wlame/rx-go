package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrUnsupportedCompression is returned by ProcessCompressed when asked
// to process FormatNone (caller bug) or a format the reader layer
// doesn't know. The actual format whitelist is enforced by
// compression.NewReader — this helper is kept exported for API
// compatibility with tests and downstream packages that assert on it.
var ErrUnsupportedCompression = errors.New("unsupported compression format")

// ProcessCompressed runs the full scan pipeline for a non-seekable
// compressed file: read file → pure-Go decompressor pipe → rg --json.
//
// STATIC-BINARY CONSOLIDATION (Stage 8 Finding 4):
//
// Prior to consolidation this function shelled out to external
// `gzip -d -c`, `xz -d -c`, `bzip2 -d -c`, or `zstd -d -c` binaries
// to produce the decompressed stream. That required the host to have
// those binaries on PATH — fine on full Linux distros, broken on
// distroless / busybox / slim Docker images that rx-go now targets
// per decision 5.15 ("single static binary").
//
// Post-consolidation: we read the source file ourselves, wrap it in
// compression.NewReader (which uses compress/gzip, compress/bzip2,
// github.com/ulikunitz/xz, and github.com/klauspost/compress/zstd —
// all pure Go), and pipe the resulting io.Reader directly into rg's
// stdin. Zero external binaries required for the decompression step.
// The only subprocess remaining is rg itself, which is expected to be
// on PATH (it's the core of the app).
//
// Offset semantics:
//   - MatchRaw.Offset is the DECOMPRESSED byte offset (matches Python).
//   - Line numbers are 1-indexed and refer to the decompressed stream.
//
// maxResults caps the number of returned matches. The worker stops
// reading events after that many, terminating rg via context cancel.
// Nil maxResults means "no limit".
func ProcessCompressed(
	ctx context.Context,
	path string,
	format compression.Format,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
	maxResults *int,
) (matches []MatchRaw, contexts []ContextRaw, elapsed time.Duration, err error) {
	start := time.Now()
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

	if format == compression.FormatNone {
		return nil, nil, 0, fmt.Errorf("%w: ProcessCompressed called with FormatNone on %s",
			ErrUnsupportedCompression, path)
	}

	// Open the source file. The decompressor wraps this reader; no
	// subprocess is spawned.
	src, err := os.Open(path)
	if err != nil {
		return nil, nil, time.Since(start), fmt.Errorf("open %s: %w", path, err)
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
			if maxResults != nil && matchCount >= *maxResults {
				// Stop reading; we signal early termination via
				// context cancel below.
				return io.EOF
			}
			subs := make([]rxtypes.Submatch, len(ev.Match.Submatches))
			for i, sm := range ev.Match.Submatches {
				subs[i] = rxtypes.Submatch{
					Text:  sm.Text(),
					Start: sm.Start,
					End:   sm.End,
				}
			}
			outMatches = append(outMatches, MatchRaw{
				Offset:       ev.Match.AbsoluteOffset, // already decompressed-stream-relative
				LineNumber:   ev.Match.LineNumber,
				LineText:     trimTrailingNewline(ev.Match.Lines.Text),
				Submatches:   subs,
				PatternIDs:   append([]string(nil), patternOrder...),
				IsCompressed: true,
			})
			matchCount++
		case RgEventContext:
			if ev.Context == nil {
				return nil
			}
			outContexts = append(outContexts, ContextRaw{
				Offset:     ev.Context.AbsoluteOffset,
				LineNumber: ev.Context.LineNumber,
				LineText:   trimTrailingNewline(ev.Context.Lines.Text),
			})
		}
		return nil
	})

	// If the stream returned io.EOF from inside the callback we use
	// that as our "stop early" signal — not a real error. Cancel so
	// rg exits and the io.Copy goroutine unblocks via broken-pipe
	// on its next write.
	if errors.Is(streamErr, io.EOF) {
		cancel()
		streamErr = nil
	}

	// Reap rg and wait for the copy goroutine so we don't leak either.
	// Order matters: waiting on rg.Wait AFTER closing stdin (done inside
	// the copy goroutine) lets rg drain its output buffer cleanly.
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
			"path", path,
			"format", string(format),
			"error", copyErr.Error(),
		)
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		return nil, nil, elapsed, streamErr
	}

	return outMatches, outContexts, elapsed, nil
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

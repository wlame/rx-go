package clicommand

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wlame/rx-go/internal/paths"
)

// The exit codes rx guarantees. They are contract: scripts branch on
// them, docs/cli/index.md publishes them, and rx-python uses the same
// table.
//
//	0 → success (including a successful scan with no matches)
//	1 → generic error (subprocess failure, IO error)
//	2 → usage error: bad flag, missing argument, invalid regex
//	3 → file not found
//	4 → access denied (path outside --search-root)
//	5 → interrupted by a signal
const (
	ExitSuccess      = 0
	ExitGenericError = 1
	ExitUsageError   = 2
	ExitFileNotFound = 3
	ExitAccessDenied = 4
	ExitInterrupted  = 5
)

// colorDecision reads NO_COLOR and RX_NO_COLOR envs plus the --no-color
// flag to decide whether ANSI escapes should be emitted. --no-color
// takes precedence, then RX_NO_COLOR, then NO_COLOR.
//
// This mirrors rx-python/src/rx/cli/trace.py:disable_color_decision.
func colorDecision(noColorFlag bool, stdout io.Writer) bool {
	if noColorFlag {
		return false
	}
	if os.Getenv("RX_NO_COLOR") != "" {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	// Respect TTY: only emit colors when stdout is a real terminal.
	// Falls back to "enabled" when we can't tell (file-backed writer in tests).
	if f, ok := stdout.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			if (fi.Mode() & os.ModeCharDevice) == 0 {
				return false
			}
		}
	}
	return true
}

// stdinIsPipe reports whether os.Stdin looks like a piped / redirected
// input (as opposed to a TTY). Used by `rx trace` to decide whether to
// read patterns from stdin.
func stdinIsPipe() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}

// ExitError carries the process exit code an error should produce.
// cmd/rx unwraps it after cobra returns and exits with Code; anything
// else exits 1.
type ExitError struct {
	Code int
	Err  error
}

// Error implements the error interface.
func (e *ExitError) Error() string { return e.Err.Error() }

// Unwrap lets errors.Is / errors.As reach the cause.
func (e *ExitError) Unwrap() error { return e.Err }

// NewExitError wraps err with the exit code it should produce.
func NewExitError(code int, err error) *ExitError {
	return &ExitError{Code: code, Err: err}
}

// exitWithError prints an error message to stderr (with an "Error: "
// prefix) and returns an *ExitError carrying the code. Return its value
// from a RunE function: `return exitWithError(os.Stderr, ExitUsageError, ...)`.
// Discarding it would leave the process exiting 1.
func exitWithError(w io.Writer, code int, format string, args ...any) *ExitError {
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(w, "Error: %s\n", msg)
	return NewExitError(code, errors.New(msg))
}

// sandboxCheck validates a user-supplied path against the --search-root
// sandbox and returns the validated form.
//
// The CLI usually runs without a sandbox: no search roots are configured
// unless `serve` (or a test) installs them, and in that case every path
// is allowed and returned unchanged. When roots are configured, a path
// outside them is an access-denied error (exit code 4).
func sandboxCheck(path string) (string, error) {
	validated, err := paths.ValidatePathWithinRoots(path)
	if err != nil {
		if errors.Is(err, paths.ErrNoSearchRootsConfigured) {
			return path, nil
		}
		return "", err
	}
	return validated, nil
}

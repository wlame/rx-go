package clicommand

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unicode"
	"unicode/utf8"

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
	// Anything that is not one — a redirect, a pipe, or a writer that is
	// not a file at all — gets plain text, because escape sequences in a
	// file the user will read back are corruption, not decoration.
	// rx-python decides the same way, through sys.stdout.isatty().
	f, ok := stdout.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
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
	// Reported is true when the error line is already on stderr, so
	// cmd/rx must not print it a second time. exitWithError sets it;
	// NewExitError leaves it false, and cmd/rx prints the error.
	Reported bool
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
	PrintError(w, msg)
	exitErr := NewExitError(code, errors.New(msg))
	exitErr.Reported = true
	return exitErr
}

// PrintError writes the one error line every rx failure prints:
// "Error: " followed by the message with its first letter capitalized.
func PrintError(w io.Writer, msg string) {
	_, _ = fmt.Fprintf(w, "Error: %s\n", capitalizeFirst(msg))
}

// IsReported reports whether err, or an error it wraps, is an ExitError
// whose line is already on stderr.
func IsReported(err error) bool {
	var exitErr *ExitError
	return errors.As(err, &exitErr) && exitErr.Reported
}

// capitalizeFirst upper-cases the first letter of a message for display.
//
// Go error strings are lower case by convention, and the wrapped error
// keeps that form — it is what `errors.Is` callers and the HTTP layer
// see. What a person reads after "Error: " is a sentence, and rx-python
// capitalizes it, so the two backends printed the same message in two
// different cases for the same mistake.
//
// Only the first rune changes, so "rg" and other lower-case identifiers
// further into the message are left alone.
func capitalizeFirst(msg string) string {
	if msg == "" {
		return msg
	}
	first, size := utf8.DecodeRuneInString(msg)
	if !unicode.IsLower(first) {
		return msg
	}
	return string(unicode.ToUpper(first)) + msg[size:]
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

package clicommand

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"unicode"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/output"
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
//	6 → the log chain a command reads is invalid (`rx logs`)
//	7 → the log chain's files changed since the fingerprint the command
//	    was given (`rx logs … --fingerprint=`)
//
// rx-python has no log chains, so 6 and 7 are rx-go's alone.
const (
	ExitSuccess      = 0
	ExitGenericError = 1
	ExitUsageError   = 2
	ExitFileNotFound = 3
	ExitAccessDenied = 4
	ExitInterrupted  = 5
	ExitChainInvalid = 6
	ExitChainChanged = 7
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
	// SECURITY: a message can name a file whose name holds terminal
	// control sequences; they are written out, not sent to the terminal.
	_, _ = fmt.Fprintf(w, "Error: %s\n", output.PrintableMessage(capitalizeFirst(msg)))
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

// exitCodeForPathError is the exit code a failure to stat or open a path
// the user named produces when that failure is the only one: 3 for a
// path that does not exist, 4 for one the process may not read, 2 for
// one that is not a regular file (a named pipe, a socket, a device),
// which no command takes, and 1 for anything else.
func exitCodeForPathError(err error) int {
	switch {
	case errors.Is(err, paths.ErrNotRegularFile):
		return ExitUsageError
	case errors.Is(err, fs.ErrNotExist):
		return ExitFileNotFound
	case errors.Is(err, fs.ErrPermission):
		return ExitAccessDenied
	default:
		return ExitGenericError
	}
}

// accessFailureText is how a command words a failure to open or read a
// file: "permission denied" for a file the process may not read and
// "not a regular file" for a named pipe, a socket or a device, the
// words every command prints for them, and the error's own text
// otherwise.
func accessFailureText(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return paths.ReasonPermissionDenied
	case errors.Is(err, paths.ErrNotRegularFile):
		return paths.ReasonNotRegularFile
	}
	return err.Error()
}

// openFailure is the error a command returns for a file the user named
// that it cannot open: "permission denied: <path>" and the access-denied
// exit code for one the process may not read, the same in `rx trace`,
// `rx samples` and `rx index`, and the code exitCodeForPathError gives
// any other failure.
// fileTZFlagUsage is the help text of --file-tz, the same in every
// command that takes it.
const fileTZFlagUsage = "Read the file's timestamps as wall clock in this zone (UTC, an IANA name or ±HH:MM), " +
	"ignoring any zone its lines write"

// parseFileTZ reads the value of --file-tz: no zone when it is empty,
// and a usage error naming the value when it names none
// (config.ParseZone).
func parseFileTZ(value string) (config.Zone, *ExitError) {
	if value == "" {
		return config.Zone{}, nil
	}
	zone, err := config.ParseZone(value)
	if err != nil {
		return config.Zone{}, exitWithError(os.Stderr, ExitUsageError, "--file-tz=%s: %s", output.Quote(value), err.Error())
	}
	return zone, nil
}

func openFailure(path string, err error) *ExitError {
	return exitWithError(os.Stderr, exitCodeForPathError(err), "%s: %s", accessFailureText(err), path)
}

// failureSummaries is the closing error line of a multi-path command
// whose every failure had the same exit code. A code missing here, and
// a run whose failures had different codes, get the command's generic
// line instead.
var failureSummaries = map[int]string{
	ExitFileNotFound: "one or more files do not exist",
	ExitAccessDenied: "one or more files were outside the search roots or could not be read",
}

// multiPathFailure is the error a command that processes several paths
// returns after reporting each failure on its own.
//
// failureCodes holds the exit code each failed path would have produced
// alone. When they are all the same, the run exits with that code, so
// `rx index missing.log` exits 3 exactly like `rx trace x missing.log`.
// When they differ, no single code tells the whole story and the run
// exits 1. genericSummary is the line printed in that case, such as
// "one or more files failed to index". It returns nil when nothing
// failed.
func multiPathFailure(failureCodes []int, genericSummary string) error {
	return multiPathFailureWith(failureCodes, failureSummaries, genericSummary)
}

// multiPathFailureWith is multiPathFailure with the closing lines of
// one shared exit code given by the caller, for a command whose paths
// are not files.
func multiPathFailureWith(failureCodes []int, summaries map[int]string, genericSummary string) error {
	if len(failureCodes) == 0 {
		return nil
	}
	code := failureCodes[0]
	for _, other := range failureCodes[1:] {
		if other != code {
			code = ExitGenericError
			break
		}
	}
	summary, ok := summaries[code]
	if !ok {
		summary = genericSummary
	}
	return NewExitError(code, errors.New(summary))
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

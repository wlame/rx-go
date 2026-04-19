package clicommand

import (
	"fmt"
	"io"
	"os"
)

// ExitCode encodes the classical CLI-return-code convention used by rx:
//
//	0 → success
//	1 → generic error
//	2 → usage error (bad flag, missing arg)
//	3 → file not found
//	4 → permission / sandbox denied
//	5 → invalid regex
//
// These match rx-python/src/rx/cli/*.py sys.exit calls. The CLI wires
// exit codes for every terminal error path.
const (
	ExitSuccess      = 0
	ExitGenericError = 1
	ExitUsageError   = 2
	ExitFileNotFound = 3
	ExitAccessDenied = 4
	ExitInvalidRegex = 5
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

// exitWithError prints an error message to stderr (with "Error: "
// prefix) and returns the right exit code. Use in place of `os.Exit`
// from command RunE functions to let tests intercept.
func exitWithError(w io.Writer, code int, format string, args ...any) int {
	_, _ = fmt.Fprintf(w, "Error: "+format+"\n", args...)
	return code
}

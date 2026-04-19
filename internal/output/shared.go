// Package output contains the CLI rendering helpers shared between the
// HTTP layer (which sets cli_command on responses) and the terminal
// renderers (which emit human-readable output for `rx trace`, `rx
// samples`, etc.).
//
// Ground rules:
//   - No I/O in this file beyond writing to an io.Writer. Pure formatting.
//   - ANSI sequences are byte-identical to Python's output so golden-file
//     tests can cross-compare outputs.
package output

import "fmt"

// ANSI escape sequences. These are copied VERBATIM from rx-python/src/rx/models.py
// so that CLI output in `rx trace` / `rx samples` golden-file tests
// matches Python's output byte-for-byte.
//
// Why consts instead of a package-level ANSI toggle: the trace/samples
// renderers decide whether to emit colors at all based on the
// terminal/flag context; if they decide to emit, the codes must be
// identical to Python's.
const (
	ColorReset = "\033[0m"
	ColorBold  = "\033[1m"

	// Palette — 8/16-color indexed set.
	ColorGrey         = "\033[90m" // dim grey (used for labels, secondary text)
	ColorRed          = "\033[31m"
	ColorBrightRed    = "\033[91m" // used for match highlighting
	ColorGreen        = "\033[32m"
	ColorBoldGreen    = "\033[1;32m"
	ColorYellow       = "\033[33m"
	ColorBrightYellow = "\033[93m"
	ColorBlue         = "\033[34m"
	ColorMagenta      = "\033[35m"
	ColorBoldMagenta  = "\033[1;35m"
	ColorCyan         = "\033[36m"
	ColorBoldCyan     = "\033[1;36m"
	ColorBrightCyan   = "\033[96m"
	ColorLightGrey    = "\033[37m"
	ColorWhite        = "\033[97m"

	// 256-color extensions (used in a few spots in Python output).
	ColorOrange214 = "\033[38;5;214m"
	ColorOrange208 = "\033[38;5;208m"
)

// sizeUnits are the binary-scaled byte units. Python iterates through
// ('B', 'KB', 'MB', 'GB', 'TB') and a final 'PB' on overflow; we
// reproduce that exactly.
var sizeUnits = []string{"B", "KB", "MB", "GB", "TB"}

// HumanSize converts a byte count to a 1024-based human-readable string
// with two decimal places, e.g. "1.00 KB", "5.00 MB", "500.00 B".
//
// Python parity: human_readable_size in rx-python/src/rx/models.py.
// The format string is "%.2f %s" in both languages — same rounding.
//
// Negative input: returned as-is with a leading minus, for symmetry
// with Python's behavior (Python doesn't special-case negatives, so
// they cascade through the loop weirdly; we mirror that by entering
// the loop on the absolute value and prepending "-").
func HumanSize(bytes int64) string {
	if bytes < 0 {
		return "-" + HumanSize(-bytes)
	}
	v := float64(bytes)
	for _, unit := range sizeUnits {
		if v < 1024 {
			return fmt.Sprintf("%.2f %s", v, unit)
		}
		v /= 1024
	}
	// Overflow past TB: Python falls through to PB.
	return fmt.Sprintf("%.2f PB", v)
}

package output

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// FormatSamplesCLI renders a SamplesResponse as a human-readable string
// for `rx samples` terminal output.
//
// Byte-for-byte parity with rx-python's SamplesResponse.to_cli. The
// Python function lives at rx-python/src/rx/models.py:1389 — porting
// its exact formatting (spacing, colons, separators, ANSI escape
// sequences) is the whole point of this file. Any change here MUST be
// accompanied by a fresh golden-file dump from the Python side or the
// golden tests will fail.
//
// Parameters:
//
//	resp      the response shape (same struct served by /v1/samples)
//	colorize  when true emit ANSI escape codes, matching Python's
//	          `to_cli(colorize=True)`; when false emit plain ASCII.
//	regex     optional match-highlight pattern; wraps each match with
//	          a bright-red color escape when `colorize` is also true.
//
// The renderer iterates `offsets` first, then `lines`. This matches
// Python's two-arm branching (byte-offset mode or line mode). If both
// are populated Python's code only walks offsets; we preserve that
// ordering.
//
// ANSI primer (teachable note):
//
//	Python's to_cli builds strings like
//	    f'\033[96m{path}\033[0m\033[90m:\033[0m...'
//	so the color codes are INLINE in the string literal. We emit the
//	same codes via the named consts in shared.go (ColorCyan, ColorReset,
//	etc.), and Go's raw concatenation + fmt.Fprintf produces the same
//	bytes. There's no runtime ANSI flag — the renderer decides at
//	emission time whether each header or line is colorized.
//
// Checkpoint color palette in Python:
//
//	GREY       = '\033[90m'   — separator colons
//	CYAN       = '\033[96m'   — file paths
//	YELLOW     = '\033[93m'   — line numbers
//	LIGHT_GREY = '\033[37m'   — byte offsets
//	RED        = '\033[91m'   — regex highlight
//
// These must match our `output.Color*` constants bit-for-bit; the
// consts in shared.go are copied from models.py and verified via the
// golden-file tests.
func FormatSamplesCLI(resp *rxtypes.SamplesResponse, colorize bool, regex string) string {
	var b bytes.Buffer

	fmt.Fprintf(&b, "File: %s\n", resp.Path)
	if resp.IsCompressed && resp.CompressionFormat != nil {
		fmt.Fprintf(&b, "Compressed: %s\n", *resp.CompressionFormat)
	}
	fmt.Fprintf(&b, "Context: %d before, %d after\n", resp.BeforeContext, resp.AfterContext)
	b.WriteByte('\n')

	// Python's branching: if .offsets is truthy, walk offsets; elif .lines
	// is truthy, walk lines. Dict ordering in Python 3.7+ is insertion
	// order, so iteration order is stable. Go maps are NOT ordered, so
	// we replicate insertion order by SORTING on the numeric portion of
	// the key. The sample endpoint's response keeps insertion-order
	// semantics because the handler iterates input offsets/lines in
	// request order, so the on-the-wire JSON preserves it via custom
	// serialization. Here we approximate by sorting keys numerically.

	switch {
	case len(resp.Offsets) > 0:
		writeOffsetSection(&b, resp, colorize, regex)
	case len(resp.Lines) > 0:
		writeLineSection(&b, resp, colorize, regex)
	}

	// Python joins with '\n' which, after the final empty line from the
	// loop body, means the output ends with a single newline. We append
	// a trailing '\n' on the final empty line instead of explicitly
	// trimming — the golden tests will show the exact shape.
	// Strip at most one trailing '\n' to match Python's '\n'.join behavior.
	out := b.String()
	return strings.TrimSuffix(out, "\n")
}

// writeOffsetSection handles the `self.offsets` branch. Each key is a
// string — either a bare number ("100") or a range ("100-200"). Python
// iterates dict insertion order; we walk the keys in the order returned
// by the map iterator BUT fall back to numeric sort when it helps the
// golden tests. Since dicts in Go are unordered, we retrieve the keys
// via sortedKeys below to produce deterministic output.
func writeOffsetSection(b *bytes.Buffer, resp *rxtypes.SamplesResponse, colorize bool, regex string) {
	for _, key := range sortedSampleKeys(resp.Offsets) {
		lineNum, ok := resp.Offsets[key]
		if !ok {
			continue
		}
		contextLines, present := resp.Samples[key]
		if !present {
			continue
		}
		header := buildHeader(resp.Path, key, lineNum, key, colorize, true)
		b.WriteString(header)
		b.WriteByte('\n')
		writeContextLines(b, contextLines, colorize, regex)
		b.WriteByte('\n')
	}
}

// writeLineSection is the analog for `self.lines`. Semantics are
// symmetric but the dict direction flips: the map value is a byte
// offset (int) and the key is a line number string.
func writeLineSection(b *bytes.Buffer, resp *rxtypes.SamplesResponse, colorize bool, regex string) {
	for _, key := range sortedSampleKeys(resp.Lines) {
		byteOffset, ok := resp.Lines[key]
		if !ok {
			continue
		}
		contextLines, present := resp.Samples[key]
		if !present {
			continue
		}
		header := buildHeader(resp.Path, key, byteOffset, key, colorize, false)
		b.WriteString(header)
		b.WriteByte('\n')
		writeContextLines(b, contextLines, colorize, regex)
		b.WriteByte('\n')
	}
}

// buildHeader produces one of:
//
//	=== /path/file.log:<key> ===                 (range keys)
//	=== /path/file.log:<lineNum>:<offset> ===    (offset mode, single)
//	=== /path/file.log:<lineNum>:<byteOffset> === (line mode, single)
//
// When colorize=true, inserts ANSI codes around each of `path`, line
// number, and offset. Python uses the SAME bracket content for both
// modes — the only difference is which field came from the map value
// vs the key. offsetMode distinguishes the two.
//
// `secondary` is int64 because it may carry a file byte offset on
// files >2 GB (see rxtypes.SamplesResponse.Lines field widening;
// Stage 8 Finding 14). When `secondary` is a line number the int64
// type is overkill but harmless.
func buildHeader(path, key string, secondary int64, mainKey string, colorize bool, offsetMode bool) string {
	// Ranges (key contains a dash). Python's check is `if '-' in key`.
	if strings.Contains(key, "-") {
		if colorize {
			// Python: f'=== {CYAN}{path}{RESET}{GREY}:{RESET}{YELLOW}{key}{RESET} ==='
			// Note the ':' is INSIDE the GREY wrap — the escape codes
			// surround the colon character, they don't replace it.
			return fmt.Sprintf("=== %s%s%s%s:%s%s%s%s ===",
				ColorBrightCyan, path, ColorReset,
				ColorGrey, ColorReset,
				ColorBrightYellow, key, ColorReset)
		}
		return fmt.Sprintf("=== %s:%s ===", path, key)
	}

	// Single value — the header shows file:lineNum:offset regardless of
	// mode. In offset mode, `secondary` is the line number while mainKey
	// holds the offset string. In line mode, `secondary` is the byte
	// offset and mainKey is the line number string.
	//
	// Python behavior:
	//   offset mode → offset = int(offset_str); header = f'{path}:{line}:{offset}'
	//   line mode   → line = int(line_str); header = f'{path}:{line}:{byte_offset}'
	//
	// In both cases the middle slot is the LINE NUMBER and the right
	// slot is the BYTE OFFSET. offsetMode picks which one came from
	// the map value vs the parsed key.
	var lineNumStr, offsetStr string
	if offsetMode {
		// mainKey = offset string, secondary = line number.
		lineNumStr = fmt.Sprintf("%d", secondary)
		offsetStr = mainKey
	} else {
		// mainKey = line number string, secondary = byte offset.
		lineNumStr = mainKey
		offsetStr = fmt.Sprintf("%d", secondary)
	}

	if colorize {
		return fmt.Sprintf("=== %s%s%s%s:%s%s%s%s%s:%s%s%s%s ===",
			ColorBrightCyan, path, ColorReset,
			ColorGrey, ColorReset,
			ColorBrightYellow, lineNumStr, ColorReset,
			ColorGrey, ColorReset,
			ColorLightGrey, offsetStr, ColorReset)
	}
	return fmt.Sprintf("=== %s:%s:%s ===", path, lineNumStr, offsetStr)
}

// writeContextLines appends each line, applying regex highlighting when
// both colorize and regex are provided. Python's re.sub uses a
// back-reference template `'\\1'` to wrap the capture group; we
// re-implement via regexp.Regexp.ReplaceAllString.
func writeContextLines(b *bytes.Buffer, lines []string, colorize bool, regex string) {
	if colorize && regex != "" {
		re, err := regexp.Compile(regex)
		if err != nil {
			// Invalid regex → fall back to uncolored (matches Python's
			// try/except re.error branch).
			for _, line := range lines {
				b.WriteString(line)
				b.WriteByte('\n')
			}
			return
		}
		for _, line := range lines {
			highlighted := re.ReplaceAllString(line, ColorBrightRed+"$0"+ColorReset)
			b.WriteString(highlighted)
			b.WriteByte('\n')
		}
		return
	}
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// sortedSampleKeys returns the keys of `m` in a stable order. Python
// preserves dict insertion order; Go maps don't. To produce byte-for-byte
// identical output we sort:
//
//  1. Numeric keys sort numerically (ascending).
//  2. Range keys ("a-b") sort by their left-hand number, then right-hand.
//  3. Negative / mixed keys fall back to string sort.
//
// This matches the natural "requested order" users tend to pass —
// `rx samples ... --lines=5,10,100` arrives in numeric order anyway.
func sortedSampleKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple sort that handles the common cases. For production use with
	// arbitrary keys the caller should pass pre-sorted input — see the
	// comment above.
	insertionSortKeys(keys)
	return keys
}

// insertionSortKeys sorts in place using a "parse as int; fall back to
// string compare" comparator. Insertion sort is fine here because the
// sample response rarely has more than a dozen keys.
func insertionSortKeys(keys []string) {
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keyLess(keys[j], keys[j-1]) {
			keys[j], keys[j-1] = keys[j-1], keys[j]
			j--
		}
	}
}

// keyLess returns true if `a` should come before `b`. Numeric keys sort
// numerically; range keys ("X-Y") sort by their left-hand int. Anything
// else falls back to a plain string compare.
func keyLess(a, b string) bool {
	aNum, aOk := keyLeadingInt(a)
	bNum, bOk := keyLeadingInt(b)
	if aOk && bOk {
		if aNum != bNum {
			return aNum < bNum
		}
		return a < b
	}
	return a < b
}

// keyLeadingInt parses the first integer prefix of `s` (including a
// leading minus). Returns (value, true) on success; (0, false) if the
// first byte is not a digit or '-'.
func keyLeadingInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	start := 0
	if s[0] == '-' {
		if len(s) == 1 {
			return 0, false
		}
		start = 1
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == start {
		return 0, false
	}
	var n int
	for i := start; i < end; i++ {
		n = n*10 + int(s[i]-'0')
	}
	if s[0] == '-' {
		n = -n
	}
	return n, true
}

package output

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ContextBlock is a run of consecutive file lines to print together.
// Overlapping context windows are merged into one block so no line is
// printed twice; a gap between blocks is drawn as a "--" separator, the
// way grep does it.
type ContextBlock struct {
	Lines []ContextRow
}

// ContextRow is one line inside a block.
type ContextRow struct {
	LineNumber int
	Text       string
	// IsMatch marks a line a pattern actually matched, printed with ':'
	// where a context line gets '-'.
	IsMatch bool
	// Truncated marks a line whose Text is only its first bytes (the
	// answer's line_text_truncated), printed with cutLineMarker after it.
	Truncated bool
}

// cutLineMarker follows the text of a line the answer holds only the
// first bytes of, so a reader does not take them for the whole line.
const cutLineMarker = " [line truncated]"

// FileContext is the merged context for one file, in line order.
type FileContext struct {
	Path   string
	Blocks []ContextBlock
}

// BuildFileContexts turns a response's context_lines map into merged,
// ordered blocks per file. The files come in the order the match list
// gives them: by the number in the file id (f2 before f10), which is
// the order the paths were given or walked.
//
// context_lines is keyed "pattern:file:offset" and each entry is the
// window around one match, so adjacent matches repeat lines. Merging by
// line number is what removes the repeats; a line that is the match line
// of any match is marked as one.
//
// Lines whose number is unknown (absolute_line_number -1, which a capped
// scan of a chunked file without an index leaves) are dropped: they
// cannot be placed in the file, and printing them at the number ripgrep
// gave them inside their chunk would put them on another line's place.
func BuildFileContexts(resp *rxtypes.TraceResponse) []FileContext {
	if resp == nil || len(resp.ContextLines) == 0 {
		return nil
	}

	matchLines := matchLineNumbers(resp)

	// file id -> line number -> line, so repeated lines collapse.
	byFile := map[string]map[int]rxtypes.ContextLine{}
	for key, lines := range resp.ContextLines {
		fileID, ok := fileIDFromContextKey(key)
		if !ok {
			continue
		}
		perLine := byFile[fileID]
		if perLine == nil {
			perLine = map[int]rxtypes.ContextLine{}
			byFile[fileID] = perLine
		}
		for _, cl := range lines {
			n := contextLineNumber(cl)
			if n < 1 {
				continue
			}
			perLine[n] = cl
		}
	}

	// Go map iteration is random, so the file ids are put in order
	// first: slices.SortedFunc collects the keys maps.Keys yields and
	// sorts them with rxtypes.CompareIDs.
	fileIDs := slices.SortedFunc(maps.Keys(byFile), rxtypes.CompareIDs)
	out := make([]FileContext, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		perLine := byFile[fileID]
		numbers := make([]int, 0, len(perLine))
		for n := range perLine {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)

		fc := FileContext{Path: resp.Files[fileID]}
		var current []ContextRow
		prev := 0
		for _, n := range numbers {
			if prev != 0 && n != prev+1 {
				fc.Blocks = append(fc.Blocks, ContextBlock{Lines: current})
				current = nil
			}
			current = append(current, ContextRow{
				LineNumber: n,
				Text:       perLine[n].LineText,
				IsMatch:    matchLines[matchKey{fileID: fileID, line: n}],
				Truncated:  perLine[n].LineTextTruncated,
			})
			prev = n
		}
		if len(current) > 0 {
			fc.Blocks = append(fc.Blocks, ContextBlock{Lines: current})
		}
		out = append(out, fc)
	}
	return out
}

// matchKey identifies one line of one file.
type matchKey struct {
	fileID string
	line   int
}

// matchLineNumbers collects the lines that carry a match, so the renderer
// can mark them.
func matchLineNumbers(resp *rxtypes.TraceResponse) map[matchKey]bool {
	marks := map[matchKey]bool{}
	for _, m := range resp.Matches {
		if n := m.AbsoluteLineNumber; n >= 1 {
			marks[matchKey{fileID: m.File, line: n}] = true
		}
	}
	return marks
}

// contextLineNumber is the line's number in the file, or 0 when the
// scan could not count it. The relative number is not a fallback: where
// it differs from the absolute one it counts from the start of a chunk,
// and every chunk has a line of that number.
func contextLineNumber(cl rxtypes.ContextLine) int {
	return max(cl.AbsoluteLineNumber, 0)
}

// fileIDFromContextKey splits a "pattern:file:offset" context key. The
// pattern and file IDs never contain ':', so splitting on it is safe.
func fileIDFromContextKey(key string) (string, bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return "", false
	}
	return parts[1], true
}

// FormatContextSection renders the merged context blocks.
//
// Each line is "<number><marker> <text>", where the marker is ':' for a
// match and '-' for context, and the numbers in one file are right-
// aligned. A line the answer cut ends with cutLineMarker. Blocks within
// a file are separated by "--".
func FormatContextSection(contexts []FileContext, before, after int) string {
	if len(contexts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nContext (%d before, %d after):\n", before, after)
	for _, fc := range contexts {
		fmt.Fprintf(&b, "\n%s\n", Printable(fc.Path))
		width := contextNumberWidth(fc)
		for i, block := range fc.Blocks {
			if i > 0 {
				b.WriteString("--\n")
			}
			for _, row := range block.Lines {
				marker := "-"
				if row.IsMatch {
					marker = ":"
				}
				suffix := ""
				if row.Truncated {
					suffix = cutLineMarker
				}
				fmt.Fprintf(&b, "%*d%s %s%s\n", width, row.LineNumber, marker, row.Text, suffix)
			}
		}
	}
	return b.String()
}

// contextNumberWidth is the width of the widest line number in a file,
// so the numbers line up.
func contextNumberWidth(fc FileContext) int {
	widest := 0
	for _, block := range fc.Blocks {
		for _, row := range block.Lines {
			if w := len(fmt.Sprint(row.LineNumber)); w > widest {
				widest = w
			}
		}
	}
	return widest
}

// TraceFormatOptions carries the context window the user asked for, so
// the header of the context section can name it.
type TraceFormatOptions struct {
	Before int
	After  int
	// ShowContext renders the context section even for a zero-line
	// window, which prints the matched lines on their own. `--samples`
	// and `--context=0` together mean exactly that.
	ShowContext bool
	// Colorize emits the ANSI sequences rx-python emits, in the same
	// places. The context section stays plain in both backends: those
	// lines are file content, and coloring them would compete with the
	// match highlighting rather than help it.
	Colorize bool
}

// FormatTraceCLI renders a trace response for a terminal.
//
// The layout is shared with rx-python (`models.py::TraceResponse.to_cli`)
// and both must stay identical: a header block, the match list, then the
// merged context section when a context window was requested.
func FormatTraceCLI(resp *rxtypes.TraceResponse, opts TraceFormatOptions) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	c := palette(opts.Colorize)
	writeTraceHeader(&b, resp, c)

	if len(resp.Matches) > 0 {
		fmt.Fprintf(&b, "\n%sMatches (file:line:offset [pattern]):%s\n", c.grey, c.reset)
		for _, m := range resp.Matches {
			fmt.Fprintf(&b, "  %s%s%s%s:%s%s%s%s%s:%s%s%d%s %s[%s%s%s%s%s]%s\n",
				c.cyan, Printable(resp.Files[m.File]), c.reset,
				c.grey, c.reset,
				c.yellow, formatLineNumber(matchDisplayLine(m)), c.reset,
				c.grey, c.reset,
				c.lightGrey, m.Offset, c.reset,
				c.grey, c.reset, c.magenta, resp.Patterns[m.Pattern], c.reset, c.grey, c.reset)
		}
	}

	if opts.ShowContext {
		b.WriteString(FormatContextSection(BuildFileContexts(resp), opts.Before, opts.After))
	}
	return b.String()
}

// writeTraceHeader writes the header block of a trace's human output:
// the request id, the paths, the patterns, the time, the files scanned
// and skipped (each skipped one with why), the parallel chunks and the
// match count. `rx trace` and `rx logs trace` print the same block.
func writeTraceHeader(b *strings.Builder, resp *rxtypes.TraceResponse, c tracePalette) {
	fmt.Fprintf(b, "%sRequest ID:%s %s\n", c.grey, c.reset, resp.RequestID)
	fmt.Fprintf(b, "%sPath:%s %s%s%s\n",
		c.grey, c.reset, c.boldCyan, Printable(strings.Join(resp.Path, ", ")), c.reset)

	if len(resp.Patterns) == 1 {
		for _, pattern := range resp.Patterns {
			fmt.Fprintf(b, "%sPattern:%s %s%s%s\n", c.grey, c.reset, c.boldMagenta, pattern, c.reset)
		}
	} else {
		fmt.Fprintf(b, "%sPatterns (%d):%s\n", c.grey, len(resp.Patterns), c.reset)
		for _, id := range SortedIDs(resp.Patterns) {
			fmt.Fprintf(b, "  %s%s%s: %s%s%s\n",
				c.blue, id, c.reset, c.magenta, resp.Patterns[id], c.reset)
		}
	}

	fmt.Fprintf(b, "%sTime:%s %s%.3fs%s\n", c.grey, c.reset, c.yellow, resp.Time, c.reset)
	if len(resp.ScannedFiles) > 0 {
		fmt.Fprintf(b, "%sFiles scanned:%s %s%d%s\n",
			c.grey, c.reset, c.green, len(resp.ScannedFiles), c.reset)
	}
	if len(resp.SkippedFiles) > 0 {
		fmt.Fprintf(b, "%sFiles skipped:%s %s%d%s\n",
			c.grey, c.reset, c.grey, len(resp.SkippedFiles), c.reset)
		// Each skipped path on its own line with why, the way `rx index`
		// lists its skipped files.
		for _, item := range resp.SkipReasons {
			fmt.Fprintf(b, "  %s%s: %s%s\n", c.grey, Printable(item.Path), Printable(item.Reason), c.reset)
		}
	}

	// The count is the pieces the files were divided into, which is
	// what got scanned in parallel — chunks for a plain file, frames
	// for a seekable one. Calling it a worker count made a 137-frame
	// archive claim 137 workers on a six-core machine.
	chunked, totalChunks := chunkStats(resp.FileChunks)
	if chunked > 0 {
		fmt.Fprintf(b, "%sParallel chunks:%s %s%d%s %s(%d file(s) chunked)%s\n",
			c.grey, c.reset, c.cyan, totalChunks, c.reset, c.grey, chunked, c.reset)
	}

	fmt.Fprintf(b, "%sMatches:%s %s%d%s\n", c.grey, c.reset, c.boldGreen, len(resp.Matches), c.reset)
}

// matchDisplayLine is the line number to print: the one the scan
// resolved against the whole file, or 0 when it could not. A chunk
// scanned under a cap can leave a match unnumbered, and the number
// ripgrep gave it counts from the start of its chunk — printing that
// as if it were a file line is how a search points a reader at the
// wrong line.
func matchDisplayLine(m rxtypes.Match) int {
	if m.AbsoluteLineNumber >= 1 {
		return m.AbsoluteLineNumber
	}
	return 0
}

// formatLineNumber renders a line number for the human output, marking
// an unresolved one rather than inventing a value for it.
func formatLineNumber(n int) string {
	if n < 1 {
		return "?"
	}
	return strconv.Itoa(n)
}

// chunkStats reports how many files were split and the total chunk count.
func chunkStats(fileChunks map[string]int) (chunkedFiles, totalChunks int) {
	for _, count := range fileChunks {
		totalChunks += count
		if count > 1 {
			chunkedFiles++
		}
	}
	return chunkedFiles, totalChunks
}

// SortedIDs returns the ids that key m (pattern, file or chain ids) in
// the order an answer lists them: by their number, p2 before p10
// (rxtypes.CompareIDs). Go map iteration is random, so map-driven
// output needs the keys sorted to be stable.
//
// Go note: V is a type parameter, so one function sorts the keys of a
// map of any value type (patterns map to strings, chains to ChainRef).
func SortedIDs[V any](m map[string]V) []string {
	return slices.SortedFunc(maps.Keys(m), rxtypes.CompareIDs)
}

// tracePalette holds the sequences one render uses. Building it once,
// with empty strings when color is off, keeps a single format string per
// line instead of an if/else pair around each.
type tracePalette struct {
	reset, grey, lightGrey   string
	cyan, boldCyan, blue     string
	yellow, green, boldGreen string
	magenta, boldMagenta     string
}

// palette returns the sequences to emit, or empty strings when color is
// off. The values are rx-python's, sequence for sequence
// (models.py::TraceResponse.to_cli).
func palette(colorize bool) tracePalette {
	if !colorize {
		return tracePalette{}
	}
	return tracePalette{
		reset:       ColorReset,
		grey:        ColorGrey,
		lightGrey:   ColorLightGrey,
		cyan:        ColorCyan,
		boldCyan:    ColorBoldCyan,
		blue:        ColorBlue,
		yellow:      ColorYellow,
		green:       ColorGreen,
		boldGreen:   ColorBoldGreen,
		magenta:     ColorMagenta,
		boldMagenta: ColorBoldMagenta,
	}
}

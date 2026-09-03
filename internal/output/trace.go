package output

import (
	"fmt"
	"sort"
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
}

// FileContext is the merged context for one file, in line order.
type FileContext struct {
	Path   string
	Blocks []ContextBlock
}

// BuildFileContexts turns a response's context_lines map into merged,
// ordered blocks per file.
//
// context_lines is keyed "pattern:file:offset" and each entry is the
// window around one match, so adjacent matches repeat lines. Merging by
// line number is what removes the repeats; a line that is the match line
// of any match is marked as one.
//
// Lines whose number is unknown (-1, which happens on a chunk-scanned
// file without an index) are dropped: they cannot be placed in the file
// and printing them in the wrong order would be worse than omitting them.
func BuildFileContexts(resp *rxtypes.TraceResponse) []FileContext {
	if resp == nil || len(resp.ContextLines) == 0 {
		return nil
	}

	matchLines := matchLineNumbers(resp)

	// file id -> line number -> text, so repeated lines collapse.
	byFile := map[string]map[int]string{}
	for key, lines := range resp.ContextLines {
		fileID, ok := fileIDFromContextKey(key)
		if !ok {
			continue
		}
		perLine := byFile[fileID]
		if perLine == nil {
			perLine = map[int]string{}
			byFile[fileID] = perLine
		}
		for _, cl := range lines {
			n := contextLineNumber(cl)
			if n < 1 {
				continue
			}
			perLine[n] = cl.LineText
		}
	}

	out := make([]FileContext, 0, len(byFile))
	for fileID, perLine := range byFile {
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
				Text:       perLine[n],
				IsMatch:    matchLines[matchKey{fileID: fileID, line: n}],
			})
			prev = n
		}
		if len(current) > 0 {
			fc.Blocks = append(fc.Blocks, ContextBlock{Lines: current})
		}
		out = append(out, fc)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
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
		n := m.AbsoluteLineNumber
		if n < 1 && m.RelativeLineNumber != nil {
			n = *m.RelativeLineNumber
		}
		if n >= 1 {
			marks[matchKey{fileID: m.File, line: n}] = true
		}
	}
	return marks
}

// contextLineNumber prefers the absolute line number and falls back to
// the chunk-relative one, which is the same thing on an unchunked file.
func contextLineNumber(cl rxtypes.ContextLine) int {
	if cl.AbsoluteLineNumber >= 1 {
		return cl.AbsoluteLineNumber
	}
	return cl.RelativeLineNumber
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
// aligned. Blocks within a file are separated by "--".
func FormatContextSection(contexts []FileContext, before, after int) string {
	if len(contexts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nContext (%d before, %d after):\n", before, after)
	for _, fc := range contexts {
		fmt.Fprintf(&b, "\n%s\n", fc.Path)
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
				fmt.Fprintf(&b, "%*d%s %s\n", width, row.LineNumber, marker, row.Text)
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

	fmt.Fprintf(&b, "Request ID: %s\n", resp.RequestID)
	fmt.Fprintf(&b, "Path: %s\n", strings.Join(resp.Path, ", "))

	if len(resp.Patterns) == 1 {
		for _, pattern := range resp.Patterns {
			fmt.Fprintf(&b, "Pattern: %s\n", pattern)
		}
	} else {
		fmt.Fprintf(&b, "Patterns (%d):\n", len(resp.Patterns))
		for _, id := range sortedKeys(resp.Patterns) {
			fmt.Fprintf(&b, "  %s: %s\n", id, resp.Patterns[id])
		}
	}

	fmt.Fprintf(&b, "Time: %.3fs\n", resp.Time)
	if len(resp.ScannedFiles) > 0 {
		fmt.Fprintf(&b, "Files scanned: %d\n", len(resp.ScannedFiles))
	}
	if len(resp.SkippedFiles) > 0 {
		fmt.Fprintf(&b, "Files skipped: %d\n", len(resp.SkippedFiles))
	}

	chunked, totalChunks := chunkStats(resp.FileChunks)
	if chunked > 0 {
		fmt.Fprintf(&b, "Parallel workers: %d (%d file(s) chunked)\n", totalChunks, chunked)
	}

	fmt.Fprintf(&b, "Matches: %d\n", len(resp.Matches))

	if len(resp.Matches) > 0 {
		b.WriteString("\nMatches (file:line:offset [pattern]):\n")
		for _, m := range resp.Matches {
			fmt.Fprintf(&b, "  %s:%d:%d [%s]\n",
				resp.Files[m.File], matchDisplayLine(m), m.Offset, resp.Patterns[m.Pattern])
		}
	}

	if opts.ShowContext {
		b.WriteString(FormatContextSection(BuildFileContexts(resp), opts.Before, opts.After))
	}
	return b.String()
}

// matchDisplayLine is the line number to print: the absolute one when the
// backend knows it, otherwise the chunk-relative one, otherwise -1.
// rx-python's to_cli picks the same way.
func matchDisplayLine(m rxtypes.Match) int {
	if m.AbsoluteLineNumber != -1 {
		return m.AbsoluteLineNumber
	}
	if m.RelativeLineNumber != nil {
		return *m.RelativeLineNumber
	}
	return -1
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

// sortedKeys keeps map-driven output stable; Go map iteration is random.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package output

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// maxFindingLines is the most lines of one finding the human output
// prints; the rest is one line saying how many are left and the
// `rx samples` command that prints them.
const maxFindingLines = 20

// FindingsFormatOptions are the settings of FormatFindingsCLI.
type FindingsFormatOptions struct {
	// Order is the selection's signature ids in catalogue order: the
	// order of the counts in the count line (the answer's signatures
	// member is a map and has none).
	Order []string
	// Colorize colors each finding's place, its signature id and its
	// level (levelColors).
	Colorize bool
}

// levelColors is the color of each level's signature id and level name
// in colored output. A level not listed stays plain.
var levelColors = map[string]string{
	"info":     ColorGrey,
	"low":      ColorBlue,
	"medium":   ColorYellow,
	"high":     ColorRed,
	"critical": ColorBrightRed,
}

// FormatFindingsCLI renders the findings of a trace answer for a
// terminal, after the matches part: a count line ("Findings: N in M
// files (id count, …)"), then each finding in the answer's order as
// `path:start[-end]  id  group · level` and its lines, numbered, with
// ':' after the number of the finding's own lines and '-' after the
// lines asked for around it (-B, -A, --context), as the context section
// marks matches and context. A finding longer than maxFindingLines
// lines prints that many and the `rx samples` command for the rest; a
// finding whose text the budget left out says so and gives the same
// command. A trace without signatures renders nothing.
//
// The lines are the file's content, printed as they are, as the
// context section prints them; the text arrives masked. Paths go
// through Printable.
func FormatFindingsCLI(resp *rxtypes.TraceResponse, opts FindingsFormatOptions) string {
	if resp == nil || resp.Signatures == nil {
		return ""
	}
	c := palette(opts.Colorize)
	var b strings.Builder
	b.WriteString("\n" + findingsCountLine(resp, opts.Order) + "\n")
	for i := range resp.Findings {
		f := &resp.Findings[i]
		b.WriteString("\n")
		writeFinding(&b, resp, f, opts.Colorize, c)
	}
	return b.String()
}

// findingsCountLine is "Findings: N in M files (id count, …)": N every
// finding of every file, those past the cap included, M the files that
// hold one, and each signature that found something with its count, in
// selection order, saying how many are listed when the cap kept fewer.
func findingsCountLine(resp *rxtypes.TraceResponse, order []string) string {
	counts := map[string]rxtypes.FindingCount{}
	var total int64
	files := 0
	for _, summary := range resp.FindingSummary {
		var inFile int64
		for id, n := range summary.Signatures {
			sum := counts[id]
			sum.Count += n.Count
			sum.Stored += n.Stored
			counts[id] = sum
			inFile += n.Count
		}
		total += inFile
		if inFile > 0 {
			files++
		}
	}
	if total == 0 {
		return "Findings: 0"
	}
	var parts []string
	for _, id := range order {
		n := counts[id]
		if n.Count == 0 {
			continue
		}
		part := id + " " + strconv.FormatInt(n.Count, 10)
		if int64(n.Stored) < n.Count {
			part += fmt.Sprintf(", first %d listed", n.Stored)
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("Findings: %d in %s (%s)", total, plural(files, "file"), strings.Join(parts, ", "))
}

// writeFinding writes one finding: its header line, then its lines.
func writeFinding(b *strings.Builder, resp *rxtypes.TraceResponse, f *rxtypes.Finding, colorize bool, c tracePalette) {
	path := resp.Files[f.File]
	ref := resp.Signatures[f.Signature]
	level := ""
	if colorize {
		level = levelColors[ref.Level]
	}
	reset := ""
	if level != "" {
		reset = ColorReset
	}
	place := strconv.FormatInt(f.StartLine, 10)
	if f.EndLine != f.StartLine {
		place += "-" + strconv.FormatInt(f.EndLine, 10)
	}
	fmt.Fprintf(b, "%s%s%s:%s%s%s  %s%s%s  %s · %s%s%s\n",
		c.cyan, Printable(path), c.reset, c.yellow, place, c.reset,
		level, f.Signature, reset, ref.Group, level, ref.Level, reset)
	if f.LinesTruncated {
		fmt.Fprintf(b, "  (text left out: the answer's text budget, RX_FINDINGS_TEXT_MAX_BYTES, ran out; %s)\n",
			samplesCommand(path, f.StartLine, f.EndLine))
		return
	}
	width := len(strconv.FormatInt(f.EndLine+int64(len(f.After)), 10))
	number := func(n int64, marker string, text string) {
		fmt.Fprintf(b, "  %*d%s %s\n", width, n, marker, text)
	}
	for i, text := range f.Before {
		number(f.StartLine-int64(len(f.Before)-i), "-", text)
	}
	shown := min(len(f.Lines), maxFindingLines)
	for i, text := range f.Lines[:shown] {
		number(f.StartLine+int64(i), ":", text)
	}
	if rest := int64(len(f.Lines) - shown); rest > 0 {
		first := f.StartLine + int64(shown)
		fmt.Fprintf(b, "  … %d more lines: %s\n", rest, samplesCommand(path, first, f.EndLine))
	}
	for i, text := range f.After {
		number(f.EndLine+1+int64(i), "-", text)
	}
}

// samplesCommand is the `rx samples` command that prints lines first to
// last of path, the path quoted for a shell.
func samplesCommand(path string, first, last int64) string {
	return fmt.Sprintf("rx samples %s --lines=%d-%d", Quote(path), first, last)
}

package output

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ChainTraceView is a chain search's answer as a trace answer: the same
// fields, each match without its place in a chain. The header and the
// context section of `rx logs trace` are those of `rx trace`, made from
// it.
func ChainTraceView(resp *rxtypes.ChainTraceResponse) *rxtypes.TraceResponse {
	matches := make([]rxtypes.Match, 0, len(resp.Matches))
	for _, m := range resp.Matches {
		matches = append(matches, m.Match())
	}
	return &rxtypes.TraceResponse{
		RequestID: resp.RequestID, Path: resp.Path, Time: resp.Time, Patterns: resp.Patterns,
		Files: resp.Files, Matches: matches, ScannedFiles: resp.ScannedFiles, SkippedFiles: resp.SkippedFiles,
		SkipReasons: resp.SkipReasons, MaxResults: resp.MaxResults, FileChunks: resp.FileChunks,
		ContextLines: resp.ContextLines, BeforeContext: resp.BeforeContext, AfterContext: resp.AfterContext,
		CLICommand: resp.CLICommand,
	}
}

// FormatChainTraceCLI renders a chain search for a terminal: the header
// `rx trace` prints, then one row per match with its text, then the
// context section when a context window was asked for.
//
// A match in a part of a chain reads `<handle>:<chain line>
// (<part>:<line>): <text>`, its global line in the chain first and the
// part's own name and line in brackets; `?` stands for a number that is
// not known (the chain is not ready, or the scan left the line
// unnumbered). A match in a file searched on its own reads
// `<path>:<line>: <text>`. With several patterns, each row names its
// pattern in square brackets before the text. Paths are escaped
// (Printable); the text is printed as the file holds it.
//
//	/var/log/syslog:123456 (syslog.3.gz:500): Oct  3 14:00:01 host sshd[812]: Accepted …
//	/var/log/syslog:? (syslog.3.gz:500): …
//	/var/log/notes.txt:12: …
func FormatChainTraceCLI(resp *rxtypes.ChainTraceResponse, opts TraceFormatOptions) string {
	if resp == nil {
		return ""
	}
	view := ChainTraceView(resp)
	var b strings.Builder
	c := palette(opts.Colorize)
	writeTraceHeader(&b, view, c)
	if len(resp.Matches) > 0 {
		fmt.Fprintf(&b, "\n%sMatches (chain:line (part:line), or file:line):%s\n", c.grey, c.reset)
		for _, m := range resp.Matches {
			b.WriteString("  ")
			b.WriteString(chainMatchPlace(resp, m, c))
			if len(resp.Patterns) > 1 {
				fmt.Fprintf(&b, " %s[%s%s%s%s]%s", c.grey, c.reset, c.magenta, Printable(resp.Patterns[m.Pattern]), c.grey, c.reset)
			}
			b.WriteString(": ")
			b.WriteString(matchText(m.LineText, m.LineTextTruncated))
			b.WriteByte('\n')
		}
	}
	if opts.ShowContext {
		b.WriteString(FormatContextSection(BuildFileContexts(view), opts.Before, opts.After))
	}
	return b.String()
}

// chainMatchPlace is where a match is, as a row of FormatChainTraceCLI
// names it: `<handle>:<chain line> (<part>:<line>)` for a match in a
// part of a chain, `<path>:<line>` for one in a file of its own.
func chainMatchPlace(resp *rxtypes.ChainTraceResponse, m rxtypes.ChainMatch, c tracePalette) string {
	path := resp.Files[m.File]
	local := formatLineNumber(matchDisplayLine(m.Match()))
	if m.Chain == nil {
		return fmt.Sprintf("%s%s%s%s:%s%s%s%s", c.cyan, Printable(path), c.reset, c.grey, c.reset, c.yellow, local, c.reset)
	}
	global := "?"
	if m.ChainLine >= 1 {
		global = strconv.FormatInt(m.ChainLine, 10)
	}
	chain := fmt.Sprintf("%s%s%s%s:%s%s%s%s",
		c.cyan, Printable(resp.Chains[*m.Chain].Path), c.reset, c.grey, c.reset, c.yellow, global, c.reset)
	part := fmt.Sprintf("%s(%s%s%s:%s%s%s%s%s)%s",
		c.grey, c.reset, Printable(filepath.Base(path)), c.grey, c.reset, c.yellow, local, c.reset, c.grey, c.reset)
	return chain + " " + part
}

// matchText is a matched line as a row prints it: its bytes as the file
// holds them, the way `rx samples` and the context section print a
// line, without its line break, and marked when the answer holds only
// its first bytes.
func matchText(text *string, truncated bool) string {
	if text == nil {
		return ""
	}
	out := strings.TrimSuffix(*text, "\n")
	if truncated {
		out += cutLineMarker
	}
	return out
}

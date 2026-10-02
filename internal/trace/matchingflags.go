package trace

import "strings"

// MatchingFlag is one ripgrep option that changes which lines match. rx
// exposes the set under ripgrep's own names on every surface: as
// `rx trace --ignore-case` (or `-i`) on the command line, and as
// `ignore_case=true` on GET /v1/trace.
type MatchingFlag struct {
	// Long is ripgrep's long name without the dashes ("ignore-case").
	Long string
	// Short is ripgrep's one-letter name without the dash ("i"). The
	// argument ripgrep receives is always "-" + Short.
	Short string
	// Usage is the one-line help text for the flag.
	Usage string
}

// QueryParam is the flag's name as an HTTP query parameter: the long name
// with underscores, as every other rx query parameter is spelled.
func (f MatchingFlag) QueryParam() string {
	return strings.ReplaceAll(f.Long, "-", "_")
}

// MatchingFlags is the whole set of ripgrep options rx accepts from a
// caller. A selected flag reaches ripgrep on every path a search can
// take — plain chunks, compressed streams, seekable frames — and is part
// of the trace-cache key.
//
// SECURITY: the set is closed on purpose. ripgrep also has options that
// run a program (--pre), change the output the JSON parser reads
// (--count, --files) or let a match cross the newline-aligned chunk
// boundaries (--multiline). Forwarding unknown flags would hand all of
// them to anyone who can shape a command line or a URL, so a surface
// rejects a flag that is not in this table.
var MatchingFlags = []MatchingFlag{
	{Long: "ignore-case", Short: "i", Usage: "Match case-insensitively (ripgrep -i)"},
	{Long: "word-regexp", Short: "w", Usage: "Match only whole words (ripgrep -w)"},
	{Long: "line-regexp", Short: "x", Usage: "Match only whole lines (ripgrep -x)"},
	{Long: "fixed-strings", Short: "F", Usage: "Treat every pattern as literal text (ripgrep -F)"},
	{Long: "pcre2", Short: "P", Usage: "Use the PCRE2 engine, for look-around and backreferences (ripgrep -P)"},
}

// RipgrepArgs returns the ripgrep arguments for the flags whose long name
// is true in selected. The result follows the table's order, not the
// caller's, so two requests that differ only in the order their flags
// were given send ripgrep the same arguments and share a cache entry.
// Names the table does not know are ignored; the surfaces only build
// selected from the table, so an unknown name never gets this far.
func RipgrepArgs(selected map[string]bool) []string {
	args := []string{}
	for _, flag := range MatchingFlags {
		if selected[flag.Long] {
			args = append(args, "-"+flag.Short)
		}
	}
	return args
}

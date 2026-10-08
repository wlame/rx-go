package output

import (
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainMatch is a match for the formatting tests: the line text, its
// file, its line in the file, and its chain and chain line.
func chainMatch(file string, line int, text string, chain *string, chainLine int64) rxtypes.ChainMatch {
	relative := line
	return rxtypes.ChainMatch{
		Pattern: "p1", File: file, RelativeLineNumber: &relative, AbsoluteLineNumber: line,
		LineText: &text, Submatches: []rxtypes.Submatch{}, Chain: chain, ChainLine: chainLine,
	}
}

// matchRows are the rows of the match list, the lines that start with
// two spaces after the list's heading.
func matchRows(out string) []string {
	_, list, _ := strings.Cut(out, "Matches (chain:line (part:line), or file:line):\n")
	var rows []string
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(line, "  ") {
			rows = append(rows, line)
		}
	}
	return rows
}

// A row names a part's match by its chain and chain line, then the
// part's name and line; `?` for what is not known (a chain that is not
// ready, a line the scan left unnumbered); a file of its own by its path
// and line. The text follows as the file holds it, without its line
// break, and marked when cut.
func TestFormatChainTraceCLI_Rows(t *testing.T) {
	c1, c2 := "c1", "c2"
	resp := &rxtypes.ChainTraceResponse{
		Path:     []string{"/var/log"},
		Patterns: map[string]string{"p1": "error"},
		Files: map[string]string{
			"f1": "/var/log/syslog.3.gz", "f2": "/var/log/syslog", "f3": "/var/log/app.log.1", "f4": "/var/log/notes.txt",
		},
		Matches: []rxtypes.ChainMatch{
			chainMatch("f1", 500, "error one\n", &c1, 123456),
			chainMatch("f2", 7, "error\ttwo\n", &c1, 140000),
			chainMatch("f3", 12, "error pending\n", &c2, -1),
			chainMatch("f3", -1, "error unnumbered", &c2, -1),
			chainMatch("f4", 3, "error notes", nil, -1),
		},
		Chains: map[string]rxtypes.ChainRef{
			"c1": {Path: "/var/log/syslog", Name: "syslog", Parts: []string{"f1", "f2"}, State: rxtypes.ChainStateReady},
			"c2": {Path: "/var/log/app.log", Name: "app.log", Parts: []string{"f3"}, State: rxtypes.ChainStatePending},
		},
	}
	resp.Matches[4].LineTextTruncated = true
	out := FormatChainTraceCLI(resp, TraceFormatOptions{})
	want := []string{
		"  /var/log/syslog:123456 (syslog.3.gz:500): error one",
		"  /var/log/syslog:140000 (syslog:7): error\ttwo",
		"  /var/log/app.log:? (app.log.1:12): error pending",
		"  /var/log/app.log:? (app.log.1:?): error unnumbered",
		"  /var/log/notes.txt:3: error notes" + cutLineMarker,
	}
	if got := matchRows(out); !slices.Equal(got, want) {
		t.Fatalf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.HasPrefix(out, "Request ID: \nPath: /var/log\nPattern: error\n") || !strings.Contains(out, "Matches: 5\n") {
		t.Fatalf("header\n%s", out)
	}
}

// With several patterns each row names its pattern, since a line two
// patterns match is two rows.
func TestFormatChainTraceCLI_RowsNameTheirPatternWithSeveralPatterns(t *testing.T) {
	c1 := "c1"
	first, second := chainMatch("f1", 4, "alpha beta", &c1, 9), chainMatch("f1", 4, "alpha beta", &c1, 9)
	second.Pattern = "p2"
	resp := &rxtypes.ChainTraceResponse{
		Patterns: map[string]string{"p1": "alpha", "p2": "beta"},
		Files:    map[string]string{"f1": "x.log.1"},
		Matches:  []rxtypes.ChainMatch{first, second},
		Chains:   map[string]rxtypes.ChainRef{"c1": {Path: "x.log", Name: "x.log", Parts: []string{"f1"}}},
	}
	want := []string{"  x.log:9 (x.log.1:4) [alpha]: alpha beta", "  x.log:9 (x.log.1:4) [beta]: alpha beta"}
	if got := matchRows(FormatChainTraceCLI(resp, TraceFormatOptions{})); !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q", got, want)
	}
}

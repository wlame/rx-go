package output

import (
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// rx-python colorizes `rx trace` and rx-go did not, so the two produced
// different output for the same search the moment a terminal was
// involved — and `--no-color` here changed nothing, which is a flag that
// silently does not work.
//
// The sequences below are rx-python's, taken from
// models.py::TraceResponse.to_cli. A pty test would prove the same thing
// less directly: --color=always is the deterministic way to ask for
// color, and colorDecision is unit-tested separately.

func colorFixture() *rxtypes.TraceResponse {
	return &rxtypes.TraceResponse{
		RequestID: "fixed",
		Path:      []string{"/tmp/app.log"},
		Time:      0.004,
		Patterns:  map[string]string{"p1": "error"},
		Files:     map[string]string{"f1": "/tmp/app.log"},
		Matches: []rxtypes.Match{
			{Pattern: "p1", File: "f1", Offset: 11, AbsoluteLineNumber: 2, LineText: strPtr("error here")},
		},
	}
}

func TestFormatTraceCLI_ColorSequencesMatchPython(t *testing.T) {
	got := FormatTraceCLI(colorFixture(), TraceFormatOptions{Colorize: true})

	want := "\033[90mRequest ID:\033[0m fixed\n" +
		"\033[90mPath:\033[0m \033[1;36m/tmp/app.log\033[0m\n" +
		"\033[90mPattern:\033[0m \033[1;35merror\033[0m\n" +
		"\033[90mTime:\033[0m \033[33m0.004s\033[0m\n" +
		"\033[90mMatches:\033[0m \033[1;32m1\033[0m\n" +
		"\n\033[90mMatches (file:line:offset [pattern]):\033[0m\n" +
		"  \033[36m/tmp/app.log\033[0m\033[90m:\033[0m\033[33m2\033[0m\033[90m:\033[0m" +
		"\033[37m11\033[0m \033[90m[\033[0m\033[35merror\033[0m\033[90m]\033[0m\n"

	if got != want {
		t.Errorf("colored output differs\n got: %q\nwant: %q", got, want)
	}
}

// Without color the output is exactly what it has always been, so the
// golden layout tests and every piped consumer are unaffected.
func TestFormatTraceCLI_PlainOutputHasNoEscapes(t *testing.T) {
	got := FormatTraceCLI(colorFixture(), TraceFormatOptions{})

	if strings.Contains(got, "\033[") {
		t.Errorf("plain output carries escape sequences: %q", got)
	}
	want := "Request ID: fixed\n" +
		"Path: /tmp/app.log\n" +
		"Pattern: error\n" +
		"Time: 0.004s\n" +
		"Matches: 1\n" +
		"\nMatches (file:line:offset [pattern]):\n" +
		"  /tmp/app.log:2:11 [error]\n"
	if got != want {
		t.Errorf("plain output differs\n got: %q\nwant: %q", got, want)
	}
}

// Several patterns take the other branch of the header, which has its
// own colors in rx-python.
func TestFormatTraceCLI_SeveralPatternsAreColored(t *testing.T) {
	resp := colorFixture()
	resp.Patterns = map[string]string{"p1": "error", "p2": "warn"}

	got := FormatTraceCLI(resp, TraceFormatOptions{Colorize: true})

	for _, want := range []string{
		"\033[90mPatterns (2):\033[0m\n",
		"  \033[34mp1\033[0m: \033[35merror\033[0m\n",
		"  \033[34mp2\033[0m: \033[35mwarn\033[0m\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// The scanned/skipped/chunk counters are colored too, and each one has
// its own color in rx-python.
func TestFormatTraceCLI_CountersAreColored(t *testing.T) {
	resp := colorFixture()
	resp.ScannedFiles = []string{"/tmp/app.log"}
	resp.SkippedFiles = []string{"/tmp/binary.dat"}
	resp.FileChunks = map[string]int{"f1": 4}

	got := FormatTraceCLI(resp, TraceFormatOptions{Colorize: true})

	for _, want := range []string{
		"\033[90mFiles scanned:\033[0m \033[32m1\033[0m\n",
		"\033[90mFiles skipped:\033[0m \033[90m1\033[0m\n",
		"\033[90mParallel chunks:\033[0m \033[36m4\033[0m \033[90m(1 file(s) chunked)\033[0m\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// The context section stays plain in both backends: those lines are file
// content, and coloring them would compete with the match highlighting
// rather than help it.
func TestFormatTraceCLI_ContextSectionStaysPlain(t *testing.T) {
	resp := colorFixture()
	resp.ContextLines = map[string][]rxtypes.ContextLine{
		"p1:f1:11": {
			{AbsoluteLineNumber: 1, LineText: "before", AbsoluteOffset: 0},
			{AbsoluteLineNumber: 2, LineText: "error here", AbsoluteOffset: 7},
			{AbsoluteLineNumber: 3, LineText: "after", AbsoluteOffset: 18},
		},
	}

	got := FormatTraceCLI(resp, TraceFormatOptions{Colorize: true, ShowContext: true, Before: 1, After: 1})

	section := got[strings.Index(got, "Matches (file:line:offset [pattern]):"):]
	body := section[strings.Index(section, "\n"):]
	if strings.Contains(body, "before\033[") || strings.Contains(body, "\033[36mbefore") {
		t.Errorf("context lines carry color: %q", body)
	}
}

// strPtr is the line-text literal these fixtures need; the wire type
// carries a pointer so an absent line is null rather than empty.
func strPtr(v string) *string { return &v }

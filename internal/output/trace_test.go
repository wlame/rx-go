package output

import (
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ctxLine is a small constructor for a context line in these tests.
func ctxLine(n int, text string) rxtypes.ContextLine {
	return rxtypes.ContextLine{
		RelativeLineNumber: n,
		AbsoluteLineNumber: n,
		LineText:           text,
	}
}

// intPtr is a helper for the optional relative line number.
func intPtr(n int) *int { return &n }

// baseResponse is a five-line file with matches on lines 3 and 5.
func baseResponse() *rxtypes.TraceResponse {
	return &rxtypes.TraceResponse{
		Files:    map[string]string{"f1": "/logs/app.log"},
		Patterns: map[string]string{"p1": "ERROR"},
		Matches: []rxtypes.Match{
			{Pattern: "p1", File: "f1", Offset: 29, AbsoluteLineNumber: 3, RelativeLineNumber: intPtr(3)},
			{Pattern: "p1", File: "f1", Offset: 67, AbsoluteLineNumber: 5, RelativeLineNumber: intPtr(5)},
		},
		ContextLines: map[string][]rxtypes.ContextLine{
			"p1:f1:29": {ctxLine(2, "line two"), ctxLine(3, "line three ERROR"), ctxLine(4, "line four")},
			"p1:f1:67": {ctxLine(4, "line four"), ctxLine(5, "line five ERROR")},
		},
	}
}

// TestBuildFileContexts_MergesOverlappingWindows is the rule from the
// ticket: two matches whose context windows touch print one block, and
// the shared line appears once.
func TestBuildFileContexts_MergesOverlappingWindows(t *testing.T) {
	contexts := BuildFileContexts(baseResponse())

	if len(contexts) != 1 {
		t.Fatalf("files: got %d, want 1", len(contexts))
	}
	if len(contexts[0].Blocks) != 1 {
		t.Fatalf("blocks: got %d, want 1 (windows overlap)", len(contexts[0].Blocks))
	}
	rows := contexts[0].Blocks[0].Lines
	if len(rows) != 4 {
		t.Fatalf("rows: got %d, want 4 (lines 2..5, no duplicate)", len(rows))
	}
	for i, want := range []int{2, 3, 4, 5} {
		if rows[i].LineNumber != want {
			t.Errorf("row %d: line %d, want %d", i, rows[i].LineNumber, want)
		}
	}
	if !rows[1].IsMatch || !rows[3].IsMatch {
		t.Errorf("lines 3 and 5 should be marked as matches: %+v", rows)
	}
	if rows[0].IsMatch || rows[2].IsMatch {
		t.Errorf("lines 2 and 4 are context, not matches: %+v", rows)
	}
}

// TestBuildFileContexts_SplitsDistantWindows keeps unrelated regions apart.
func TestBuildFileContexts_SplitsDistantWindows(t *testing.T) {
	resp := baseResponse()
	resp.Matches = append(resp.Matches, rxtypes.Match{
		Pattern: "p1", File: "f1", Offset: 900, AbsoluteLineNumber: 40, RelativeLineNumber: intPtr(40),
	})
	resp.ContextLines["p1:f1:900"] = []rxtypes.ContextLine{
		ctxLine(39, "line thirty-nine"),
		ctxLine(40, "line forty ERROR"),
		ctxLine(41, "line forty-one"),
	}

	contexts := BuildFileContexts(resp)

	if len(contexts[0].Blocks) != 2 {
		t.Fatalf("blocks: got %d, want 2 (lines 2-5 and 39-41)", len(contexts[0].Blocks))
	}
	if got := contexts[0].Blocks[1].Lines[0].LineNumber; got != 39 {
		t.Errorf("second block starts at %d, want 39", got)
	}
}

// TestBuildFileContexts_DropsUnknownLineNumbers guards the chunked-file
// case: a line the backend could not place must not be printed at a
// guessed position.
func TestBuildFileContexts_DropsUnknownLineNumbers(t *testing.T) {
	resp := baseResponse()
	resp.ContextLines["p1:f1:29"] = []rxtypes.ContextLine{
		{RelativeLineNumber: -1, AbsoluteLineNumber: -1, LineText: "unplaceable"},
		ctxLine(3, "line three ERROR"),
	}

	contexts := BuildFileContexts(resp)

	for _, block := range contexts[0].Blocks {
		for _, row := range block.Lines {
			if row.Text == "unplaceable" {
				t.Errorf("a line with no known number was printed: %+v", row)
			}
		}
	}
}

// TestBuildFileContexts_SortsFilesByPath keeps output stable across runs;
// Go map iteration is random.
func TestBuildFileContexts_SortsFilesByPath(t *testing.T) {
	resp := baseResponse()
	resp.Files["f2"] = "/logs/a-first.log"
	resp.ContextLines["p1:f2:10"] = []rxtypes.ContextLine{ctxLine(1, "first file")}

	contexts := BuildFileContexts(resp)

	if len(contexts) != 2 {
		t.Fatalf("files: got %d, want 2", len(contexts))
	}
	if contexts[0].Path != "/logs/a-first.log" {
		t.Errorf("first file: got %s, want /logs/a-first.log", contexts[0].Path)
	}
}

// TestFormatContextSection_RendersGrepStyleMarkers pins the exact text,
// because both backends must produce it byte for byte.
func TestFormatContextSection_RendersGrepStyleMarkers(t *testing.T) {
	got := FormatContextSection(BuildFileContexts(baseResponse()), 1, 1)

	want := "\nContext (1 before, 1 after):\n" +
		"\n/logs/app.log\n" +
		"2- line two\n" +
		"3: line three ERROR\n" +
		"4- line four\n" +
		"5: line five ERROR\n"
	if got != want {
		t.Errorf("context section:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestFormatContextSection_SeparatesBlocks draws grep's "--" between
// non-contiguous regions and right-aligns the numbers.
func TestFormatContextSection_SeparatesBlocks(t *testing.T) {
	resp := baseResponse()
	resp.ContextLines["p1:f1:900"] = []rxtypes.ContextLine{ctxLine(40, "line forty")}

	got := FormatContextSection(BuildFileContexts(resp), 1, 1)

	if !strings.Contains(got, "\n--\n") {
		t.Errorf("blocks should be separated by --:\n%s", got)
	}
	if !strings.Contains(got, " 2- line two") {
		t.Errorf("line numbers should be right-aligned to the widest:\n%s", got)
	}
}

// TestFormatContextSection_EmptyWhenNoContext keeps the section out of
// the way when nothing was asked for.
func TestFormatContextSection_EmptyWhenNoContext(t *testing.T) {
	resp := baseResponse()
	resp.ContextLines = nil

	if got := FormatContextSection(BuildFileContexts(resp), 0, 0); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

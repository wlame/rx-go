package output

import (
	"strconv"
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

// A capped scan of a chunked file can leave a line unnumbered with the
// number ripgrep gave it inside its chunk. That number names another
// line of the file, so the line is not printed there, and a match it
// belongs to marks no line.
func TestBuildFileContexts_DropsLinesNumberedOnlyInsideTheirChunk(t *testing.T) {
	resp := baseResponse()
	resp.Matches = append(resp.Matches, rxtypes.Match{
		Pattern: "p1", File: "f1", Offset: 900000, AbsoluteLineNumber: -1, RelativeLineNumber: intPtr(2),
	})
	resp.ContextLines["p1:f1:900000"] = []rxtypes.ContextLine{
		{RelativeLineNumber: 2, AbsoluteLineNumber: -1, LineText: "line 9002 ERROR, of another chunk"},
		{RelativeLineNumber: 4, AbsoluteLineNumber: -1, LineText: "line 9004, of another chunk"},
	}

	contexts := BuildFileContexts(resp)

	for _, block := range contexts[0].Blocks {
		for _, row := range block.Lines {
			if strings.Contains(row.Text, "another chunk") {
				t.Errorf("a line numbered only inside its chunk was printed as line %d: %q", row.LineNumber, row.Text)
			}
			if row.LineNumber == 2 && row.IsMatch {
				t.Errorf("line 2 is marked as a match by a match numbered only inside its chunk")
			}
		}
	}
}

// TestBuildFileContexts_ListsFilesInFileIDOrder: the context section
// lists the files in the order the match list does, by the number in
// the file id (f2 before f10), whatever their paths; Go map iteration is
// random, so the order is set here.
func TestBuildFileContexts_ListsFilesInFileIDOrder(t *testing.T) {
	resp := baseResponse()
	resp.Files["f2"] = "/logs/b-second.log"
	resp.ContextLines["p1:f2:10"] = []rxtypes.ContextLine{ctxLine(1, "second file")}
	resp.Files["f10"] = "/logs/a-tenth.log"
	resp.ContextLines["p1:f10:10"] = []rxtypes.ContextLine{ctxLine(1, "tenth file")}

	contexts := BuildFileContexts(resp)

	var got []string
	for _, fc := range contexts {
		got = append(got, fc.Path)
	}
	want := []string{"/logs/app.log", "/logs/b-second.log", "/logs/a-tenth.log"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files: got %v, want %v", got, want)
	}
}

// TestFormatTraceCLI_ListsPatternsByNumber: the patterns header lists
// p2 before p10, as the matches are ordered.
func TestFormatTraceCLI_ListsPatternsByNumber(t *testing.T) {
	resp := baseResponse()
	resp.Patterns = map[string]string{}
	var want []string
	for k := 1; k <= 11; k++ {
		id := "p" + strconv.Itoa(k)
		resp.Patterns[id] = "word" + strconv.Itoa(k)
		want = append(want, "  "+id+": word"+strconv.Itoa(k))
	}

	out := FormatTraceCLI(resp, TraceFormatOptions{})

	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  p") {
			got = append(got, line)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("patterns header:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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

// A line the answer cut is printed with a marker after its text, so a
// reader does not take its first bytes for the whole line.
func TestFormatContextSection_MarksACutLine(t *testing.T) {
	resp := baseResponse()
	cut := ctxLine(4, "line fo")
	cut.LineTextTruncated = true
	resp.ContextLines["p1:f1:29"][2] = cut
	resp.ContextLines["p1:f1:67"][0] = cut

	got := FormatContextSection(BuildFileContexts(resp), 1, 1)

	if !strings.Contains(got, "\n4- line fo [line truncated]\n") {
		t.Errorf("the cut line 4 should carry the marker:\n%s", got)
	}
	if strings.Count(got, "[line truncated]") != 1 {
		t.Errorf("only line 4 is cut:\n%s", got)
	}
}

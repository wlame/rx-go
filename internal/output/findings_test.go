package output

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// findingsAnswer is a trace answer with the findings of the example:
// an error line and a three-line traceback in app.log.
func findingsAnswer() *rxtypes.TraceResponse {
	return &rxtypes.TraceResponse{
		Files: map[string]string{"f1": "app.log"},
		Signatures: map[string]rxtypes.SignatureRef{
			"python-traceback": {Name: "Python traceback", Group: "traceback", Level: "high", Kind: "block"},
			"error-line":       {Name: "Error line", Group: "severity", Level: "medium", Kind: "regex"},
		},
		Findings: []rxtypes.Finding{
			{Signature: "error-line", File: "f1", StartLine: 2, EndLine: 2, Lines: []string{"ERROR boom one"}},
			{Signature: "python-traceback", File: "f1", StartLine: 4, EndLine: 6,
				Lines: []string{"Traceback (most recent call last):", `  File "a.py", line 1`, "ValueError: bad"}},
			{Signature: "error-line", File: "f1", StartLine: 9, EndLine: 9, Lines: []string{"ERROR boom two"}},
		},
		FindingSummary: map[string]rxtypes.FileFindingSummary{"f1": {State: "full", Signatures: map[string]rxtypes.FindingCount{
			"python-traceback": {Count: 1, Stored: 1}, "error-line": {Count: 2, Stored: 2},
		}}},
	}
}

// The findings part: the count line with each signature's count in
// selection order, then each finding as path:lines, id, group and
// level, and its numbered lines.
func TestFormatFindings_Layout(t *testing.T) {
	got := FormatFindingsCLI(findingsAnswer(), FindingsFormatOptions{Order: []string{"python-traceback", "error-line"}})
	want := `
Findings: 3 in 1 file (python-traceback 1, error-line 2)

app.log:2  error-line  severity · medium
  2: ERROR boom one

app.log:4-6  python-traceback  traceback · high
  4: Traceback (most recent call last):
  5:   File "a.py", line 1
  6: ValueError: bad

app.log:9  error-line  severity · medium
  9: ERROR boom two
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A block longer than 20 lines prints its first 20, then how many are
// left and the samples command that prints them; lines asked for before
// and after come with their own marker, the after lines after the note.
func TestFormatFindings_LongBlockPrintsTwentyLinesAndTheCommandForTheRest(t *testing.T) {
	var lines []string
	for n := 4; n <= 403; n++ {
		lines = append(lines, fmt.Sprintf("LINE %d", n))
	}
	resp := &rxtypes.TraceResponse{
		Files:      map[string]string{"f1": "app.log"},
		Signatures: map[string]rxtypes.SignatureRef{"big-block": {Group: "structure", Level: "low", Kind: "block"}},
		Findings: []rxtypes.Finding{{Signature: "big-block", File: "f1", StartLine: 4, EndLine: 403,
			Before: []string{"LINE 3"}, Lines: lines, After: []string{"LINE 404"}}},
		FindingSummary: map[string]rxtypes.FileFindingSummary{"f1": {Signatures: map[string]rxtypes.FindingCount{"big-block": {Count: 1, Stored: 1}}}},
	}
	got := FormatFindingsCLI(resp, FindingsFormatOptions{Order: []string{"big-block"}})
	var want strings.Builder
	want.WriteString("\nFindings: 1 in 1 file (big-block 1)\n\napp.log:4-403  big-block  structure · low\n")
	want.WriteString("    3- LINE 3\n")
	for n := 4; n <= 23; n++ {
		fmt.Fprintf(&want, "  %3d: LINE %d\n", n, n)
	}
	want.WriteString("  … 380 more lines: rx samples app.log --lines=24-403\n")
	want.WriteString("  404- LINE 404\n")
	if got != want.String() {
		t.Errorf("got:\n%s\nwant:\n%s", got, want.String())
	}
}

// A finding past the text budget says its text was left out and how to
// read it; a path that needs quoting is quoted in the command and
// escaped where it is printed.
func TestFormatFindings_TruncatedFindingSaysHowToReadIt(t *testing.T) {
	resp := findingsAnswer()
	resp.Files["f1"] = "my app.log"
	resp.Findings[1].Lines, resp.Findings[1].LinesTruncated = nil, true
	got := FormatFindingsCLI(resp, FindingsFormatOptions{Order: []string{"python-traceback", "error-line"}})
	want := "my app.log:4-6  python-traceback  traceback · high\n" +
		"  (text left out: the answer's text budget, RX_FINDINGS_TEXT_MAX_BYTES, ran out; rx samples 'my app.log' --lines=4-6)\n"
	if !strings.Contains(got, want) {
		t.Errorf("got:\n%s\nwant it to hold:\n%s", got, want)
	}
}

// Counts past the cap are exact in the count line, which says how many
// are listed; a selection that found nothing says so.
func TestFormatFindings_CountLine(t *testing.T) {
	resp := findingsAnswer()
	resp.FindingSummary["f1"].Signatures["error-line"] = rxtypes.FindingCount{Count: 12000, Stored: 2}
	got := FormatFindingsCLI(resp, FindingsFormatOptions{Order: []string{"python-traceback", "error-line"}})
	if want := "Findings: 12001 in 1 file (python-traceback 1, error-line 12000, first 2 listed)\n"; !strings.Contains(got, want) {
		t.Errorf("got:\n%s", got)
	}
	none := &rxtypes.TraceResponse{
		Signatures:     map[string]rxtypes.SignatureRef{"error-line": {}},
		Findings:       []rxtypes.Finding{},
		FindingSummary: map[string]rxtypes.FileFindingSummary{"f1": {Signatures: map[string]rxtypes.FindingCount{"error-line": {}}}},
	}
	if got := FormatFindingsCLI(none, FindingsFormatOptions{Order: []string{"error-line"}}); got != "\nFindings: 0\n" {
		t.Errorf("got %q", got)
	}
	if got := FormatFindingsCLI(&rxtypes.TraceResponse{}, FindingsFormatOptions{}); got != "" {
		t.Errorf("a trace without signatures printed %q", got)
	}
}

// With color, the id and the level take the level's color, the place the
// match list's colors; without it there is no escape sequence at all.
func TestFormatFindings_ColorsByLevel(t *testing.T) {
	got := FormatFindingsCLI(findingsAnswer(), FindingsFormatOptions{Order: []string{"python-traceback", "error-line"}, Colorize: true})
	for _, want := range []string{
		ColorRed + "python-traceback" + ColorReset, ColorRed + "high" + ColorReset,
		ColorYellow + "error-line" + ColorReset, ColorCyan + "app.log" + ColorReset,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("colored output lacks %q:\n%q", want, got)
		}
	}
	plain := FormatFindingsCLI(findingsAnswer(), FindingsFormatOptions{Order: []string{"python-traceback", "error-line"}})
	if strings.Contains(plain, "\033[") {
		t.Errorf("plain output holds an escape sequence: %q", plain)
	}
}

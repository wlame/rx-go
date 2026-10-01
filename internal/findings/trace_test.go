package findings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// traceOptions are Trace's options for a test: a fresh cache, two
// workers, 1 MiB lines and a budget that holds everything.
func traceOptions(t testing.TB) TraceOptions {
	t.Helper()
	return TraceOptions{Scan: testOptions(t), LineTextBytes: 1 << 20, TextBudget: 64 << 20}
}

// traceFiles classifies paths and gives them the ids f1, f2, … in order.
func traceFiles(t testing.TB, paths ...string) []TraceFile {
	t.Helper()
	out := make([]TraceFile, len(paths))
	for i, p := range paths {
		out[i] = TraceFile{ID: fmt.Sprintf("f%d", i+1), File: classify(t, p)}
	}
	return out
}

// mustTrace runs Trace and fails the test on an error.
func mustTrace(t testing.TB, files []TraceFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts TraceOptions) *TraceAnswer {
	t.Helper()
	ans, err := Trace(context.Background(), files, cat, sel, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

// fileLines splits a text into its lines, without line endings.
func fileLines(text string) []string {
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// Findings of several files come in the one order (file id, first line,
// last line, signature id), each with its file id, its lines read from
// the file, and the counts of its file in the summary; the selection is
// described by id.
func TestTrace_FindingsOfSeveralFilesInTheOneOrder(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defWarnLine, defTrace)
	dir := t.TempDir()
	first := "LINE 1 ok\nLINE 2 ERROR WARN both\nTraceback (most recent call last):\n  File \"a.py\", line 1\nValueError: bad\nLINE 6 ERROR again\n"
	second := "LINE 1 WARN only\nLINE 2 quiet\n"
	paths := []string{writeLog(t, dir, "b.log", first), writeLog(t, dir, "a.log", second)}
	ans := mustTrace(t, traceFiles(t, paths...), cat, pick(t, cat, "err-line", "warn-line", "py-trace"), traceOptions(t))

	type key struct {
		file, sig  string
		start, end int64
	}
	var got []key
	for _, f := range ans.Findings {
		got = append(got, key{f.File, f.Signature, f.StartLine, f.EndLine})
	}
	want := []key{
		{"f1", "err-line", 2, 2}, {"f1", "warn-line", 2, 2}, {"f1", "py-trace", 3, 5},
		{"f1", "err-line", 6, 6}, {"f2", "warn-line", 1, 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("findings %v\nwant %v", got, want)
	}
	texts := map[string][]string{"f1": fileLines(first), "f2": fileLines(second)}
	for _, f := range ans.Findings {
		if wantLines := texts[f.File][f.StartLine-1 : f.EndLine]; !slices.Equal(f.Lines, wantLines) || f.LinesTruncated {
			t.Errorf("%s %s:%d lines %q, want %q", f.Signature, f.File, f.StartLine, f.Lines, wantLines)
		}
	}
	if s := ans.FindingSummary["f1"].Signatures; s["err-line"] != (rxtypes.FindingCount{Count: 2, Stored: 2}) || s["py-trace"].Count != 1 {
		t.Errorf("summary of f1 %+v", ans.FindingSummary["f1"])
	}
	if s := ans.FindingSummary["f2"]; s.Signatures["err-line"] != (rxtypes.FindingCount{}) || s.State != string(StateFull) {
		t.Errorf("summary of f2 %+v", s)
	}
	if ref := ans.Signatures["py-trace"]; ref != (rxtypes.SignatureRef{Name: "Python traceback", Group: "traceback", Level: "medium", Kind: "block"}) {
		t.Errorf("signature py-trace %+v", ref)
	}
	if len(ans.Signatures) != 3 {
		t.Errorf("signatures %v, want the three selected", ans.Signatures)
	}
}

// The lines asked for before and after a finding come with it, as far
// as the file has them.
func TestTrace_LinesBeforeAndAfter(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	text := "LINE 1 ERROR first\nLINE 2\nLINE 3\nLINE 4 ERROR mid\nLINE 5\n"
	opts := traceOptions(t)
	opts.Before, opts.After = 2, 1
	ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", text)), cat, pick(t, cat, "err-line"), opts)
	if len(ans.Findings) != 2 {
		t.Fatalf("findings %+v", ans.Findings)
	}
	if f := ans.Findings[0]; f.Before != nil || !slices.Equal(f.After, []string{"LINE 2"}) {
		t.Errorf("line 1: before %q after %q", f.Before, f.After)
	}
	if f := ans.Findings[1]; !slices.Equal(f.Before, []string{"LINE 2", "LINE 3"}) || !slices.Equal(f.After, []string{"LINE 5"}) {
		t.Errorf("line 4: before %q after %q", f.Before, f.After)
	}
}

// The text budget keeps the lines of the findings that fit, in the one
// order; the finding at which it runs out and every one after it, in
// this file and the next, keep their place and lose their text, marked
// lines_truncated, and the next file's text is not read at all.
func TestTrace_TextBudgetCutsTheLinesAndMarksTheRest(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	dir := t.TempDir()
	line := func(n int) string { return fmt.Sprintf("LINE %d ERROR %s", n, strings.Repeat("x", 90)) } // 100+ bytes
	var b strings.Builder
	for n := 1; n <= 10; n++ {
		b.WriteString(line(n) + "\n")
	}
	paths := []string{writeLog(t, dir, "a.log", b.String()), writeLog(t, dir, "b.log", b.String())}
	opts := traceOptions(t)
	opts.TextBudget = int64(3*len(line(1)) + 5) // three lines and a bit
	tr := newTracer()
	var read []string
	inner := tr.readSpans
	tr.readSpans = func(ctx context.Context, req samples.SpansRequest, visit func(int, samples.SpanLine) samples.SpanAction) error {
		read = append(read, filepath.Base(req.Source.Path()))
		return inner(ctx, req, visit)
	}
	ans, err := tr.trace(context.Background(), traceFiles(t, paths...), cat, pick(t, cat, "err-line"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Findings) != 20 {
		t.Fatalf("%d findings, want 20", len(ans.Findings))
	}
	kept := int64(0)
	for i, f := range ans.Findings {
		wantKept := i < 3
		if (f.Lines != nil) != wantKept || f.LinesTruncated == wantKept {
			t.Errorf("finding %d (%s:%d): lines %q truncated %v", i, f.File, f.StartLine, f.Lines, f.LinesTruncated)
		}
		for _, l := range f.Lines {
			kept += int64(len(l))
		}
	}
	if kept > opts.TextBudget {
		t.Errorf("kept %d bytes of text, budget %d", kept, opts.TextBudget)
	}
	if !slices.Equal(read, []string{"a.log"}) {
		t.Errorf("text read from %v; b.log's findings were all past the budget", read)
	}
}

// Each line is cut like a trace's line_text: at the line text limit, on
// a character boundary.
func TestTrace_EachLineIsCutAtTheLineTextLimit(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	text := "ERROR " + strings.Repeat("é", 100) + "\n"
	opts := traceOptions(t)
	opts.LineTextBytes = 11
	ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", text)), cat, pick(t, cat, "err-line"), opts)
	if got := ans.Findings[0].Lines; !slices.Equal(got, []string{"ERROR éé"}) {
		t.Errorf("lines %q, want the line cut to 10 bytes on a character boundary", got)
	}
}

// defPem is a redacting block: a private key's header and its body.
const defPem = `
  - id: pem-key
    name: Private key
    group: secret
    description: A PEM private key.
    redact: true
    block:
      start: '-----BEGIN [A-Z ]*PRIVATE KEY-----'
      end: '-----END [A-Z ]*PRIVATE KEY-----'
`

// A redacted value is masked in the excerpt and in the lines, of its
// own finding and of any other finding on its line; a secret past the
// excerpt's 200 bytes is masked in the lines and marks the finding
// masked; the body of a redacting block is replaced line by line by
// `[masked, N bytes]`. No secret byte reaches the answer.
func TestTrace_SecretsAreMaskedInEveryLine(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defKey, defPem)
	const key, body = "ABCDEFGHIJ012345", "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"
	text := "ERROR token KEY-" + key + " refused\n" +
		"ERROR " + strings.Repeat("y", 220) + " KEY-" + key + "\n" +
		"-----BEGIN PRIVATE KEY-----\n" + body + "\n" + body + "xyz\n-----END PRIVATE KEY-----\n"
	opts := traceOptions(t)
	opts.After = 1
	ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", text)), cat, pick(t, cat, "err-line", "api-key", "pem-key"), opts)
	raw, err := json.Marshal(ans)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key[4:], body[:12]} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the answer holds %q:\n%s", secret, raw)
		}
	}
	byKey := map[string]rxtypes.Finding{}
	for _, f := range ans.Findings {
		byKey[fmt.Sprintf("%s:%d", f.Signature, f.StartLine)] = f
	}
	if f := byKey["err-line:1"]; !f.Masked || f.Lines[0] != "ERROR token KEY-ABCD************ refused" {
		t.Errorf("err-line:1 masked %v lines %q", f.Masked, f.Lines)
	}
	if f := byKey["err-line:2"]; !f.Masked || !strings.HasSuffix(f.Lines[0], "KEY-ABCD************") || strings.Contains(f.Excerpt, "KEY") {
		t.Errorf("err-line:2 (secret past the excerpt) masked %v excerpt %q lines %q", f.Masked, f.Excerpt, f.Lines)
	}
	pem := byKey["pem-key:3"]
	want := []string{"----************", "[masked, 32 bytes]", "[masked, 35 bytes]", "[masked, 25 bytes]"}
	if !pem.Masked || !slices.Equal(pem.Lines, want) || pem.EndLine != 6 {
		t.Errorf("pem-key masked %v lines %q, want %q", pem.Masked, pem.Lines, want)
	}
}

// Line 1's byte-order mark is not part of the line a redactor matches
// (as in a scan's line view), so an anchor at the line's start masks
// its value in line 1's text too.
func TestTrace_ByteOrderMarkDoesNotHideASecret(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, `
  - id: pw
    name: Password
    group: secret
    description: A password at the start of a line.
    redact: true
    regex: '^password=(\S+)'
    capture: 1
`)
	text := "\uFEFFpassword=hunter2hunter2 ERROR\n"
	ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", text)), cat, pick(t, cat, "err-line"), traceOptions(t))
	if f := ans.Findings[0]; !f.Masked || strings.Contains(f.Lines[0], "hunter2") {
		t.Errorf("masked %v lines %q", f.Masked, f.Lines)
	}
}

// A file whose scan fails is listed as skipped with the reason, and
// gives no finding and no summary; the other files are answered.
func TestTrace_AFileThatCannotBeScannedIsSkipped(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	dir := t.TempDir()
	gone := writeLog(t, dir, "gone.log", "ERROR one\n")
	kept := writeLog(t, dir, "kept.log", "ERROR two\n")
	files := traceFiles(t, gone, kept)
	// The path now leads to another file: its pin refuses the read.
	replacement := writeLog(t, dir, "new.log", "ERROR three\n")
	if err := os.Rename(replacement, gone); err != nil {
		t.Fatal(err)
	}
	ans := mustTrace(t, files, cat, pick(t, cat, "err-line"), traceOptions(t))
	if len(ans.Skipped) != 1 || ans.Skipped[0].Path != gone || ans.Skipped[0].Reason == "" {
		t.Errorf("skipped %+v, want %s with a reason", ans.Skipped, gone)
	}
	if len(ans.Findings) != 1 || ans.Findings[0].File != "f2" {
		t.Errorf("findings %+v, want kept.log's only", ans.Findings)
	}
	if _, ok := ans.FindingSummary["f1"]; ok {
		t.Errorf("summary holds the skipped file: %+v", ans.FindingSummary)
	}
}

// time_ms is an instant: in a file whose timestamps carry no zone, a
// line's wall clock is read in the log zone (RX_LOG_TZ), as samples
// reads it; a file whose timestamps carry zones is not shifted.
func TestTrace_TimeIsAnInstantReadInTheLogZone(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	dir := t.TempDir()
	zoneless := writeLog(t, dir, "local.log", "2026-01-15 10:00:00 INFO start\n2026-01-15 10:00:05 ERROR boom\n")
	zoned := writeLog(t, dir, "zoned.log", "2026-01-15T10:00:00+01:00 INFO start\n2026-01-15T10:00:05+01:00 ERROR boom\n")
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RX_LOG_TZ", "Europe/Berlin")
	opts := traceOptions(t)
	opts.LogZone = berlin
	ans := mustTrace(t, traceFiles(t, zoneless, zoned), cat, pick(t, cat, "err-line"), opts)
	want := time.Date(2026, 1, 15, 10, 0, 5, 0, berlin).UnixMilli() // 09:00:05 UTC
	for _, f := range ans.Findings {
		if f.TimeMS == nil || *f.TimeMS != want {
			t.Errorf("%s: time_ms %v, want %d", f.File, f.TimeMS, want)
		}
	}
	// samples gives the same instant for the zone-less line.
	resp, err := samples.Resolve(context.Background(), samples.Request{
		Path: zoneless, Lines: []samples.OffsetOrRange{{Start: 2}}, IndexLoader: samples.NoIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	stamps := resp.LineTimestamps["2"]
	if len(stamps) != 1 || stamps[0] == nil || *stamps[0] != want {
		t.Errorf("samples line_timestamps %v, findings %d", resp.LineTimestamps, want)
	}
}

// A read of a file's text that fails is the file's failure: listed as
// skipped, its findings left out; a cancel is the call's.
func TestTrace_ATextReadThatFailsSkipsTheFile(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	path := writeLog(t, t.TempDir(), "app.log", "ERROR one\n")
	tr := newTracer()
	tr.readSpans = func(context.Context, samples.SpansRequest, func(int, samples.SpanLine) samples.SpanAction) error {
		return fmt.Errorf("%s: %w", path, samples.ErrSpanPastEnd)
	}
	ans, err := tr.trace(context.Background(), traceFiles(t, path), cat, pick(t, cat, "err-line"), traceOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Findings) != 0 || len(ans.Skipped) != 1 || len(ans.FindingSummary) != 0 {
		t.Errorf("answer %+v", ans)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Trace(ctx, traceFiles(t, path), cat, pick(t, cat, "err-line"), traceOptions(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled call: %v", err)
	}
}

// A secret that the line text limit cuts in two is masked before the
// cut: the line is read past the limit and masked whole, so the part
// before the cut never shows.
func TestTrace_ASecretCutByTheLineLimitIsMasked(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defKey)
	text := "ERROR 0123456789 KEY-ABCDEFGHIJ012345 end\n"
	opts := traceOptions(t)
	opts.LineTextBytes = 30 // cuts the key after KEY-ABCDEFGHI
	ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", text)), cat, pick(t, cat, "err-line"), opts)
	if f := ans.Findings[0]; !f.Masked || f.Lines[0] != "ERROR 0123456789 KEY-ABCD*****" {
		t.Errorf("masked %v lines %q", f.Masked, f.Lines)
	}
}

// Each line spends at least a byte of the budget: a finding of
// thousands of empty lines does not hand over lines for nothing.
func TestTrace_EmptyLinesSpendTheBudget(t *testing.T) {
	cat := loadCatalogue(t, defBlank)
	text := "LINE 1\n" + strings.Repeat("\n", 3000) + "LINE 3002\n"
	opts := traceOptions(t)
	opts.TextBudget = 1000
	ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", text)), cat, pick(t, cat, "blank-lines"), opts)
	if len(ans.Findings) != 1 || !ans.Findings[0].LinesTruncated || ans.Findings[0].Lines != nil {
		t.Errorf("findings %+v, want one blank run past the budget", ans.Findings)
	}
}

package jsonblob

// Unit and end-to-end tests for the json-blob-multiline detector.
//
// Two layers, mirroring the other detectors' test files:
//
//  1. Unit: drive a fresh Detector through a real Coordinator with
//     in-memory line input built from the testdata/*.log fixtures.
//     Exercises the state machine directly without going through
//     index.Build. Testdata fixtures are plain log-shaped files;
//     each test names its fixture and asserts on the emitted anomalies.
//
//  2. End-to-end: one test drives the detector through index.Build
//     with Analyze=true on a real fixture so the full coordinator +
//     flush-context path is exercised. Asserts the anomaly surfaces
//     on UnifiedFileIndex.Anomalies with Detector == detectorName.
//
// Fixtures live in testdata/ so they're easy to read and replace without
// code changes; the loader below splits them into lines ready to feed.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/index"
)

// feedFixture reads testdata/<name>, splits into lines, and drives a
// fresh Detector through a real Coordinator. Returns anomalies from the
// detector directly (so tests see the semantic Category, not the
// coordinator's Name() rewrite — matches the pattern used by other
// detectors' tests).
//
// windowLines lets each test pick a window size; the unterminated test
// uses a small window to force a truncated emission.
func feedFixture(t *testing.T, name string, windowLines int) []analyzer.Anomaly {
	t.Helper()

	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return feedLines(t, splitLinesPreserveOffsets(data), windowLines)
}

// feedLines is the low-level helper shared by fixture-based tests and
// synthetic-input tests. Takes a pre-computed line list with byte
// offsets so the caller can assert on offsets directly if desired.
func feedLines(t *testing.T, lines []rawLine, windowLines int) []analyzer.Anomaly {
	t.Helper()

	d := New()
	coord := analyzer.NewCoordinator(windowLines, []analyzer.LineDetector{d})
	for i, l := range lines {
		coord.ProcessLine(int64(i+1), l.start, l.end, l.bytes)
	}
	return d.Finalize(nil)
}

// rawLine is one line's bytes with its absolute byte-offset span in the
// source. Offsets are computed so the line-terminator is included in
// end-start (matching the coordinator's end-exclusive convention).
type rawLine struct {
	bytes      []byte
	start, end int64
}

// splitLinesPreserveOffsets splits data on '\n' and returns each line
// WITHOUT the trailing newline, along with the byte offset range of
// the line in the original data (end == start + len(line) + 1 for
// newline-terminated lines; for a possible trailing line without '\n'
// end == start + len(line)).
//
// Matches how the coordinator is fed by the index builder: raw line
// bytes without terminator, offsets include the terminator.
func splitLinesPreserveOffsets(data []byte) []rawLine {
	out := make([]rawLine, 0, bytes.Count(data, []byte{'\n'})+1)
	var off int64
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			// Final line without a newline.
			line := data
			out = append(out, rawLine{
				bytes: line,
				start: off,
				end:   off + int64(len(line)),
			})
			return out
		}
		line := data[:i]
		out = append(out, rawLine{
			bytes: line,
			start: off,
			end:   off + int64(i+1),
		})
		off += int64(i + 1)
		data = data[i+1:]
	}
	return out
}

// TestDetector_SimpleObject covers the baseline case: a plain `{...}`
// spanning several lines with scalar fields inside. Emits exactly one
// anomaly covering the `{` and `}` lines inclusive.
func TestDetector_SimpleObject(t *testing.T) {
	got := feedFixture(t, "simple_object.log", 128)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Lines 3..6 in the fixture: `{`, two body lines, `}` — 4 lines total.
	if a.StartLine != 3 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 3..6", a.StartLine, a.EndLine)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "4 lines") {
		t.Errorf("description = %q, want it to mention '4 lines'", a.Description)
	}
}

// TestDetector_NestedObject covers a blob with nested `{}` and `[]`
// inside it. The outer counter must return to zero exactly once, on
// the outer `}` line. Only one anomaly is emitted (the outermost blob);
// inner sub-objects are not separately reported.
func TestDetector_NestedObject(t *testing.T) {
	got := feedFixture(t, "nested_object.log", 128)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Opener on line 2 (`{`), closer on line 9 (`}`).
	if a.StartLine != 2 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 2..9", a.StartLine, a.EndLine)
	}
}

// TestDetector_SimpleArray mirrors TestDetector_SimpleObject but for
// an opener `[` / closer `]`. Ensures the array case works identically.
func TestDetector_SimpleArray(t *testing.T) {
	got := feedFixture(t, "simple_array.log", 128)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 2..6", a.StartLine, a.EndLine)
	}
}

// TestDetector_StringsWithBraces is the string-awareness regression:
// string values contain `{`, `[`, `]`, `}` and escaped `\"` quotes.
// The bracket counter must IGNORE those so the outer blob still closes
// on line 7 and emits exactly one anomaly.
func TestDetector_StringsWithBraces(t *testing.T) {
	got := feedFixture(t, "strings_with_braces.log", 128)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Opener on line 2, closer on line 7.
	if a.StartLine != 2 || a.EndLine != 7 {
		t.Errorf("span: start=%d end=%d, want 2..7", a.StartLine, a.EndLine)
	}
}

// TestDetector_Unterminated_TruncatedAtWindow is the window-edge abort:
// the fixture opens a blob but never closes it. We use a tiny window
// (4 lines) so the age guard fires on the 4th line inside the blob,
// emitting a truncated anomaly + the per-run sentinel.
//
// Window arithmetic: opener is line 2 of the fixture. With window
// size W, isAged fires when (current_line - opener_line) >= W. We
// want the fixture to have at least W+1 body lines so aging occurs.
// The unterminated.log fixture has 7 lines total (opener on line 2,
// body to line 7), so with W=4 the abort triggers on line 6.
func TestDetector_Unterminated_TruncatedAtWindow(t *testing.T) {
	got := feedFixture(t, "unterminated.log", 4)

	// We expect two anomalies:
	//   1. the truncated-span anomaly emitted mid-scan when the window aged,
	//   2. the per-run sentinel emitted by Finalize with the
	//      "truncated_at_window: true" prefix.
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2 (truncated span + sentinel); %+v",
			len(got), got)
	}

	// Find the sentinel (span is line 0..0 with zero offsets).
	var sentinel, partial analyzer.Anomaly
	for _, a := range got {
		if a.StartLine == 0 && a.EndLine == 0 {
			sentinel = a
		} else {
			partial = a
		}
	}
	if sentinel.Description == "" {
		t.Fatalf("no sentinel anomaly found: %+v", got)
	}
	if !strings.HasPrefix(sentinel.Description, truncatedSentinelPrefix) {
		t.Errorf("sentinel description = %q, want prefix %q",
			sentinel.Description, truncatedSentinelPrefix)
	}
	if !strings.Contains(sentinel.Description, "truncated_count=") {
		t.Errorf("sentinel description = %q, want a 'truncated_count=' tag",
			sentinel.Description)
	}

	// Partial anomaly must start at the opener line (line 2 in fixture).
	if partial.StartLine != 2 {
		t.Errorf("partial start line = %d, want 2", partial.StartLine)
	}
	if !strings.HasPrefix(partial.Description, "truncated:") {
		t.Errorf("partial description = %q, want 'truncated:' prefix",
			partial.Description)
	}
}

// TestDetector_NoOpener covers input that contains no multi-line JSON
// at all — the detector should stay silent. Protects against a
// regression where a random `{` character in a prose line is treated
// as an opener.
func TestDetector_NoOpener(t *testing.T) {
	lines := linesFromStrings([]string{
		"plain log message",
		"another line with { somewhere in the middle",
		"or with } closer-ish chars too",
		"done",
	})
	got := feedLines(t, lines, 128)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (no opener lines); %+v", len(got), got)
	}
}

// TestDetector_OpenerWithTrailingContent covers the "not exactly `{`"
// case: a line like `request { action: "foo" }` shouldn't open a blob
// because the trimmed content isn't exactly `{`.
func TestDetector_OpenerWithTrailingContent(t *testing.T) {
	lines := linesFromStrings([]string{
		`request: { "action": "foo" }`,
		`{ inline stuff here }`,
	})
	got := feedLines(t, lines, 128)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (openers not exact); %+v",
			len(got), got)
	}
}

// TestDetector_MalformedAbandoned covers the "counter hits zero but
// trimmed line doesn't end with the expected closer" branch. The
// detector abandons silently — no anomaly, no sentinel.
//
// Sequence: opener `{` (counter=1), then a line `} extra stuff`: the
// `}` drops counter to 0 but the trimmed line's last char is `f`, not
// `}`, so we don't emit. Abandon silently.
func TestDetector_MalformedAbandoned(t *testing.T) {
	lines := linesFromStrings([]string{
		"{",
		"} extra stuff here",
		"done",
	})
	got := feedLines(t, lines, 128)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (malformed abandoned silently); %+v",
			len(got), got)
	}
}

// TestDetector_MismatchedCloser covers opener `{` closed on a `]`
// line: the detector abandons silently. No anomaly, no sentinel.
func TestDetector_MismatchedCloser(t *testing.T) {
	lines := linesFromStrings([]string{
		"{",
		`  "key": "value"`,
		"]",
		"done",
	})
	got := feedLines(t, lines, 128)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (mismatched closer); %+v",
			len(got), got)
	}
}

// TestDetector_CloserIndentMismatch: a `}` at a different indent from
// the opener is NOT a valid closer. We abandon silently.
func TestDetector_CloserIndentMismatch(t *testing.T) {
	lines := linesFromStrings([]string{
		"{",
		`  "nested": {`,
		`    "inner": "value"`,
		`  }`,
		`    }`, // indent 4, but opener was indent 0
		"done",
	})
	got := feedLines(t, lines, 128)
	// Counter math: opener +1, line 2 `{` +1 (counter=2), line 4 `}` -1
	// (counter=1), line 5 `}` -1 (counter=0). Counter hits 0 on line 5,
	// trimmed ends with `}`, but indent is 4 not 0 — abandon silently.
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (closer indent != opener indent); %+v",
			len(got), got)
	}
}

// TestDetector_UnclosedAtEOF covers the EOF case: a blob opens but the
// file ends while the blob is still open. Finalize must emit the
// truncated span + sentinel.
func TestDetector_UnclosedAtEOF(t *testing.T) {
	lines := linesFromStrings([]string{
		"{",
		`  "key": "value"`,
		// no closer, EOF
	})
	got := feedLines(t, lines, 128)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2 (EOF-truncated span + sentinel); %+v",
			len(got), got)
	}
	// Find the sentinel.
	var sentinelFound bool
	for _, a := range got {
		if strings.HasPrefix(a.Description, truncatedSentinelPrefix) {
			sentinelFound = true
			break
		}
	}
	if !sentinelFound {
		t.Errorf("no sentinel anomaly found among: %+v", got)
	}
}

// TestDetector_TwoConsecutiveBlobs exercises the reset-and-reopen path:
// two well-formed blobs back to back. Both should emit, no sentinel.
func TestDetector_TwoConsecutiveBlobs(t *testing.T) {
	lines := linesFromStrings([]string{
		"first event",
		"{",
		`  "id": 1`,
		"}",
		"second event",
		"[",
		`  "a",`,
		`  "b"`,
		"]",
		"done",
	})
	got := feedLines(t, lines, 128)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 2 || got[0].EndLine != 4 {
		t.Errorf("first span: start=%d end=%d, want 2..4",
			got[0].StartLine, got[0].EndLine)
	}
	if got[1].StartLine != 6 || got[1].EndLine != 9 {
		t.Errorf("second span: start=%d end=%d, want 6..9",
			got[1].StartLine, got[1].EndLine)
	}
}

// TestScanBrackets_StringAwareness table-tests the bracket-counting
// primitive directly. Covers plain characters, string-enclosed ones,
// and escaped-quote cases.
func TestScanBrackets_StringAwareness(t *testing.T) {
	cases := []struct {
		in                    string
		wantOpens, wantCloses int
	}{
		{`{`, 1, 0},
		{`}`, 0, 1},
		{`{}`, 1, 1},
		{`[[]]`, 2, 2},
		{`"{}"`, 0, 0},  // brackets inside string
		{`"\""`, 0, 0},  // escaped quote; string opens and closes
		{`"\\"`, 0, 0},  // literal backslash at end of string
		{`"\\{"`, 0, 0}, // literal backslash then `{` inside string
		{`"a"{`, 1, 0},  // string then opener outside
		{`"{" }`, 0, 1}, // string `{` then space then literal `}`
		// `\"{\"` — the first `"` is outside a string, so it opens one.
		// Then `{` and the trailing chars are inside the string; the
		// final `"` is preceded by one `\` (escaped per isEscaped), so
		// we stay inside the string to EOL. Net: 0 opens, 0 closes.
		// This is a documented limitation of the line-local scanner: we
		// do not track a "was previously in a string" state across
		// line boundaries, and escape-run parsing is local.
		{`\"{\"`, 0, 0},
	}
	for _, c := range cases {
		opens, closes := scanBrackets([]byte(c.in))
		if opens != c.wantOpens || closes != c.wantCloses {
			t.Errorf("scanBrackets(%q) = %d opens, %d closes; want %d, %d",
				c.in, opens, closes, c.wantOpens, c.wantCloses)
		}
	}
}

// TestIsEscaped table-tests the backslash-counting primitive used by
// the string-awareness logic.
func TestIsEscaped(t *testing.T) {
	cases := []struct {
		in   string
		idx  int
		want bool
	}{
		{`"`, 0, false},   // no preceding backslash
		{`\"`, 1, true},   // one backslash
		{`\\"`, 2, false}, // two backslashes (the pair escapes itself)
		{`\\\"`, 3, true}, // three (odd)
		{`a"`, 1, false},  // plain char, then quote
	}
	for _, c := range cases {
		got := isEscaped([]byte(c.in), c.idx)
		if got != c.want {
			t.Errorf("isEscaped(%q, %d) = %v, want %v", c.in, c.idx, got, c.want)
		}
	}
}

// TestDetector_Metadata nails the plan-mandated metadata. If a constant
// drifts this test fails loudly so the change is deliberate.
func TestDetector_Metadata(t *testing.T) {
	d := New()
	cases := []struct {
		got, want string
		field     string
	}{
		{d.Name(), "json-blob-multiline", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "format", "Category"},
		{d.Description(), "Multi-line JSON objects or arrays spanning several lines", "Description"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
}

// TestDetector_EndToEnd_ViaIndexBuild confirms the detector plugs into
// the real index.Build pipeline and its anomalies surface in the
// UnifiedFileIndex.Anomalies list with Detector == detectorName.
func TestDetector_EndToEnd_ViaIndexBuild(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "simple_object.log"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "event.log")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	idx, err := index.Build(path, index.BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{New()},
	})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if idx.Anomalies == nil {
		t.Fatal("idx.Anomalies is nil; expected populated slice under Analyze=true")
	}

	anomalies := *idx.Anomalies
	if len(anomalies) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(anomalies), anomalies)
	}
	a := anomalies[0]
	if a.Detector != detectorName {
		t.Errorf("Detector = %q, want %q", a.Detector, detectorName)
	}
	if a.Category != detectorName {
		// The coordinator rewrites Category to Name() — that's the
		// dedup contract (see analyzer.Coordinator.Finalize).
		t.Errorf("Category = %q, want %q (coordinator overwrites to Name())",
			a.Category, detectorName)
	}
	if a.StartLine != 3 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 3..6", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

// linesFromStrings turns a []string of line contents (no trailing
// newlines) into []rawLine with offsets computed as if each line were
// terminated by a single '\n'. Handy for synthetic-input tests.
func linesFromStrings(ss []string) []rawLine {
	out := make([]rawLine, 0, len(ss))
	var off int64
	for _, s := range ss {
		start := off
		end := start + int64(len(s)) + 1 // +1 for the '\n' terminator
		out = append(out, rawLine{
			bytes: []byte(s),
			start: start,
			end:   end,
		})
		off = end
	}
	return out
}

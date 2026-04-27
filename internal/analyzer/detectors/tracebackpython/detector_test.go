package tracebackpython

// Unit and end-to-end tests for the traceback-python detector.
//
// Two layers, mirroring the other detectors' test files:
//
//  1. Unit: drive a fresh Detector through a real Coordinator with
//     in-memory line input built from the testdata/*.log fixtures.
//     Exercises the state machine directly without going through
//     index.Build.
//
//  2. End-to-end: one test drives the detector through index.Build with
//     Analyze=true on a real fixture so the full coordinator + flush-
//     context path is exercised. Asserts the anomaly surfaces on
//     UnifiedFileIndex.Anomalies with Detector == detectorName.

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
// fresh Detector through a real Coordinator. Returns the anomalies
// emitted by the detector directly.
func feedFixture(t *testing.T, name string) []analyzer.Anomaly {
	t.Helper()

	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return feedLines(t, splitLinesPreserveOffsets(data))
}

// feedLines is the low-level helper shared by fixture-based tests and
// synthetic-input tests. Takes a pre-computed line list with byte
// offsets so the caller can assert on offsets directly if desired.
func feedLines(t *testing.T, lines []rawLine) []analyzer.Anomaly {
	t.Helper()

	d := New()
	// Window size of 128 matches the default used elsewhere; Python
	// traceback detection doesn't need a large window (the state machine
	// uses no lookback), but we use a realistic size to exercise the
	// normal path through the coordinator.
	coord := analyzer.NewCoordinator(128, []analyzer.LineDetector{d})
	for i, l := range lines {
		coord.ProcessLine(int64(i+1), l.start, l.end, l.bytes)
	}
	return d.Finalize(nil)
}

// rawLine is one line's bytes with its absolute byte-offset span in the
// source. Offsets follow the coordinator's end-exclusive convention:
// end - start includes the terminator.
type rawLine struct {
	bytes      []byte
	start, end int64
}

// splitLinesPreserveOffsets splits data on '\n' and returns each line
// WITHOUT the trailing newline, along with the byte offset range of
// the line in the original data.
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

// TestDetector_SingleFrame covers the baseline case: opener, one frame
// (File + source line), and one exception line. Emits exactly one
// anomaly spanning the opener through the exception line.
func TestDetector_SingleFrame(t *testing.T) {
	got := feedFixture(t, "single_frame.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: request received id=abc
	//   2: Traceback (most recent call last):     <- opener
	//   3:   File "...", line 42, in handle        <- frame
	//   4:     value = int(payload)                <- source
	//   5: ValueError: invalid literal...          <- exception line
	//   6: request completed
	if a.StartLine != 2 || a.EndLine != 5 {
		t.Errorf("span: start=%d end=%d, want 2..5", a.StartLine, a.EndLine)
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

// TestDetector_MultiFrame covers a traceback with several stacked
// frames (the common case: user code → library → leaf). The detector
// must extend the span through every frame line and stop on the
// exception line.
func TestDetector_MultiFrame(t *testing.T) {
	got := feedFixture(t, "multi_frame.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: starting up
	//   2: Traceback (most recent call last):     <- opener
	//   3..8: frames and source lines (6 lines)
	//   9: RuntimeError: boom                     <- exception line
	//   10: shutting down
	if a.StartLine != 2 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 2..9", a.StartLine, a.EndLine)
	}
}

// TestDetector_Chained covers Python's exception-chaining output: two
// traceback blocks separated by a blank line and the "During handling
// of the above exception, another exception occurred:" prose. The
// detector emits ONE anomaly per opener — so two in this case.
func TestDetector_Chained(t *testing.T) {
	got := feedFixture(t, "chained.log")
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2 (one per opener); %+v", len(got), got)
	}
	// First traceback: opener on line 2, exception on line 7.
	// Fixture:
	//   1: inbound request
	//   2: Traceback (most recent call last):
	//   3..6: frames and source lines
	//   7: json.decoder.JSONDecodeError: ...
	//   8: (blank)
	//   9: During handling of the above exception, another exception occurred:
	//   10: (blank)
	//   11: Traceback (most recent call last):
	//   12..15: frames and source lines
	//   16: ValueError: bad payload
	//   17: request failed
	if got[0].StartLine != 2 || got[0].EndLine != 7 {
		t.Errorf("first span: start=%d end=%d, want 2..7",
			got[0].StartLine, got[0].EndLine)
	}
	if got[1].StartLine != 11 || got[1].EndLine != 16 {
		t.Errorf("second span: start=%d end=%d, want 11..16",
			got[1].StartLine, got[1].EndLine)
	}
}

// TestDetector_EOFTraceback covers the "file ends mid-traceback" case.
// The exception line is the last line of the fixture (no trailing
// newline or blank). Finalize must emit the pending traceback.
func TestDetector_EOFTraceback(t *testing.T) {
	got := feedFixture(t, "eof_traceback.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: process crashing now
	//   2: Traceback (most recent call last):     <- opener
	//   3..6: frames and source lines
	//   7: SystemExit: 1                          <- exception line, last real line
	if a.StartLine != 2 || a.EndLine != 7 {
		t.Errorf("span: start=%d end=%d, want 2..7", a.StartLine, a.EndLine)
	}
}

// TestDetector_NoOpener covers a file that looks traceback-ish but
// doesn't have the exact opener cue. The detector should stay silent.
// Guards against a regression where a partial-match regex would fire.
func TestDetector_NoOpener(t *testing.T) {
	lines := linesFromStrings([]string{
		"Traceback (something else):",
		`  File "/app/x.py", line 1, in <module>`,
		"ValueError: nope",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (opener cue not exact); %+v",
			len(got), got)
	}
}

// TestDetector_FramesNoException covers the "traceback is truncated
// mid-frames by a log rotation" case: opener + a frame, then a non-
// indented, non-exception-shaped line. Detector emits the partial span
// and closes.
func TestDetector_FramesNoException(t *testing.T) {
	lines := linesFromStrings([]string{
		"Traceback (most recent call last):",
		`  File "/app/x.py", line 5, in foo`,
		"    do_thing()",
		"some random prose that terminates the traceback",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Opener on line 1, last frame-ish line on line 3. The terminator
	// line (4) does NOT belong to the anomaly.
	if a.StartLine != 1 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 1..3", a.StartLine, a.EndLine)
	}
}

// TestDetector_BlankLineClosesTraceback covers the blank-line-inside-
// frames edge case. Python tracebacks don't contain blank lines; a
// blank line mid-traceback means the traceback ended without an
// exception line (e.g. piped through a tool that wrapped every Nth
// line with a blank). Detector emits what it has and closes.
func TestDetector_BlankLineClosesTraceback(t *testing.T) {
	lines := linesFromStrings([]string{
		"Traceback (most recent call last):",
		`  File "/app/x.py", line 5, in foo`,
		"    do_thing()",
		"", // blank line — closes traceback without exception line
		"next log event",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 1 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 1..3", a.StartLine, a.EndLine)
	}
}

// TestDetector_ExceptionNoMessage covers the case where the exception
// line has no colon/message — e.g. `KeyboardInterrupt` on its own line.
// The exceptionLineRe allows the optional `: ...` tail.
func TestDetector_ExceptionNoMessage(t *testing.T) {
	lines := linesFromStrings([]string{
		"Traceback (most recent call last):",
		`  File "/app/x.py", line 5, in foo`,
		"    do_thing()",
		"KeyboardInterrupt",
		"next log event",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 1 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 1..4", a.StartLine, a.EndLine)
	}
}

// TestDetector_ExceptionDottedPath covers exception types with dotted
// module paths (e.g. `json.decoder.JSONDecodeError: ...`). The
// exceptionLineRe must accept these.
func TestDetector_ExceptionDottedPath(t *testing.T) {
	lines := linesFromStrings([]string{
		"Traceback (most recent call last):",
		`  File "/app/x.py", line 5, in foo`,
		"    do_thing()",
		"json.decoder.JSONDecodeError: Expecting value",
		"next log event",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 1 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 1..4", a.StartLine, a.EndLine)
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
		{d.Name(), "traceback-python", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "log-traceback", "Category"},
		{d.Description(), "Python tracebacks (Traceback (most recent call last): ...)", "Description"},
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
	content, err := os.ReadFile(filepath.Join("testdata", "single_frame.log"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
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
	if a.Category != detectorCategory {
		t.Errorf("Category = %q, want %q (semantic bucket)", a.Category, detectorCategory)
	}
	if a.StartLine != 2 || a.EndLine != 5 {
		t.Errorf("span: start=%d end=%d, want 2..5", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

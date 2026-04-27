package tracebackjava

// Unit and end-to-end tests for the traceback-java detector.
//
// Two layers, mirroring the tracebackpython tests:
//
//  1. Unit: drive a fresh Detector through a real Coordinator with
//     in-memory line input built from testdata/*.log fixtures or
//     synthetic line lists. Exercises the state machine without going
//     through index.Build.
//
//  2. End-to-end: one test drives the detector through index.Build with
//     Analyze=true on a real fixture so the full coordinator +
//     flush-context path is exercised. Asserts the anomaly surfaces on
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
// fresh Detector through a real Coordinator. Returns anomalies from the
// detector directly (so tests see the semantic Category, not the
// coordinator's Name() rewrite — matches the pattern used by other
// detectors' tests).
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
	// Window size of 128 matches the default used elsewhere; Java stack
	// detection uses no lookback, but a realistic size still exercises
	// the normal path through the coordinator.
	coord := analyzer.NewCoordinator(128, []analyzer.LineDetector{d})
	for i, l := range lines {
		coord.ProcessLine(int64(i+1), l.start, l.end, l.bytes)
	}
	return d.Finalize(nil)
}

// rawLine is one line's bytes with its absolute byte-offset span in
// the source. Offsets follow the coordinator's end-exclusive
// convention: end - start includes the terminator.
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

// TestDetector_SingleStack covers the baseline case: Exception in
// thread opener + two frames.
func TestDetector_SingleStack(t *testing.T) {
	got := feedFixture(t, "single_stack.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: service starting up
	//   2: Exception in thread "main" java.lang.NullPointerException: ...
	//   3: \tat com.example.app.Main.process(Main.java:42)
	//   4: \tat com.example.app.Main.main(Main.java:17)
	//   5: service exiting
	if a.StartLine != 2 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 2..4", a.StartLine, a.EndLine)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "3 lines") {
		t.Errorf("description = %q, want it to mention '3 lines'", a.Description)
	}
}

// TestDetector_CausedByChain covers a throwable chain where the outer
// exception has two cause layers. The detector must extend the span
// through every `Caused by: ...` line plus its frames plus the
// `... N more` summaries.
func TestDetector_CausedByChain(t *testing.T) {
	got := feedFixture(t, "caused_by.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1:  inbound request id=42
	//   2:  Exception in thread "..." java.lang.RuntimeException: wrapper failure
	//   3..5: frames
	//   6:  Caused by: java.io.IOException: disk quota exceeded
	//   7..8: frames
	//   9:  ... 2 more
	//   10: Caused by: java.nio.file.FileSystemException: ...
	//   11: frame
	//   12: ... 4 more
	//   13: request failed id=42
	if a.StartLine != 2 || a.EndLine != 12 {
		t.Errorf("span: start=%d end=%d, want 2..12", a.StartLine, a.EndLine)
	}
}

// TestDetector_Suppressed covers a stack with a Suppressed section.
// The Suppressed block is indented and also contains nested frames;
// the detector's continuation shapes must handle both the
// `\tSuppressed: ...` header and the indented frames and `... N more`
// lines inside it.
func TestDetector_Suppressed(t *testing.T) {
	got := feedFixture(t, "suppressed.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1:  batch job start
	//   2:  Exception in thread "worker-3" ...
	//   3..4: frames
	//   5:  \tSuppressed: java.io.IOException: cleanup failure
	//   6..7: nested frames (extra-indented: match the `\s+at` shape)
	//   8:  \t\t... 1 more
	//   9:  \tSuppressed: java.lang.IllegalStateException: ...
	//   10: nested frame
	//   11: \t\t... 2 more
	//   12: Caused by: java.sql.SQLException: ...
	//   13: frame
	//   14: ... 3 more
	//   15: batch job done
	if a.StartLine != 2 || a.EndLine != 14 {
		t.Errorf("span: start=%d end=%d, want 2..14", a.StartLine, a.EndLine)
	}
}

// TestDetector_MultipleStacksBackToBack covers two stacks separated by
// nothing but a new `Exception in thread "..."` line. Each is its own
// anomaly — the detector must not merge them.
func TestDetector_MultipleStacksBackToBack(t *testing.T) {
	got := feedFixture(t, "multiple_stacks.log")
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	// Fixture (1-based):
	//   1: request one start
	//   2: Exception in thread "pool-1-thread-1" java.lang.IllegalArgumentException: bad arg
	//   3..4: frames
	//   5: Exception in thread "pool-1-thread-2" java.lang.NullPointerException: npe here
	//   6..7: frames
	//   8: request one end
	if got[0].StartLine != 2 || got[0].EndLine != 4 {
		t.Errorf("first span: start=%d end=%d, want 2..4",
			got[0].StartLine, got[0].EndLine)
	}
	if got[1].StartLine != 5 || got[1].EndLine != 7 {
		t.Errorf("second span: start=%d end=%d, want 5..7",
			got[1].StartLine, got[1].EndLine)
	}
}

// TestDetector_BareExceptionOpener covers the second opener shape: a
// bare FQCN-shaped type at column 0 (e.g. the output of
// `printStackTrace()` without the "Exception in thread" prefix).
func TestDetector_BareExceptionOpener(t *testing.T) {
	got := feedFixture(t, "bare_exception.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1: starting
	//   2: java.lang.IllegalStateException: something broke
	//   3..4: frames
	//   5: done
	if a.StartLine != 2 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 2..4", a.StartLine, a.EndLine)
	}
}

// TestDetector_EOFStack covers a file that ends mid-stack (no trailing
// blank or terminator line). Finalize must emit the pending region.
func TestDetector_EOFStack(t *testing.T) {
	lines := linesFromStrings([]string{
		`Exception in thread "main" java.lang.NullPointerException: x`,
		"\tat com.example.Main.m(Main.java:1)",
		"\tat com.example.Main.main(Main.java:2)",
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

// TestDetector_BlankLineClosesStack covers the blank-line close: an
// opener + one frame + a blank line closes the region. Any subsequent
// content is not part of the anomaly.
func TestDetector_BlankLineClosesStack(t *testing.T) {
	lines := linesFromStrings([]string{
		`Exception in thread "main" java.lang.RuntimeException: boom`,
		"\tat com.example.App.run(App.java:10)",
		"", // blank line closes
		"app shutting down",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 1 || a.EndLine != 2 {
		t.Errorf("span: start=%d end=%d, want 1..2", a.StartLine, a.EndLine)
	}
}

// TestDetector_NoOpener covers input that never matches either opener
// shape. The detector must stay silent.
func TestDetector_NoOpener(t *testing.T) {
	lines := linesFromStrings([]string{
		"just a log line",
		"\tat something that looks like a frame",
		"Caused by: something that looks cause-ish",
		"another log line",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_PartialOpenerShouldNotFire guards against a regression
// where a substring match would cause a mid-line `Exception in thread`
// to open a stack. Both opener regexes are anchored with `^`.
func TestDetector_PartialOpenerShouldNotFire(t *testing.T) {
	lines := linesFromStrings([]string{
		// Leading content before the "Exception in thread" — must not match.
		`2026-04-21 12:00:00 INFO Exception in thread "main" reported by user`,
		// Bare opener shape with leading whitespace — must not match.
		"  java.lang.NullPointerException: indented",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_Metadata nails the plan-mandated metadata. If a
// constant drifts this test fails loudly so the change is deliberate.
func TestDetector_Metadata(t *testing.T) {
	d := New()
	cases := []struct {
		got, want string
		field     string
	}{
		{d.Name(), "traceback-java", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "log-traceback", "Category"},
		{d.Description(), "Java stack traces (Exception in thread ... / Caused by: / Suppressed:)", "Description"},
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
	content, err := os.ReadFile(filepath.Join("testdata", "single_stack.log"))
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
	if a.Category != detectorName {
		// The coordinator rewrites Category to Name() — that's the
		// dedup contract (see analyzer.Coordinator.Finalize).
		t.Errorf("Category = %q, want %q (coordinator overwrites to Name())",
			a.Category, detectorName)
	}
	if a.StartLine != 2 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 2..4", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

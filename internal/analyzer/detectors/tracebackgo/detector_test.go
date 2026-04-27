package tracebackgo

// Unit and end-to-end tests for the traceback-go detector.
//
// Two layers, mirroring the tracebackpython / tracebackjava tests:
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
	// Window size of 128 matches the default used elsewhere; Go stack
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

// TestDetector_SimplePanic covers the baseline: a `panic: ...` opener,
// one goroutine block, and the `exit status N` terminator.
func TestDetector_SimplePanic(t *testing.T) {
	got := feedFixture(t, "simple_panic.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: service starting up
	//   2: panic: runtime error: index out of range [3] with length 2
	//   3: (blank)
	//   4: goroutine 1 [running]:
	//   5: main.process(...)
	//   6: \t/app/main.go:42
	//   7: main.main()
	//   8: \t/app/main.go:17 +0x35
	//   9: exit status 2
	//   10: service exiting
	if a.StartLine != 2 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 2..9", a.StartLine, a.EndLine)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "8 lines") {
		t.Errorf("description = %q, want it to mention '8 lines'", a.Description)
	}
}

// TestDetector_MultipleGoroutines covers a dump with three goroutine
// blocks separated by blank lines. The detector must treat the internal
// blank lines as separators (continue the region), not as terminators.
func TestDetector_MultipleGoroutines(t *testing.T) {
	got := feedFixture(t, "multiple_goroutines.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1:  worker pool ready
	//   2:  panic: send on closed channel
	//   3:  (blank)
	//   4:  goroutine 7 [running]:
	//   5:  main.producer(0xc0000140a0)
	//   6:  \t/app/producer.go:31 +0x85
	//   7:  created by main.main
	//   8:  \t/app/main.go:12 +0x6f
	//   9:  (blank — internal separator)
	//   10: goroutine 1 [chan receive]:
	//   11: main.main()
	//   12: \t/app/main.go:15 +0x8a
	//   13: (blank — internal separator)
	//   14: goroutine 5 [chan receive]:
	//   15: main.consumer(0xc0000140a0)
	//   16: \t/app/consumer.go:22 +0x4c
	//   17: created by main.main
	//   18: \t/app/main.go:13 +0x90
	//   19: exit status 2
	//   20: shutdown complete
	if a.StartLine != 2 || a.EndLine != 19 {
		t.Errorf("span: start=%d end=%d, want 2..19", a.StartLine, a.EndLine)
	}
}

// TestDetector_RaceDetector covers a runtime-reported data race
// (fatal error: concurrent map writes) which opens with
// `fatal error: ` rather than `panic: ` and dumps multiple
// goroutines. This exercises the fatal-error opener branch.
func TestDetector_RaceDetector(t *testing.T) {
	got := feedFixture(t, "race_detector.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1:  starting batch job
	//   2:  fatal error: concurrent map writes
	//   3:  (blank)
	//   4:  goroutine 8 [running]:
	//   5:  main.writer(0xc000010060)
	//   6:  \t/app/race.go:19 +0x5b
	//   7:  created by main.main
	//   8:  \t/app/race.go:11 +0x6f
	//   9:  (blank — internal separator)
	//   10: goroutine 1 [sleep]:
	//   11: time.Sleep(0x3b9aca00)
	//   12: \t/usr/local/go/src/runtime/time.go:195 +0x135
	//   13: main.main()
	//   14: \t/app/race.go:14 +0x7c
	//   15: (blank — internal separator)
	//   16: goroutine 9 [runnable]:
	//   17: main.writer(0xc000010060)
	//   18: \t/app/race.go:19 +0x5b
	//   19: created by main.main
	//   20: \t/app/race.go:11 +0x6f
	//   21: exit status 2
	//   22: job finished
	if a.StartLine != 2 || a.EndLine != 21 {
		t.Errorf("span: start=%d end=%d, want 2..21", a.StartLine, a.EndLine)
	}
}

// TestDetector_SIGSEGV covers a SIGSEGV panic. The distinctive feature
// is the `[signal SIGSEGV: ...]` info line that follows the `panic: `
// opener — the detector must treat it as a continuation, not as a
// region terminator.
func TestDetector_SIGSEGV(t *testing.T) {
	got := feedFixture(t, "sigsegv.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1:  server listening on :8080
	//   2:  panic: runtime error: invalid memory address or nil pointer dereference
	//   3:  [signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a5f8c]
	//   4:  (blank)
	//   5:  goroutine 18 [running]:
	//   6:  main.(*Handler).Serve(0x0, 0xc000012080)
	//   7:  \t/app/handler.go:55 +0x2c
	//   8:  net/http.serverHandler.ServeHTTP(0xc000010200, 0xc000012080, 0xc000012100)
	//   9:  \t/usr/local/go/src/net/http/server.go:2871 +0x43b
	//   10: net/http.(*conn).serve(0xc000014000, 0x61a9c0, 0xc000050040)
	//   11: \t/usr/local/go/src/net/http/server.go:1930 +0xb1f
	//   12: created by net/http.(*Server).Serve
	//   13: \t/usr/local/go/src/net/http/server.go:2969 +0x36c
	//   14: exit status 2
	//   15: server shutting down
	if a.StartLine != 2 || a.EndLine != 14 {
		t.Errorf("span: start=%d end=%d, want 2..14", a.StartLine, a.EndLine)
	}
}

// TestDetector_EOFStack covers a file that ends mid-stack (no trailing
// `exit status` / blank). Finalize must emit the pending region.
func TestDetector_EOFStack(t *testing.T) {
	lines := linesFromStrings([]string{
		"panic: boom",
		"",
		"goroutine 1 [running]:",
		"main.main()",
		"\t/app/main.go:10 +0x20",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 1 || a.EndLine != 5 {
		t.Errorf("span: start=%d end=%d, want 1..5", a.StartLine, a.EndLine)
	}
}

// TestDetector_BlankThenNonContinuationCloses covers the close rule:
// a blank line followed by a non-continuation line terminates the
// region (and the non-continuation line is NOT included).
func TestDetector_BlankThenNonContinuationCloses(t *testing.T) {
	lines := linesFromStrings([]string{
		"panic: boom",
		"",
		"goroutine 1 [running]:",
		"main.main()",
		"\t/app/main.go:10",
		"",
		"ordinary log line after crash",
		"another ordinary line",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Region must span opener through the last indented frame (line 5).
	// The trailing blank (line 6) and log lines (7, 8) are excluded.
	if a.StartLine != 1 || a.EndLine != 5 {
		t.Errorf("span: start=%d end=%d, want 1..5", a.StartLine, a.EndLine)
	}
}

// TestDetector_DoubleBlankCloses covers the "two blanks in a row"
// case — if we see a blank in stateInStackBlank, that's definitely the
// end (no real Go traceback has two consecutive blank lines inside it).
func TestDetector_DoubleBlankCloses(t *testing.T) {
	lines := linesFromStrings([]string{
		"panic: boom",
		"",
		"goroutine 1 [running]:",
		"main.main()",
		"\t/app/main.go:10",
		"",
		"",
		"\t/bogus/continuation.go:1",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Region must end on line 5 — the second blank closes immediately,
	// and the line after (8) is not part of the region.
	if a.StartLine != 1 || a.EndLine != 5 {
		t.Errorf("span: start=%d end=%d, want 1..5", a.StartLine, a.EndLine)
	}
}

// TestDetector_BackToBackPanics covers two panics separated only by a
// new `panic: ` opener (no blank line between). Each is its own
// anomaly — the detector must not merge them.
func TestDetector_BackToBackPanics(t *testing.T) {
	lines := linesFromStrings([]string{
		"panic: first",
		"goroutine 1 [running]:",
		"main.a()",
		"\t/app/main.go:1",
		"panic: second",
		"goroutine 2 [running]:",
		"main.b()",
		"\t/app/main.go:2",
	})
	got := feedLines(t, lines)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 1 || got[0].EndLine != 4 {
		t.Errorf("first span: start=%d end=%d, want 1..4", got[0].StartLine, got[0].EndLine)
	}
	if got[1].StartLine != 5 || got[1].EndLine != 8 {
		t.Errorf("second span: start=%d end=%d, want 5..8", got[1].StartLine, got[1].EndLine)
	}
}

// TestDetector_NoOpener covers input that never matches either opener
// shape. The detector must stay silent.
func TestDetector_NoOpener(t *testing.T) {
	lines := linesFromStrings([]string{
		"just a log line",
		"goroutine info reported",
		"\tat something that looks like a frame",
		"another log line",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_PartialOpenerShouldNotFire guards against a regression
// where a substring match would cause a mid-line `panic:` or
// `fatal error:` to open a stack. Both openers are anchored at column 0
// via bytes.HasPrefix.
func TestDetector_PartialOpenerShouldNotFire(t *testing.T) {
	lines := linesFromStrings([]string{
		// Leading content before the opener — must not match.
		`2026-04-21 12:00:00 INFO panic: reported by user`,
		`2026-04-21 12:00:01 INFO fatal error: reported by user`,
		// Indented opener — must not match.
		"  panic: indented",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_ExitStatusClosesInclusively verifies that the
// `exit status N` line is included in the emitted region — it's
// the user-visible tail of a `go run` crash block.
func TestDetector_ExitStatusClosesInclusively(t *testing.T) {
	lines := linesFromStrings([]string{
		"panic: boom",
		"",
		"goroutine 1 [running]:",
		"main.main()",
		"\t/app/main.go:10",
		"exit status 2",
		"shell prompt",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Region must include the exit status line (line 6) but not the
	// trailing unrelated content (line 7).
	if a.StartLine != 1 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 1..6", a.StartLine, a.EndLine)
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
		{d.Name(), "traceback-go", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "log-traceback", "Category"},
		{d.Description(), "Go runtime tracebacks (panic: / fatal error: / goroutine N [state]:)", "Description"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// Severity is a package constant used by emit; check it explicitly
	// so a mistaken edit is caught.
	if severity != 0.8 {
		t.Errorf("severity = %v, want 0.8", severity)
	}
}

// TestDetector_EndToEnd_ViaIndexBuild confirms the detector plugs into
// the real index.Build pipeline and its anomalies surface in the
// UnifiedFileIndex.Anomalies list with Detector == detectorName.
func TestDetector_EndToEnd_ViaIndexBuild(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "simple_panic.log"))
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
	if a.StartLine != 2 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 2..9", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

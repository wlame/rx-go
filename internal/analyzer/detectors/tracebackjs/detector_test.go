package tracebackjs

// Unit and end-to-end tests for the traceback-js detector.
//
// Two layers, mirroring the tracebackjava / tracebackgo test layouts:
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
	// Window size of 128 matches the default used elsewhere; JS stack
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

// TestDetector_NodeStyleStack covers the baseline case: an Error
// opener + four parenthesized frames from a Node app.
func TestDetector_NodeStyleStack(t *testing.T) {
	got := feedFixture(t, "node_stack.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: server booting up
	//   2: TypeError: Cannot read properties of undefined (reading 'id')
	//   3: at Object.handler (/app/src/handlers/user.js:42:17)
	//   4: at Layer.handle [as handle_request] (/app/node_modules/express/lib/router/layer.js:95:5)
	//   5: at next (/app/node_modules/express/lib/router/route.js:137:13)
	//   6: at Route.dispatch (/app/node_modules/express/lib/router/route.js:112:3)
	//   7: server still running
	if a.StartLine != 2 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 2..6", a.StartLine, a.EndLine)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "5 lines") {
		t.Errorf("description = %q, want it to mention '5 lines'", a.Description)
	}
}

// TestDetector_BrowserStyleStack covers the shorter, bare-path frame
// shape typical of browser devtools exports: no function names, just
// bundle.js:line:col locations.
func TestDetector_BrowserStyleStack(t *testing.T) {
	got := feedFixture(t, "browser_stack.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1: page loaded
	//   2: ReferenceError: foo is not defined
	//   3: at bundle.js:1:234
	//   4: at bundle.js:1:512
	//   5: user clicked button
	if a.StartLine != 2 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 2..4", a.StartLine, a.EndLine)
	}
}

// TestDetector_UnhandledRejection covers Node's
// UnhandledPromiseRejectionWarning header. The first line in the
// fixture that looks like "(node:12345) UnhandledPromiseRejectionWarning: ..."
// has a `(node:N)` prefix so it does NOT match our anchored opener and
// is (correctly) not the start of the stack. The SECOND line (without
// the node-prefix) matches the opener and is followed by frames — this
// is the real stack the detector should find.
func TestDetector_UnhandledRejection(t *testing.T) {
	got := feedFixture(t, "unhandled_rejection.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture (1-based):
	//   1: worker starting
	//   2: (node:12345) UnhandledPromiseRejectionWarning: ... (ignored)
	//   3: UnhandledPromiseRejectionWarning: Error: database connection lost
	//   4: at Connection.onTimeout (/app/src/db.js:88:12)
	//   5: at Timeout._onTimeout (/app/src/db.js:144:9)
	//   6: at listOnTimeout (internal/timers.js:554:17)
	//   7: worker continuing
	if a.StartLine != 3 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 3..6", a.StartLine, a.EndLine)
	}
}

// TestDetector_PendingOpenerRejected guards the one-line lookahead
// rule: an Error-shaped line NOT followed by an `at ...` frame must
// not produce an anomaly. This is the core reason the detector uses
// a pending state — a log message like "TypeError: bad input" on its
// own is normal prose and must stay silent.
func TestDetector_PendingOpenerRejected(t *testing.T) {
	lines := linesFromStrings([]string{
		"TypeError: bad input received from client",
		"client request rejected",
		"moving on",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (opener not confirmed by at-line); %+v",
			len(got), got)
	}
}

// TestDetector_PendingOpenerAtEOFDropped confirms that a bare opener
// at EOF — never confirmed — is not emitted. Finalize must reset the
// pending state silently.
func TestDetector_PendingOpenerAtEOFDropped(t *testing.T) {
	lines := linesFromStrings([]string{
		"request handled",
		"TypeError: very suspicious",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (EOF opener never confirmed); %+v",
			len(got), got)
	}
}

// TestDetector_EOFStack covers a file that ends mid-stack (no trailing
// prose line). Finalize must emit the pending region.
func TestDetector_EOFStack(t *testing.T) {
	lines := linesFromStrings([]string{
		"Error: boom",
		"    at /app/x.js:1:1",
		"    at /app/y.js:2:2",
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

// TestDetector_MultipleStacksBackToBack confirms two stacks separated
// by nothing but a new opener are emitted as separate anomalies.
func TestDetector_MultipleStacksBackToBack(t *testing.T) {
	lines := linesFromStrings([]string{
		"request a",
		"TypeError: first failure",
		"    at fn (/app/a.js:1:1)",
		"RangeError: second failure",
		"    at fn (/app/b.js:2:2)",
		"request done",
	})
	got := feedLines(t, lines)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 2 || got[0].EndLine != 3 {
		t.Errorf("first span: start=%d end=%d, want 2..3",
			got[0].StartLine, got[0].EndLine)
	}
	if got[1].StartLine != 4 || got[1].EndLine != 5 {
		t.Errorf("second span: start=%d end=%d, want 4..5",
			got[1].StartLine, got[1].EndLine)
	}
}

// TestDetector_NonStackProse covers unrelated log content — must stay
// silent. Covers lookalikes too ("at" mid-prose, "Error" mid-prose).
func TestDetector_NonStackProse(t *testing.T) {
	lines := linesFromStrings([]string{
		"server started on port 3000",
		"    at first glance this looks like a frame but no colon-line-col",
		"no Error: here because this line starts with lowercase",
		"handled 42 requests in 1s",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_PartialOpenerShouldNotFire guards against a regression
// where a substring match would cause a mid-line `TypeError: ...` to
// open a stack. The opener regex is anchored with `^`.
func TestDetector_PartialOpenerShouldNotFire(t *testing.T) {
	lines := linesFromStrings([]string{
		// Leading timestamp/prefix before the Error — must not match.
		`2026-04-21 12:00:00 WARN TypeError: bad user input reported`,
		// An at-line follows — which would confirm if the opener had
		// matched. It didn't, so no anomaly.
		"    at fn (/app/x.js:1:1)",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_BareErrorOpener confirms the bare `Error:` form (base
// Error class, no subclass prefix) works. The regex `^\w*Error: `
// accepts zero or more word chars before `Error`, so `Error: ...` is
// valid. Common in minified code where the class identity is lost.
func TestDetector_BareErrorOpener(t *testing.T) {
	lines := linesFromStrings([]string{
		"Error: something went wrong",
		"    at fn (/app/x.js:1:1)",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
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
		{d.Name(), "traceback-js", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "log-traceback", "Category"},
		{d.Description(), "JavaScript / Node.js stack traces (Error: ... / at ...)", "Description"},
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
	content, err := os.ReadFile(filepath.Join("testdata", "node_stack.log"))
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
	if a.StartLine != 2 || a.EndLine != 6 {
		t.Errorf("span: start=%d end=%d, want 2..6", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

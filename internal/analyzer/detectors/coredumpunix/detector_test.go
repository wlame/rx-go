package coredumpunix

// Unit and end-to-end tests for the coredump-unix detector.
//
// Two layers, mirroring the traceback* detector test layouts:
//
//  1. Unit: drive a fresh Detector through a real Coordinator with
//     in-memory line input built from testdata/*.log fixtures or
//     synthetic line lists. Exercises the per-variant state machines
//     without going through index.Build.
//
//  2. End-to-end: one test drives the detector through index.Build
//     with Analyze=true on a real fixture so the full coordinator +
//     flush-context path is exercised. Asserts the anomaly surfaces
//     on UnifiedFileIndex.Anomalies with Detector == detectorName.

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
// fresh Detector through a real Coordinator. Returns anomalies from
// the detector directly (so tests see the semantic Category, not the
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
// offsets so callers can assert on offsets directly if desired.
func feedLines(t *testing.T, lines []rawLine) []analyzer.Anomaly {
	t.Helper()

	d := New()
	// Window size of 128 matches the default used elsewhere; the
	// coredump detector uses no lookback, but a realistic size still
	// exercises the normal path through the coordinator.
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

// TestDetector_SegfaultFixture covers the terse one-line case: a
// `Segmentation fault (core dumped)` line surrounded by unrelated
// prose. The region is just the single opener line.
func TestDetector_SegfaultFixture(t *testing.T) {
	got := feedFixture(t, "segfault.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: starting up the worker
	//   2: running task 42
	//   3: Segmentation fault (core dumped)
	//   4: shell returned 139
	if a.StartLine != 3 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 3..3", a.StartLine, a.EndLine)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "Segmentation fault") {
		t.Errorf("description = %q, want it to mention 'Segmentation fault'",
			a.Description)
	}
}

// TestDetector_SegfaultTwoLineTail covers the rarer shape where the
// `(core dumped)` tail appears on its own on the line AFTER a bare
// `Segmentation fault`. The region must span both lines.
func TestDetector_SegfaultTwoLineTail(t *testing.T) {
	lines := linesFromStrings([]string{
		"running binary",
		"Segmentation fault",
		"(core dumped)",
		"shell returned",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 2..3", a.StartLine, a.EndLine)
	}
}

// TestDetector_AsanFixture covers the AddressSanitizer variant. The
// region spans opener through ABORTING tail.
func TestDetector_AsanFixture(t *testing.T) {
	got := feedFixture(t, "asan.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: running tests with AddressSanitizer
	//   2: ==12345==ERROR: AddressSanitizer: heap-buffer-overflow...
	//   3: READ of size 4 at 0x602000000018 thread T0
	//   4-6: #0 #1 #2 frames
	//   7: 0x602000000018 is located 0 bytes to the right of ...
	//   8: SUMMARY: AddressSanitizer: heap-buffer-overflow ...
	//   9: ==12345==ABORTING
	//  10: test runner exiting with code 1
	if a.StartLine != 2 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 2..9", a.StartLine, a.EndLine)
	}
	if !strings.Contains(a.Description, "AddressSanitizer") {
		t.Errorf("description = %q, want it to mention 'AddressSanitizer'",
			a.Description)
	}
}

// TestDetector_KernelOopsFixture covers the kernel Call Trace variant.
// Region runs from `Call Trace:` through the last bracketed-timestamp
// line; the non-bracketed line after closes.
func TestDetector_KernelOopsFixture(t *testing.T) {
	got := feedFixture(t, "kernel_oops.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: [  123.456789] systemd[1]: Started ...      (pre-opener)
	//   2: [  124.000000] kernel BUG at fs/ext4/inode  (pre-opener)
	//   3: [  124.001234] Call Trace:                   (OPENER)
	//   4-7: [  124.001235..8] [<addr>] frames
	//   8: [  124.002000] ---[ end trace ... ]---
	//   9: [  125.000000] systemd[1]: Reached target
	// All lines 3..9 are bracketed-timestamp lines so the region
	// continues through the final pre-existing bracketed line. That
	// matches the rule "bracketed timestamp is continuation; first
	// non-bracketed line closes". If the log has nothing but bracketed
	// lines the region runs to EOF.
	if a.StartLine != 3 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 3..9", a.StartLine, a.EndLine)
	}
	if !strings.Contains(a.Description, "Kernel oops") {
		t.Errorf("description = %q, want it to mention 'Kernel oops'",
			a.Description)
	}
}

// TestDetector_KernelOopsClosesOnNonBracketed confirms a log that
// returns to non-kernel lines after the Call Trace ends emits a region
// that stops at the last bracketed-timestamp line.
func TestDetector_KernelOopsClosesOnNonBracketed(t *testing.T) {
	lines := linesFromStrings([]string{
		"normal application log line",
		"[   42.000000] Call Trace:",
		"[   42.000001]  [<ffffffff810a1234>] do_sync_write+0x56/0x78",
		"[   42.000002]  [<ffffffff810a1300>] vfs_write+0x90/0xa0",
		"application continues without kernel prefix",
		"another line",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 2..4 (stops at last bracketed line)",
			a.StartLine, a.EndLine)
	}
}

// TestDetector_StackSmashingFixture covers the stack-smashing variant.
// Region runs from the `*** stack smashing detected ***` banner through
// the `Aborted (core dumped)` tail.
func TestDetector_StackSmashingFixture(t *testing.T) {
	got := feedFixture(t, "stack_smashing.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Fixture layout (1-based):
	//   1: launching vulnerable binary
	//   2: about to call function with overflow
	//   3: *** stack smashing detected ***: terminated   (OPENER)
	//   4: ======= Backtrace: =========
	//   5-7: three libc/app frames
	//   8: ======= Memory map: ========
	//   9-10: memory-map entries
	//  11: Aborted (core dumped)
	//  12: parent shell reporting exit
	if a.StartLine != 3 || a.EndLine != 11 {
		t.Errorf("span: start=%d end=%d, want 3..11", a.StartLine, a.EndLine)
	}
	if !strings.Contains(a.Description, "Stack smashing") {
		t.Errorf("description = %q, want it to mention 'Stack smashing'",
			a.Description)
	}
}

// TestDetector_StackSmashingClosesOnBlank confirms a blank line closes
// the region even if the `Aborted (core dumped)` tail is missing
// (truncated log).
func TestDetector_StackSmashingClosesOnBlank(t *testing.T) {
	lines := linesFromStrings([]string{
		"running binary",
		"*** stack smashing detected ***: terminated",
		"======= Backtrace: =========",
		"/lib/x86_64-linux-gnu/libc.so.6(+0x1210e2)[0x7f2e9c8bf0e2]",
		"",
		"next log line after blank",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 4 {
		t.Errorf("span: start=%d end=%d, want 2..4", a.StartLine, a.EndLine)
	}
}

// TestDetector_NoOpenerNoAnomaly covers ordinary log content with no
// crash markers. Must stay silent.
func TestDetector_NoOpenerNoAnomaly(t *testing.T) {
	lines := linesFromStrings([]string{
		"application starting",
		"handled request /api/users in 42ms",
		"log line mentioning Segmentation fault in the middle of prose",
		"shutting down gracefully",
	})
	got := feedLines(t, lines)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (no column-0 opener); %+v", len(got), got)
	}
}

// TestDetector_OpenerAtEOF confirms a crash dump at EOF — no trailing
// prose or close line — is still emitted via Finalize.
func TestDetector_OpenerAtEOF(t *testing.T) {
	lines := linesFromStrings([]string{
		"running binary",
		"==99999==ERROR: AddressSanitizer: stack-buffer-overflow",
		"    #0 0x55a7d8f2a1bf in main /app/src/main.c:12:11",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 2..3", a.StartLine, a.EndLine)
	}
}

// TestDetector_BackToBackVariants confirms that two distinct crashes
// in a row (e.g. segfault followed by ASAN) emit two anomalies — the
// detector must close the first region and re-open on the second
// variant's opener.
func TestDetector_BackToBackVariants(t *testing.T) {
	lines := linesFromStrings([]string{
		"first process starting",
		"Segmentation fault (core dumped)",
		"==55555==ERROR: AddressSanitizer: heap-use-after-free",
		"    #0 0x400abc in func /app/x.c:1:1",
		"==55555==ABORTING",
		"done",
	})
	got := feedLines(t, lines)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 2 || got[0].EndLine != 2 {
		t.Errorf("first (segfault) span: start=%d end=%d, want 2..2",
			got[0].StartLine, got[0].EndLine)
	}
	if got[1].StartLine != 3 || got[1].EndLine != 5 {
		t.Errorf("second (asan) span: start=%d end=%d, want 3..5",
			got[1].StartLine, got[1].EndLine)
	}
}

// TestDetector_VariantClassification exercises classifyOpener directly
// on representative lines of each shape. This pins the combined-regex
// alternatives and the variant dispatch so a regex edit that accidentally
// confuses two variants fails loudly.
func TestDetector_VariantClassification(t *testing.T) {
	cases := []struct {
		line string
		want variant
	}{
		{"Segmentation fault", variantSegfault},
		{"Segmentation fault (core dumped)", variantSegfault},
		{"*** stack smashing detected ***: terminated", variantStackSmash},
		{"==42==ERROR: AddressSanitizer: heap-buffer-overflow", variantAsan},
		{"[   12.345] Call Trace:", variantKernel},
		{"[12.345] Call Trace:", variantKernel},
		{"", variantNone},
		{"random log line", variantNone},
		// Mid-line variants must NOT open — all openers are anchored
		// at column 0.
		{"got Segmentation fault during run", variantNone},
		{"prefix *** stack smashing detected ***", variantNone},
		// Kernel line without "Call Trace:" text must not open — the
		// bracketed timestamp alone isn't enough.
		{"[   12.345] regular kernel log line", variantNone},
	}
	for _, c := range cases {
		got := classifyOpener([]byte(c.line))
		if got != c.want {
			t.Errorf("classifyOpener(%q) = %v, want %v", c.line, got, c.want)
		}
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
		{d.Name(), "coredump-unix", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "log-crash", "Category"},
		{
			d.Description(),
			"Unix crash dumps (segfault / ASAN / kernel oops / stack smashing)",
			"Description",
		},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// Severity is a package constant used by emit; check it explicitly
	// so a mistaken edit is caught.
	if severity != 0.9 {
		t.Errorf("severity = %v, want 0.9", severity)
	}
}

// TestDetector_EndToEnd_ViaIndexBuild confirms the detector plugs into
// the real index.Build pipeline and its anomalies surface in the
// UnifiedFileIndex.Anomalies list with Detector == detectorName.
func TestDetector_EndToEnd_ViaIndexBuild(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "asan.log"))
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
	if a.StartLine != 2 || a.EndLine != 9 {
		t.Errorf("span: start=%d end=%d, want 2..9", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

package analyzer

import (
	"testing"
)

// callRecord is a single OnLine invocation captured by the tracking
// detector below. We copy bytes out of the borrowed slice because the
// window's buffers alias across pushes.
type callRecord struct {
	number       int64
	startOffset  int64
	endOffset    int64
	content      string
	indentPrefix int
	isBinary     bool
	lenAtCall    int // w.Len() at the time of the call — sanity check
}

// trackingDetector is a LineDetector used only in this file. Unlike
// the recorderDetector in linedetector_test.go, this one captures the
// full per-event payload plus the precomputed IndentPrefix / IsBinary
// flags so we can assert the coordinator is populating them correctly.
type trackingDetector struct {
	NoopAnalyzer

	calls []callRecord

	// emit is returned verbatim from Finalize so a test can verify the
	// coordinator aggregates across multiple detectors without touching
	// the slices.
	emit []Anomaly

	// flushSeen is captured during Finalize. Tests use it to confirm
	// the exact *FlushContext was passed through.
	flushSeen *FlushContext
}

func (t *trackingDetector) OnLine(w *Window) {
	ev := w.Current()
	t.calls = append(t.calls, callRecord{
		number:       ev.Number,
		startOffset:  ev.StartOffset,
		endOffset:    ev.EndOffset,
		content:      string(ev.Bytes), // string() from []byte always copies
		indentPrefix: ev.IndentPrefix,
		isBinary:     ev.IsBinary,
		lenAtCall:    w.Len(),
	})
}

func (t *trackingDetector) Finalize(flush *FlushContext) []Anomaly {
	t.flushSeen = flush
	return t.emit
}

// Compile-time checks that the tracking detector satisfies both
// interfaces. Catches interface drift faster than a failing test body.
var (
	_ FileAnalyzer = (*trackingDetector)(nil)
	_ LineDetector = (*trackingDetector)(nil)
)

func newTrackingDetector(name string) *trackingDetector {
	return &trackingDetector{
		NoopAnalyzer: NoopAnalyzer{NameValue: name, VersionValue: "0.1.0"},
	}
}

func TestCoordinator_DispatchesLinesInOrder(t *testing.T) {
	// Two detectors see every ProcessLine in the same order they were
	// delivered. Verifies the coordinator's per-line fan-out.
	d1 := newTrackingDetector("t1")
	d2 := newTrackingDetector("t2")
	c := NewCoordinator(16, []LineDetector{d1, d2})

	lines := []string{"alpha", "beta", "gamma"}
	for i, line := range lines {
		num := int64(i + 1)
		c.ProcessLine(num, num*10, num*10+int64(len(line)), []byte(line))
	}

	for _, d := range []*trackingDetector{d1, d2} {
		if len(d.calls) != len(lines) {
			t.Fatalf("%s: got %d calls, want %d", d.NameValue, len(d.calls), len(lines))
		}
		for i, want := range lines {
			got := d.calls[i]
			if got.content != want || got.number != int64(i+1) {
				t.Errorf("%s call[%d] = {num=%d, content=%q}, want {num=%d, content=%q}",
					d.NameValue, i, got.number, got.content, i+1, want)
			}
		}
	}
}

func TestCoordinator_PrecomputesIndentAndBinary(t *testing.T) {
	// The coordinator must compute IndentPrefix (tabs+spaces leader) and
	// IsBinary (any byte outside \t\n\r / printable ASCII) exactly once
	// per line, and the values must reach the detector via the window.
	d := newTrackingDetector("t")
	c := NewCoordinator(4, []LineDetector{d})

	cases := []struct {
		name       string
		line       []byte
		wantIndent int
		wantBinary bool
	}{
		{"no_indent", []byte("hello"), 0, false},
		{"spaces", []byte("    indented"), 4, false},
		{"tabs", []byte("\t\tdeep"), 2, false},
		{"mixed", []byte(" \t mixed"), 3, false},
		{"empty", []byte(""), 0, false},
		{"all_whitespace", []byte("   "), 3, false},
		// Bytes outside the printable ASCII + \t\n\r set mark the line
		// as binary. 0x01 is a control character that should flag.
		{"binary_byte", []byte("ok\x01bad"), 0, true},
		// High-bit byte (e.g., UTF-8 continuation) also flags as binary
		// because containsNonTextBytes is not Unicode-aware.
		{"high_bit", []byte("nm\xc3\xa9"), 0, true},
		// CR inside a line is allowed, does not flag binary.
		{"cr_allowed", []byte("carr\riage"), 0, false},
	}

	for i, tc := range cases {
		num := int64(i + 1)
		c.ProcessLine(num, 0, int64(len(tc.line)), tc.line)
	}

	if len(d.calls) != len(cases) {
		t.Fatalf("got %d calls, want %d", len(d.calls), len(cases))
	}
	for i, tc := range cases {
		got := d.calls[i]
		if got.indentPrefix != tc.wantIndent {
			t.Errorf("case %s: indent = %d, want %d", tc.name, got.indentPrefix, tc.wantIndent)
		}
		if got.isBinary != tc.wantBinary {
			t.Errorf("case %s: isBinary = %v, want %v", tc.name, got.isBinary, tc.wantBinary)
		}
	}
}

func TestCoordinator_WindowLenTracksPushes(t *testing.T) {
	// Each OnLine sees the window saturate at its size. With windowLines=3
	// and 5 pushes, Len() should be 1, 2, 3, 3, 3.
	d := newTrackingDetector("t")
	c := NewCoordinator(3, []LineDetector{d})

	for i := 1; i <= 5; i++ {
		c.ProcessLine(int64(i), 0, 1, []byte{'x'})
	}
	wantLens := []int{1, 2, 3, 3, 3}
	for i, want := range wantLens {
		if d.calls[i].lenAtCall != want {
			t.Errorf("call[%d] Len = %d, want %d", i, d.calls[i].lenAtCall, want)
		}
	}
}

func TestCoordinator_FinalizeAggregatesAnomalies(t *testing.T) {
	// Coordinator.Finalize concatenates each detector's anomaly slice
	// in detector order.
	d1 := newTrackingDetector("t1")
	d1.emit = []Anomaly{
		{StartLine: 1, EndLine: 2, Category: "a", Severity: 0.5},
		{StartLine: 5, EndLine: 5, Category: "a", Severity: 0.5},
	}
	d2 := newTrackingDetector("t2")
	d2.emit = []Anomaly{
		{StartLine: 10, EndLine: 11, Category: "b", Severity: 0.8},
	}

	c := NewCoordinator(8, []LineDetector{d1, d2})
	c.ProcessLine(1, 0, 1, []byte("x"))

	flush := &FlushContext{TotalLines: 1, MedianLineLength: 1, P99LineLength: 1}
	got := c.Finalize(flush)

	if len(got) != 3 {
		t.Fatalf("got %d anomalies, want 3", len(got))
	}
	if got[0].Category != "a" || got[1].Category != "a" || got[2].Category != "b" {
		t.Errorf("category order = [%s %s %s], want [a a b]",
			got[0].Category, got[1].Category, got[2].Category)
	}
	// The flush context must be forwarded verbatim to each detector.
	if d1.flushSeen != flush || d2.flushSeen != flush {
		t.Errorf("flush pointer not forwarded: d1=%p d2=%p want=%p", d1.flushSeen, d2.flushSeen, flush)
	}
}

func TestCoordinator_ZeroDetectorsIsNoop(t *testing.T) {
	// Fast path: with no detectors, ProcessLine does nothing and
	// Finalize returns nil. This guarantees users running `rx index`
	// without --analyze pay nothing for the coordinator.
	c := NewCoordinator(128, nil)
	// ProcessLine with nil window must not panic.
	for i := 0; i < 1000; i++ {
		c.ProcessLine(int64(i), 0, 1, []byte{'x'})
	}
	if got := c.Finalize(&FlushContext{}); got != nil {
		t.Errorf("Finalize with no detectors = %v, want nil", got)
	}
}

func TestCoordinator_ZeroDetectorsDoesNotAllocateWindow(t *testing.T) {
	// Directly inspect the internal field — this is a white-box test in
	// the same package, so we can verify the window is skipped entirely.
	// The contract is documented in NewCoordinator and worth nailing
	// down: future refactors that start allocating a Window for the
	// empty-detector case should fail this test.
	c := NewCoordinator(128, nil)
	if c.window != nil {
		t.Errorf("expected c.window to be nil when no detectors are registered")
	}

	// With detectors, we do allocate a window.
	c = NewCoordinator(128, []LineDetector{newTrackingDetector("t")})
	if c.window == nil {
		t.Errorf("expected c.window to be non-nil when detectors are present")
	}
}

// BenchmarkCoordinator_NoDetectors guards the fast path. Regressions
// that add work to the zero-detector case (e.g. always allocating a
// window or always computing indent/binary) will show up here.
func BenchmarkCoordinator_NoDetectors(b *testing.B) {
	c := NewCoordinator(128, nil)
	line := []byte("some typical log line with moderate length for benchmarking")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.ProcessLine(int64(i), 0, int64(len(line)), line)
	}
}

// BenchmarkCoordinator_OneDetector gives us a reference point for the
// "normal" cost of dispatch against a single trivial detector.
func BenchmarkCoordinator_OneDetector(b *testing.B) {
	d := newTrackingDetector("bench")
	c := NewCoordinator(128, []LineDetector{d})
	line := []byte("some typical log line with moderate length for benchmarking")
	// Warm up the window slots so we're not measuring slot growth.
	for i := 0; i < 256; i++ {
		c.ProcessLine(int64(i), 0, int64(len(line)), line)
	}
	// Reset the tracking detector's slice to keep memory bounded and
	// comparable across iterations. We only measure ProcessLine cost.
	d.calls = d.calls[:0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.ProcessLine(int64(i), 0, int64(len(line)), line)
	}
}

func TestCountIndentPrefix(t *testing.T) {
	// Unit-test the helper directly so regressions in the coordinator
	// don't mask regressions in the helper (and vice versa).
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 0},
		{"    abc", 4},
		{"\t\tabc", 2},
		{" \t abc", 3},
		{"    ", 4},
		{"\n", 0}, // \n is not a space/tab
	}
	for _, c := range cases {
		if got := countIndentPrefix([]byte(c.in)); got != c.want {
			t.Errorf("countIndentPrefix(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestContainsNonTextBytes(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"hello world", false},
		{"tabs\tare\tfine", false},
		{"\r\n", false},
		{"\x00", true},
		{"\x1f", true}, // one below 0x20
		{"\x7f", true}, // DEL
		{"\x80", true}, // high bit
		{"mixed\x01in", true},
		{"\x20", false}, // space boundary
		{"\x7e", false}, // ~ boundary
	}
	for _, c := range cases {
		if got := containsNonTextBytes([]byte(c.in)); got != c.want {
			t.Errorf("containsNonTextBytes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

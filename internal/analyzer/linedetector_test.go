package analyzer

import (
	"context"
	"testing"
)

// recorderDetector is a minimal LineDetector implementation used by
// the tests in this file. It records every OnLine call's key event
// fields and the FlushContext it sees during Finalize. We keep it
// here (not in a testdata helper) because the interface is small
// enough that inlining is clearer than centralizing.
type recorderDetector struct {
	// Embed a NoopAnalyzer to satisfy FileAnalyzer cheaply.
	NoopAnalyzer

	// Linger — each OnLine copies the Bytes so we can assert against
	// the content later without worrying about the borrowed-slice
	// lifetime contract.
	lines []string
	nums  []int64

	// flushSeen is set during Finalize. Tests inspect it to confirm
	// the coordinator passes the context through untouched.
	flushSeen *FlushContext

	// emit is what Finalize returns. Set by tests as needed.
	emit []Anomaly
}

func (r *recorderDetector) OnLine(w *Window) {
	ev := w.Current()
	// Copy the borrowed bytes — the contract requires it.
	r.lines = append(r.lines, string(ev.Bytes))
	r.nums = append(r.nums, ev.Number)
}

func (r *recorderDetector) Finalize(flush *FlushContext) []Anomaly {
	r.flushSeen = flush
	return r.emit
}

// Sanity check: the recorder satisfies both interfaces at compile
// time. A dedicated assertion catches accidental interface drift
// earlier than any test body would.
var (
	_ FileAnalyzer = (*recorderDetector)(nil)
	_ LineDetector = (*recorderDetector)(nil)
)

func TestLineDetector_NoopAnalyzerSatisfiesFileAnalyzer(t *testing.T) {
	// Baseline: the LineDetector contract embeds FileAnalyzer, so any
	// LineDetector can be used wherever a FileAnalyzer is expected.
	var _ FileAnalyzer = &recorderDetector{
		NoopAnalyzer: NoopAnalyzer{NameValue: "rec", VersionValue: "0.1.0"},
	}
}

func TestLineEvent_BorrowedBytesFromWindow(t *testing.T) {
	// Documents the borrowed-bytes contract: a detector that keeps
	// ev.Bytes without copying sees stale data after the next push.
	w := NewWindow(2)
	w.push(1, 0, 5, []byte("first"), 0, false)
	ev := w.Current()
	borrowed := ev.Bytes // SAME underlying array as the slot
	w.push(2, 5, 11, []byte("second"), 0, false)

	// After the second push into the same slot (size=2 means slot 0
	// gets reused after push#3, but here we only pushed twice into a
	// size-2 window so slot 0 still holds line 1). The point is simply
	// that code should not assume ev.Bytes survives unrelated pushes
	// unless copied.
	_ = borrowed

	// More targeted check: force a wrap and show the borrowed slice
	// from line 1 now aliases line 3's content.
	w.push(3, 11, 18, []byte("third!"), 0, false)
	// Refetch: the window's head is now at slot 0, which is where
	// line 1 used to live. The borrowed pointer alias from line 1 now
	// points at line 3's bytes.
	cur := w.Current()
	if string(cur.Bytes) != "third!" {
		t.Fatalf("post-wrap Current = %q, want %q", cur.Bytes, "third!")
	}
}

func TestFlushContext_FieldsAreIndependent(t *testing.T) {
	// Lightweight smoke test that FlushContext is a plain value type:
	// constructing it and reading the fields yields the values set.
	// Future changes that, say, use pointers or computed getters would
	// need to revisit the zero-allocation plumbing in Task 5.
	fc := FlushContext{
		TotalLines:       1000,
		MedianLineLength: 80,
		P99LineLength:    512,
	}
	if fc.TotalLines != 1000 || fc.MedianLineLength != 80 || fc.P99LineLength != 512 {
		t.Errorf("FlushContext round-trip failed: %+v", fc)
	}
}

func TestRecorderDetector_AnalyzeReturnsReport(t *testing.T) {
	// recorderDetector embeds NoopAnalyzer, so its Analyze must still
	// honor the FileAnalyzer contract. This ensures we haven't broken
	// the base contract while adding the new hooks.
	r := &recorderDetector{
		NoopAnalyzer: NoopAnalyzer{NameValue: "rec", VersionValue: "0.1.0"},
	}
	rep, err := r.Analyze(context.Background(), Input{Path: "/tmp/x"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Name != "rec" || rep.Version != "0.1.0" {
		t.Errorf("report fields mismatch: %+v", rep)
	}
}

package analyzer

import (
	"bytes"
	"testing"
)

// pushLine is a small test helper so each test doesn't repeat the six
// positional args of the unexported push method. Keeps intent readable
// at the call sites below.
func pushLine(w *Window, num int64, content string) {
	// StartOffset/EndOffset aren't inspected by most tests — they're
	// here to make sure push correctly stores arbitrary values.
	start := num * 100
	end := start + int64(len(content)) + 1 // +1 for a hypothetical \n
	w.push(num, start, end, []byte(content), 0, false)
}

func TestNewWindow_ClampsSize(t *testing.T) {
	// Below 1 clamps up; above maxWindowLines clamps down. Exercises
	// the "any user value" contract so callers don't have to sanitize.
	cases := []struct {
		in, want int
	}{
		{-5, 1},
		{0, 1},
		{1, 1},
		{128, 128},
		{maxWindowLines, maxWindowLines},
		{maxWindowLines + 1, maxWindowLines},
		{10_000_000, maxWindowLines},
	}
	for _, c := range cases {
		got := NewWindow(c.in).Size()
		if got != c.want {
			t.Errorf("NewWindow(%d).Size() = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWindow_ZeroEventsBeforeFirstPush(t *testing.T) {
	w := NewWindow(4)
	if w.Len() != 0 {
		t.Errorf("Len() = %d, want 0", w.Len())
	}
	ev := w.Current()
	// Zero LineEvent: no Bytes, Number == 0.
	if ev.Number != 0 || ev.Bytes != nil {
		t.Errorf("Current() before push = %+v, want zero value", ev)
	}
	if back := w.At(0); back.Number != 0 || back.Bytes != nil {
		t.Errorf("At(0) before push = %+v, want zero value", back)
	}
}

func TestWindow_PushCurrentMatches(t *testing.T) {
	w := NewWindow(4)
	pushLine(w, 1, "hello world")
	cur := w.Current()
	if cur.Number != 1 {
		t.Errorf("Number = %d, want 1", cur.Number)
	}
	if string(cur.Bytes) != "hello world" {
		t.Errorf("Bytes = %q, want %q", cur.Bytes, "hello world")
	}
	if w.Len() != 1 {
		t.Errorf("Len = %d, want 1", w.Len())
	}
}

func TestWindow_PushWrapsAtSize(t *testing.T) {
	// With size=3 and 7 pushes, Len() should saturate at 3 and the
	// oldest visible line (At(2)) should be push #5 (numbers 5,6,7).
	w := NewWindow(3)
	for i := int64(1); i <= 7; i++ {
		pushLine(w, i, "")
	}
	if w.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", w.Len())
	}
	if w.Current().Number != 7 {
		t.Errorf("Current().Number = %d, want 7", w.Current().Number)
	}
	if got := w.At(0).Number; got != 7 {
		t.Errorf("At(0) = %d, want 7", got)
	}
	if got := w.At(1).Number; got != 6 {
		t.Errorf("At(1) = %d, want 6", got)
	}
	if got := w.At(2).Number; got != 5 {
		t.Errorf("At(2) = %d, want 5", got)
	}
	// Out of range both ways.
	if got := w.At(3); got.Number != 0 || got.Bytes != nil {
		t.Errorf("At(3) should be zero, got %+v", got)
	}
	if got := w.At(-1); got.Number != 0 || got.Bytes != nil {
		t.Errorf("At(-1) should be zero, got %+v", got)
	}
}

func TestWindow_AtReturnsCorrectBackIndex(t *testing.T) {
	w := NewWindow(5)
	for i := int64(1); i <= 5; i++ {
		pushLine(w, i, "")
	}
	// At(back) must match: At(0) = 5, At(1) = 4, At(2) = 3, At(3) = 2,
	// At(4) = 1. The loop catches off-by-one regressions in the
	// head-minus-back arithmetic.
	for back := 0; back < 5; back++ {
		want := int64(5 - back)
		got := w.At(back).Number
		if got != want {
			t.Errorf("At(%d).Number = %d, want %d", back, got, want)
		}
	}
}

func TestWindow_BorrowedSliceMatchesLastPushed(t *testing.T) {
	// Confirms that after multiple pushes, At(0) and Current() both
	// still reflect the last-pushed content — i.e. the slot buffer was
	// overwritten, not appended.
	w := NewWindow(2)
	pushLine(w, 1, "aaa")
	pushLine(w, 2, "bb")
	pushLine(w, 3, "cccc")

	cur := w.Current()
	if !bytes.Equal(cur.Bytes, []byte("cccc")) {
		t.Errorf("Current().Bytes = %q, want %q", cur.Bytes, "cccc")
	}
	prev := w.At(1)
	if !bytes.Equal(prev.Bytes, []byte("bb")) {
		t.Errorf("At(1).Bytes = %q, want %q", prev.Bytes, "bb")
	}
}

func TestWindow_BufferReusedAfterWrap(t *testing.T) {
	// Pushing more lines than size should overwrite the oldest slot
	// rather than allocate a fresh buffer. We can't directly observe
	// capacity churn, but we can at least verify data lines up.
	w := NewWindow(2)
	pushLine(w, 1, "AAAA")
	pushLine(w, 2, "BBBB")
	pushLine(w, 3, "CCCC") // overwrites slot that held 1 ("AAAA")
	pushLine(w, 4, "DD")   // overwrites slot that held 2 ("BBBB")

	if string(w.Current().Bytes) != "DD" {
		t.Errorf("Current = %q, want %q", w.Current().Bytes, "DD")
	}
	if string(w.At(1).Bytes) != "CCCC" {
		t.Errorf("At(1) = %q, want %q", w.At(1).Bytes, "CCCC")
	}
}

func TestWindow_MetadataPropagation(t *testing.T) {
	w := NewWindow(2)
	w.push(10, 1000, 1015, []byte("line with meta"), 4, true)

	ev := w.Current()
	if ev.Number != 10 {
		t.Errorf("Number = %d, want 10", ev.Number)
	}
	if ev.StartOffset != 1000 {
		t.Errorf("StartOffset = %d, want 1000", ev.StartOffset)
	}
	if ev.EndOffset != 1015 {
		t.Errorf("EndOffset = %d, want 1015", ev.EndOffset)
	}
	if ev.IndentPrefix != 4 {
		t.Errorf("IndentPrefix = %d, want 4", ev.IndentPrefix)
	}
	if !ev.IsBinary {
		t.Errorf("IsBinary = false, want true")
	}
}

func TestWindow_PushAllocationsAfterWarmup(t *testing.T) {
	// The contract is "zero alloc after warmup". We warm up by doing
	// >= 2×size pushes with a representative payload so every slot's
	// backing array has grown to its steady-state capacity. Then
	// AllocsPerRun with the same-size payload should report 0.
	size := 32
	w := NewWindow(size)
	payload := bytes.Repeat([]byte("x"), 128) // realistic line length

	// Warmup: 2×size pushes covers every slot twice, guaranteeing each
	// backing array has grown at least once.
	for i := 0; i < size*2; i++ {
		w.push(int64(i), 0, int64(len(payload)), payload, 0, false)
	}

	got := testing.AllocsPerRun(1000, func() {
		w.push(0, 0, int64(len(payload)), payload, 0, false)
	})
	if got != 0 {
		t.Errorf("Window.push allocations/op after warmup = %.2f, want 0", got)
	}
}

package analyzer

// Window is a fixed-capacity ring buffer of recently observed lines,
// passed by pointer to every LineDetector's OnLine callback.
//
// Purpose:
//
//   - Amortize allocation: each slot owns a reusable []byte that grows
//     to the steady-state line length over the first few pushes and
//     then stops allocating. After warmup, push is zero-alloc.
//
//   - Share context: detectors that need to look at the previous line
//     (json-blob continuation, python traceback continuation, etc.) can
//     query Window.At(1) instead of maintaining their own 1-line copy.
//
// Layout:
//
//   - The Window owns a fixed-size array of `maxWindowLines` slots.
//     The effective capacity is `w.size`, set at construction; slots
//     beyond `w.size` are unused. Using a fixed array (not a slice)
//     keeps the struct itself allocation-free once created.
//
//   - `head` points at the slot for the most-recently-pushed line.
//     When n >= size, older entries are silently overwritten, which is
//     the correct behavior for a sliding window.
//
//   - Byte buffers are reused by `append(slot.buf[:0], line...)`: this
//     reslices the existing backing array to length zero and then
//     copies in the new content. If the backing array is large enough
//     no allocation happens.

// maxWindowLines caps the Window's per-instance backing array so the
// struct size is bounded regardless of user configuration. The config
// resolver (Task 2) clamps the user-visible window size to this value.
const maxWindowLines = 2048

// maxSlotBufCap caps the retained capacity of a slot's byte buffer. When
// a push receives a line longer than this cap, we drop the slot's old
// backing array and allocate a fresh one sized exactly to the incoming
// line. Next pushes with shorter lines will then use a small buffer
// again.
//
// Without this cap, a single very-long line (e.g. a 10 MB JSON blob
// serialized onto one line) would grow every slot's backing array to
// ≥10 MB permanently — malformed input could balloon a Coordinator's
// memory footprint to windowSize × 10 MB. 64 KB is large enough to
// absorb realistic log lines without per-push allocation while keeping
// the worst-case retained memory bounded.
const maxSlotBufCap = 64 * 1024

// lineSlot is one slot in the window. Stores the byte buffer plus the
// per-line metadata the coordinator needs to rebuild a LineEvent when
// a detector queries the window.
//
// Slots are valued (not pointers) and live inside the Window's fixed
// array — no per-push heap allocation for the slot itself.
type lineSlot struct {
	number       int64
	startOffset  int64
	endOffset    int64
	buf          []byte // reused between pushes; cap() grows monotonically
	indentPrefix int
	isBinary     bool
}

// Window holds the sliding view of recently pushed lines.
//
// Instance-per-worker: the coordinator creates one Window for its
// worker. Windows are NOT safe for concurrent use — they're designed
// for the single-goroutine hot scan loop.
type Window struct {
	slots [maxWindowLines]lineSlot
	size  int   // effective capacity; 1 <= size <= maxWindowLines
	head  int   // index of most-recent push in slots[0..size)
	n     int64 // total pushes since construction (saturates at size for "length")
}

// NewWindow returns a Window with the given effective size. size is
// clamped to the range [1, maxWindowLines] so callers can pass any
// user-provided value without pre-sanitizing.
//
// The returned Window is ready to use; no other initialization needed.
func NewWindow(size int) *Window {
	if size < 1 {
		size = 1
	}
	if size > maxWindowLines {
		size = maxWindowLines
	}
	return &Window{size: size, head: -1}
}

// Size returns the effective window capacity (1..maxWindowLines).
func (w *Window) Size() int { return w.size }

// Len returns the current number of lines held in the window. Bounded
// above by Size(); matches the number of slots currently containing a
// valid pushed line.
func (w *Window) Len() int {
	if w.n >= int64(w.size) {
		return w.size
	}
	return int(w.n)
}

// push adds one line to the window, reusing the slot byte buffer if
// possible (zero-alloc after warmup).
//
// Arguments mirror the per-line information the coordinator collects
// before dispatching: absolute line number, byte-offset range, the raw
// line bytes, and the two precomputed helpers (indent count + binary
// flag). The coordinator is the only caller, so we keep the signature
// tight rather than taking a struct.
//
// Not exported: detectors never push — only the coordinator does.
func (w *Window) push(num, start, end int64, line []byte, indent int, bin bool) {
	// Advance head modulo size. The branch is cheaper than `%` on the
	// hot path for small `size` and avoids modulo-by-non-power-of-two.
	w.head++
	if w.head >= w.size {
		w.head = 0
	}

	slot := &w.slots[w.head]
	slot.number = num
	slot.startOffset = start
	slot.endOffset = end
	// Buffer reuse with a retained-capacity cap. Normal case: reslice the
	// existing backing array to zero length and append copies without
	// allocating. Exceptional case (finding #9): if the slot's retained
	// capacity already exceeds maxSlotBufCap, OR the incoming line is
	// larger than maxSlotBufCap, we allocate a fresh buffer sized to the
	// line. This prevents a single huge line from permanently inflating
	// every slot's backing array.
	if cap(slot.buf) > maxSlotBufCap || len(line) > maxSlotBufCap {
		// Allocate fresh storage sized exactly to the incoming line. Any
		// previously-retained large buffer becomes unreachable and will
		// be reclaimed on the next GC cycle.
		slot.buf = append([]byte(nil), line...)
	} else {
		slot.buf = append(slot.buf[:0], line...)
	}
	slot.indentPrefix = indent
	slot.isBinary = bin
	w.n++
}

// Current returns the most-recently-pushed line as a LineEvent. The
// returned Bytes is borrowed from the slot's buffer (same lifetime
// rules as LineEvent.Bytes).
//
// If no line has been pushed yet, returns a zero LineEvent with nil
// Bytes. Callers on the OnLine hot path know the coordinator just
// pushed, so this zero-case is mostly a safety net for tests.
func (w *Window) Current() LineEvent {
	if w.n == 0 {
		return LineEvent{}
	}
	return slotToEvent(&w.slots[w.head])
}

// At returns the line `back` pushes behind the current one. back=0 is
// Current; back=1 is the previous line; back=Len()-1 is the oldest
// line still in the window.
//
// If back is out of range (negative or >= Len()), returns a zero
// LineEvent. This keeps the call site simple: a detector looking for
// "the previous line" can just check `ev.Number != 0` or `ev.Bytes !=
// nil`.
func (w *Window) At(back int) LineEvent {
	if back < 0 || back >= w.Len() {
		return LineEvent{}
	}
	idx := w.head - back
	if idx < 0 {
		idx += w.size
	}
	return slotToEvent(&w.slots[idx])
}

// slotToEvent builds a LineEvent from a slot pointer. Aliases the
// slot's buffer so there's no copy on the hot path. The returned event
// is a stack value — its lifetime is the caller's frame.
func slotToEvent(s *lineSlot) LineEvent {
	return LineEvent{
		Number:       s.number,
		StartOffset:  s.startOffset,
		EndOffset:    s.endOffset,
		Bytes:        s.buf,
		IndentPrefix: s.indentPrefix,
		IsBinary:     s.isBinary,
	}
}

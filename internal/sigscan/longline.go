package sigscan

// The long-line measure.
//
// A line is long when its length is at least
// max(min_bytes, factor × the median length of its neighbors): the
// window lines before it and the window lines after it, fewer at the
// two ends of the text. The median of an even count is the lower of the
// two middle values. A line with no neighbor (a text of one line) is
// never long. Lengths are full lengths, without the line ending,
// however much of the line the view holds.
//
// Only a line of min_bytes or more can be long, so the median is worked
// out for those lines only, and by counting rather than sorting: lengths
// are whole numbers, so factor × median ≤ length exactly when the
// median is at most length / factor (integer division), and the lower
// median of n values is at most x exactly when at least (n-1)/2 + 1 of
// them are. That is one pass over the 2·window neighbors, no sort and
// no allocation.
//
// A chunk's walk keeps the lengths of its last 2·window+1 lines in a
// ring and decides a line once the window lines after it are in the
// ring. A line within window lines of the chunk's first or last line
// has neighbors in other chunks: the walk hands it to the merge as an
// edge candidate, with the lengths of the chunk's first and last
// 2·window lines. The merge keeps the lengths of the lines it has seen
// near the merge point, and decides each edge candidate, in line order,
// once the neighbors on both sides are known (at the end of the text,
// with the neighbors there are).

// longLineSpec is a long-line measure's parameters.
type longLineSpec struct {
	window, factor, minBytes int64
}

// longAgainst reports whether a line of the given length is long
// against its neighbors' lengths, given in up to two slices (the parts
// of the window before and after the line).
func (s longLineSpec) longAgainst(length int64, before, after []int64) bool {
	n := len(before) + len(after)
	if length < s.minBytes || n == 0 {
		return false
	}
	limit := length / s.factor
	atMost := 0
	for _, v := range before {
		if v <= limit {
			atMost++
		}
	}
	for _, v := range after {
		if v <= limit {
			atMost++
		}
	}
	return atMost >= (n-1)/2+1
}

// longCandidate is a line of min_bytes or more, waiting for its
// neighbors: where it is, its length, and what its finding would hold.
type longCandidate struct {
	// idx is the line's index: chunk-relative in a walk, from 0 at the
	// first line of the text in the merge.
	idx, length, start, next int64
	excerpt                  []byte
	hasEx, masked            bool
	tm                       timeRef
}

// emit adds the candidate to out as a finding.
func (c *longCandidate) emit(cs *compiledSig, out sink) {
	out.add(cs, &rawFinding{startIdx: c.idx, endIdx: c.idx, startOff: c.start, endOff: c.next,
		excerpt: c.excerpt, hasEx: c.hasEx, vs: -1, ve: -1, masked: c.masked, tm: c.tm})
}

// longLineEdge is what a chunk hands the merge for a long-line measure:
// the lengths of its first and last min(2·window, lines) lines, and the
// candidates within window lines of its edges, in line order.
type longLineEdge struct {
	head, tail []int64
	cands      []longCandidate
}

// longLineWalker runs a long-line measure over one chunk's lines.
type longLineWalker struct {
	cs   *compiledSig
	spec longLineSpec
	ex   *excerpter
	// ring holds the lengths of the last 2·window+1 lines, line i at
	// i % len(ring).
	ring []int64
	head []int64
	// queue are the candidates at window lines or more from the chunk's
	// start, waiting for the window lines after them; at most window+1.
	queue []longCandidate
	// edge are the candidates the merge decides, in line order.
	edge []longCandidate
	// free are excerpt buffers of candidates already decided, reused.
	free  [][]byte
	lines int64
}

func newLongLineWalker(cs *compiledSig, ex *excerpter) *longLineWalker {
	return &longLineWalker{cs: cs, spec: *cs.long, ex: ex, ring: make([]int64, 2*cs.long.window+1)}
}

// line takes line l, at index idx of the chunk. times gives a candidate
// the time of its line, and out takes the findings decided here.
func (w *longLineWalker) line(l *line, idx int64, times *timeTracker, out sink) {
	window := w.spec.window
	size := int64(len(w.ring))
	w.ring[idx%size] = l.length
	if idx < 2*window {
		w.head = append(w.head, l.length)
	}
	w.lines = idx + 1
	// This line is the last neighbor of the queued candidate window
	// lines back: its window, idx-2·window to idx, is all in the ring.
	if len(w.queue) > 0 && w.queue[0].idx == idx-window {
		c := &w.queue[0]
		at := c.idx % size
		if w.spec.longAgainst(c.length, w.ring[:at], w.ring[at+1:]) {
			c.emit(w.cs, out)
		}
		if c.excerpt != nil {
			w.free = append(w.free, c.excerpt[:0])
		}
		w.queue = w.queue[1:]
	}
	if l.length < w.spec.minBytes {
		return
	}
	c := longCandidate{idx: idx, length: l.length, start: l.start, next: l.next, tm: times.at(l.start)}
	if out.mayStore() {
		c.excerpt, c.masked = w.ex.build(w.buffer(), l.view)
		c.hasEx = true
	}
	if idx < window {
		w.edge = append(w.edge, c)
		return
	}
	w.queue = append(w.queue, c)
}

// buffer returns an empty excerpt buffer, a reused one when there is.
func (w *longLineWalker) buffer() []byte {
	if n := len(w.free); n > 0 {
		b := w.free[n-1]
		w.free = w.free[:n-1]
		return b
	}
	return nil
}

// finish returns the chunk's edge for the merge: the candidates still
// queued are within window lines of its last line.
func (w *longLineWalker) finish() *longLineEdge {
	k := min(2*w.spec.window, w.lines)
	size := int64(len(w.ring))
	tail := make([]int64, k)
	for i := range k {
		tail[i] = w.ring[(w.lines-k+i)%size]
	}
	return &longLineEdge{head: w.head, tail: tail, cands: append(w.edge, w.queue...)}
}

// longLineMerge decides a long-line measure's edge candidates in the
// merge.
type longLineMerge struct {
	spec longLineSpec
	// lengths are the lengths of lines bufStart, bufStart+1, …: the lines
	// just before the merge point, as many as the queued candidates and
	// the next chunk's candidates can need.
	lengths  []int64
	bufStart int64
	// queue are the candidates waiting for neighbors after them, in line
	// order, with indexes from the first line of the text.
	queue []longCandidate
}

// add merges a chunk of n lines that starts at line index base: its
// candidates near its start are decided (with the chunks before it),
// then its own findings are taken, then its candidates near its end wait
// for the chunks after it. That keeps the findings in line order.
func (lm *longLineMerge) add(m *merger, si int32, sc *sigChunk, base, n int64) {
	edge := sc.long
	cs := m.e.sigs[si]
	out := m.storeSink(si, 1, true)
	lm.lengths = append(lm.lengths, edge.head...)
	k := 0
	for ; k < len(edge.cands) && edge.cands[k].idx < lm.spec.window; k++ {
		lm.enqueue(edge.cands[k], base)
	}
	lm.decide(cs, out, false)
	m.take(si, sc.findings, sc.count)
	headLen, tailFrom := int64(len(edge.head)), n-int64(len(edge.tail))
	if tailFrom <= headLen {
		// The head and the tail cover every line of the chunk.
		lm.lengths = append(lm.lengths, edge.tail[headLen-tailFrom:]...)
	} else {
		// INVARIANT: the queue is empty here. The chunk has more than
		// 4·window lines, so its head holds 2·window lengths: every
		// candidate within window lines of its start, and every earlier
		// one, found all its neighbors there and was decided above.
		lm.lengths = append(lm.lengths[:0], edge.tail...)
		lm.bufStart = base + tailFrom
	}
	for ; k < len(edge.cands); k++ {
		lm.enqueue(edge.cands[k], base)
	}
	lm.trim()
}

// enqueue adds a chunk's candidate, renumbered from the text's start.
func (lm *longLineMerge) enqueue(c longCandidate, base int64) {
	c.idx += base
	lm.queue = append(lm.queue, c)
}

// decide decides the queued candidates whose window lines after them
// are known, in order; at the end of the text (final) all of them, with
// the neighbors there are.
func (lm *longLineMerge) decide(cs *compiledSig, out sink, final bool) {
	window := lm.spec.window
	end := lm.bufStart + int64(len(lm.lengths))
	for len(lm.queue) > 0 {
		c := &lm.queue[0]
		if !final && c.idx+window >= end {
			return
		}
		lo, hi := max(c.idx-window, 0), min(c.idx+window, end-1)
		at := c.idx - lm.bufStart
		if lm.spec.longAgainst(c.length, lm.lengths[lo-lm.bufStart:at], lm.lengths[at+1:hi-lm.bufStart+1]) {
			c.emit(cs, out)
		}
		lm.queue = lm.queue[1:]
	}
}

// trim drops the lengths no candidate can need any more: those more
// than window lines before the first queued candidate, and before the
// last window lines (the window of the next chunk's first lines).
func (lm *longLineMerge) trim() {
	end := lm.bufStart + int64(len(lm.lengths))
	keep := end - lm.spec.window
	if len(lm.queue) > 0 {
		keep = min(keep, lm.queue[0].idx-lm.spec.window)
	}
	if drop := keep - lm.bufStart; drop > 0 {
		lm.lengths = append(lm.lengths[:0], lm.lengths[drop:]...)
		lm.bufStart = keep
	}
}

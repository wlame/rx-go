package sigscan

import (
	"slices"
)

// The time-gap and time-backwards measures look at the stamped lines
// only, each against the stamped line before it in the file.
//
// time-backwards: a line stamped earlier than the stamped line before
// it by more than tolerance. The finding is that line.
//
// time-gap: a silence of at least max(min_gap, factor × the median
// interval) between two consecutive stamped lines. The finding runs
// from the line just before the later stamped line (the earlier stamped
// line, or an unstamped line after it, such as a traceback's last line)
// to that stamped line. The median interval is taken over every pair
// of consecutive stamped lines, a step back counted as 0, and is known
// only at the end: so every candidate, a pair at least min_gap apart,
// is kept until then. The intervals are counted in buckets that keep
// three significant digits (exact below one second; 61,234 ms is
// counted as 61,200), the median being the lower bound of the bucket
// that holds it. At most maxGapCandidates (or the cap, when it is
// larger) candidates are kept: past that, only the longest silences are
// kept, dropping every candidate of the shortest length kept so far, so
// what is kept does not depend on the order the chunks are merged in,
// nor on where the file is cut: the merge takes each chunk's cutoff.
// Such a file then reports only silences longer than the longest one
// dropped, and its count is a lower bound.
//
// Across chunks: a chunk reports its first stamped line (and, for
// time-gap, the line just before it, when that line is in the chunk),
// and the merge pairs it with the last stamped line before the chunk;
// time-gap also needs the chunk's last line, which may be the line just
// before the next chunk's first stamped line.

// maxGapCandidates is the fewest silences time-gap keeps until the end
// of a scan.
const maxGapCandidates = 1 << 16

// lineRef is one line as the start of a finding: where it is, and the
// excerpt and time a finding that starts there holds. ok is false for
// no line.
type lineRef struct {
	idx, start int64
	// length is the line's length without its ending.
	length        int64
	excerpt       []byte
	hasEx, masked bool
	tm            timeRef
	ok            bool
}

// prevLine is the line before the current one in a walk, for time-gap:
// its view (borrowed), length, start and time.
type prevLine struct {
	view   []byte
	length int64
	start  int64
	tm     timeRef
}

// ref returns the line idx (the walk's previous line) as a lineRef,
// with its excerpt when out may store it.
func (p *prevLine) ref(idx int64, ex *excerpter, out sink) lineRef {
	r := lineRef{idx: idx, start: p.start, length: p.length, tm: p.tm, ok: true}
	if out.mayStore() {
		r.excerpt, r.masked = ex.build(nil, p.view)
		r.hasEx = true
	}
	return r
}

// stampedLine is a stamped line as the end of a finding or a pair:
// where it is, its length without its ending, and its timestamp.
type stampedLine struct {
	idx, start, next, length, ms int64
	// off is the zone offset the line writes, minutes east of UTC.
	off int
}

// backSpec is a time-backwards measure's parameters, in milliseconds.
type backSpec struct {
	tolerance int64
}

// backFirst is a chunk's first stamped line for time-backwards, with
// what its finding would hold.
type backFirst struct {
	line          stampedLine
	excerpt       []byte
	hasEx, masked bool
}

// backWalker runs time-backwards over one chunk.
type backWalker struct {
	cs    *compiledSig
	spec  backSpec
	ex    *excerpter
	exBuf []byte
	prev  stampRef
	first *backFirst
}

// line takes line l, at index idx of the chunk.
func (w *backWalker) line(l *line, idx int64, out sink) {
	if !l.stamped {
		return
	}
	switch {
	case !w.prev.ok:
		f := &backFirst{line: stampedLine{idx: idx, start: l.start, next: l.next, length: l.length, ms: l.ms, off: l.off}}
		if out.mayStore() {
			f.excerpt, f.masked = w.ex.build(nil, l.view)
			f.hasEx = true
		}
		w.first = f
	case w.prev.ms-l.ms > w.spec.tolerance:
		f := rawFinding{startIdx: idx, endIdx: idx, startOff: l.start, endOff: l.next, firstLen: l.length, vs: -1, ve: -1,
			tm: timeRef{ms: l.ms, off: l.off, state: timeKnown}}
		if out.storing() {
			w.exBuf, f.masked = w.ex.build(w.exBuf[:0], l.view)
			f.excerpt, f.hasEx = w.exBuf, true
		}
		out.add(w.cs, &f)
	}
	w.prev = stampRef{ms: l.ms, start: l.start, off: l.off, ok: true}
}

// mergeBackwards merges a chunk of a time-backwards measure: its first
// stamped line against the last one before the chunk (carry), then the
// chunk's own findings, which come after it.
func (m *merger) mergeBackwards(si int32, sc *sigChunk, base int64) {
	cs := m.e.sigs[si]
	if f := sc.back; f != nil && m.lastStamp.ok && m.lastStamp.ms-f.line.ms > cs.back.tolerance {
		m.storeSink(si, 1, true).add(cs, &rawFinding{startIdx: base + f.line.idx, endIdx: base + f.line.idx,
			startOff: f.line.start, endOff: f.line.next, firstLen: f.line.length, excerpt: f.excerpt, hasEx: f.hasEx, masked: f.masked,
			vs: -1, ve: -1, tm: timeRef{ms: f.line.ms, off: f.line.off, state: timeKnown}})
	}
	m.take(si, sc.findings, sc.count)
}

// gapSpec is a time-gap measure's parameters, min_gap in milliseconds.
type gapSpec struct {
	minGap int64
	factor float64
}

// gapCandidate is a pair of consecutive stamped lines at least min_gap
// apart: the finding it would be, and the interval.
type gapCandidate struct {
	delta int64
	from  lineRef // the line just before the later stamped line
	to    stampedLine
}

// gapFirst is a chunk's first stamped line for time-gap, and the line
// just before it when that line is in the chunk (before.ok).
type gapFirst struct {
	line   stampedLine
	before lineRef
}

// gapEdge is what a chunk hands the merge for a time-gap measure.
type gapEdge struct {
	first     *gapFirst
	intervals intervalCounts
	kept      gapKeeper
}

// gapWalker runs time-gap over one chunk.
type gapWalker struct {
	spec gapSpec
	ex   *excerpter
	prev stampRef
	edge gapEdge
}

func newGapWalker(cs *compiledSig, ex *excerpter, limit int) *gapWalker {
	return &gapWalker{spec: *cs.gap, ex: ex, edge: gapEdge{kept: gapKeeper{limit: limit, cutoff: -1}}}
}

// line takes line l, at index idx of the chunk; before is the line
// before it.
func (w *gapWalker) line(l *line, idx int64, before *prevLine, out sink) {
	if !l.stamped {
		return
	}
	here := stampedLine{idx: idx, start: l.start, next: l.next, length: l.length, ms: l.ms, off: l.off}
	if !w.prev.ok {
		f := &gapFirst{line: here}
		if idx > 0 {
			f.before = before.ref(idx-1, w.ex, out)
		}
		w.edge.first = f
	} else {
		delta := l.ms - w.prev.ms
		w.edge.intervals.add(delta)
		if delta >= w.spec.minGap && w.edge.kept.accepts(delta) {
			w.edge.kept.add(gapCandidate{delta: delta, from: before.ref(idx-1, w.ex, out), to: here})
		}
	}
	w.prev = stampRef{ms: l.ms, start: l.start, off: l.off, ok: true}
}

// gapMerge gathers a time-gap measure's intervals and candidates in the
// merge.
type gapMerge struct {
	spec      gapSpec
	intervals intervalCounts
	kept      gapKeeper
}

// add merges a chunk whose first line has index base. carry is the last
// stamped line before the chunk and lastLine the line just before the
// chunk.
//
// The chunk's keeper may have compacted and dropped every candidate at
// or below its cutoff; the merge takes that cutoff first, so a shorter
// candidate of another chunk (the edge pair included) is dropped here
// as it would be in one chunk. A chunk's cutoff is never above the
// cutoff of the whole file (the (limit+1)-th longest of a part is at
// most that of the whole), so what is kept is the same however the
// file is cut.
func (gm *gapMerge) add(edge *gapEdge, base int64, carry stampRef, lastLine lineRef) {
	gm.kept.raise(edge.kept.cutoff)
	if f := edge.first; f != nil && carry.ok {
		delta := f.line.ms - carry.ms
		gm.intervals.add(delta)
		if delta >= gm.spec.minGap {
			from := lastLine
			if f.before.ok {
				from = f.before
				from.idx += base
			}
			to := f.line
			to.idx += base
			gm.kept.add(gapCandidate{delta: delta, from: from, to: to})
		}
	}
	gm.intervals.merge(&edge.intervals)
	for _, c := range edge.kept.items {
		c.from.idx += base
		c.to.idx += base
		gm.kept.add(c)
	}
}

// finish adds a finding to out for every kept candidate at least
// max(min_gap, factor × the median interval) long, in line order.
func (gm *gapMerge) finish(cs *compiledSig, out sink) {
	median := float64(gm.intervals.lowerMedian())
	gm.kept.compact()
	for _, c := range gm.kept.items {
		if float64(c.delta) < gm.spec.factor*median {
			continue
		}
		out.add(cs, &rawFinding{startIdx: c.from.idx, endIdx: c.to.idx, startOff: c.from.start, endOff: c.to.next,
			firstLen: c.from.length, excerpt: c.from.excerpt, hasEx: c.from.hasEx, masked: c.from.masked, vs: -1, ve: -1, tm: c.from.tm})
	}
}

// gapKeeper keeps time-gap candidates in line order, at most about
// 2·limit of them at a time and limit after compact: the longest, by
// dropping every candidate at or below a cutoff that only rises.
type gapKeeper struct {
	limit int
	items []gapCandidate
	// cutoff is the longest interval dropped so far; -1 when none was.
	cutoff int64
}

// accepts reports whether a candidate of interval delta would be kept.
func (k *gapKeeper) accepts(delta int64) bool {
	return delta > k.cutoff
}

// raise lifts the cutoff to cutoff, when that is higher, and drops the
// candidates at or below it.
func (k *gapKeeper) raise(cutoff int64) {
	if cutoff <= k.cutoff {
		return
	}
	k.cutoff = cutoff
	k.dropAtOrBelowCutoff()
}

// dropAtOrBelowCutoff drops the candidates at or below the cutoff,
// keeping the others in line order.
func (k *gapKeeper) dropAtOrBelowCutoff() {
	kept := k.items[:0]
	for _, c := range k.items {
		if c.delta > k.cutoff {
			kept = append(kept, c)
		}
	}
	clear(k.items[len(kept):])
	k.items = kept
}

// add keeps c when it is longer than the cutoff.
func (k *gapKeeper) add(c gapCandidate) {
	if !k.accepts(c.delta) {
		return
	}
	k.items = append(k.items, c)
	if len(k.items) >= 2*k.limit {
		k.compact()
	}
}

// compact brings the candidates down to limit or fewer: the cutoff
// rises to the (limit+1)-th longest interval, and every candidate at or
// below it goes, ties included, so the candidates kept are the same
// whatever order they came in.
func (k *gapKeeper) compact() {
	if len(k.items) <= k.limit {
		return
	}
	deltas := make([]int64, len(k.items))
	for i, c := range k.items {
		deltas[i] = c.delta
	}
	slices.Sort(deltas)
	k.cutoff = max(k.cutoff, deltas[len(deltas)-k.limit-1])
	k.dropAtOrBelowCutoff()
}

// intervalCounts counts intervals between stamped lines by their value
// to three significant digits, for their median: exact below 1,000 ms,
// then 900 buckets per power of ten. A step back counts as 0.
type intervalCounts struct {
	counts map[int]int64
	total  int64
}

// pow10 holds the powers of ten an int64 reaches.
var pow10 = func() []int64 {
	out := []int64{1}
	for len(out) < 19 {
		out = append(out, out[len(out)-1]*10)
	}
	return out
}()

// intervalBucket returns the bucket of an interval of v ≥ 0 ms.
func intervalBucket(v int64) int {
	if v < 1000 {
		return int(v)
	}
	digits := 4
	for digits < len(pow10) && v >= pow10[digits] {
		digits++
	}
	return 1000 + (digits-4)*900 + int(v/pow10[digits-3]) - 100
}

// intervalValue returns the lower bound of a bucket.
func intervalValue(b int) int64 {
	if b < 1000 {
		return int64(b)
	}
	b -= 1000
	return int64(b%900+100) * pow10[b/900+1]
}

// add counts one interval.
func (c *intervalCounts) add(delta int64) {
	if c.counts == nil {
		c.counts = map[int]int64{}
	}
	c.counts[intervalBucket(max(delta, 0))]++
	c.total++
}

// merge adds the counts of o.
func (c *intervalCounts) merge(o *intervalCounts) {
	for b, n := range o.counts {
		if c.counts == nil {
			c.counts = map[int]int64{}
		}
		c.counts[b] += n
	}
	c.total += o.total
}

// lowerMedian returns the lower median of the counted intervals, as the
// lower bound of its bucket; 0 when none was counted.
func (c *intervalCounts) lowerMedian() int64 {
	if c.total == 0 {
		return 0
	}
	buckets := make([]int, 0, len(c.counts))
	for b := range c.counts {
		buckets = append(buckets, b)
	}
	slices.Sort(buckets)
	rank := (c.total - 1) / 2
	for _, b := range buckets {
		if rank < c.counts[b] {
			return intervalValue(b)
		}
		rank -= c.counts[b]
	}
	return intervalValue(buckets[len(buckets)-1])
}

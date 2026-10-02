package sigscan

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// The density measure: bursts of log lines over time.
//
// Every line from the file's first stamped line on is counted in the
// time bucket of its effective time: its own timestamp, or the last one
// before it (lines before the first stamped line are not counted). The
// counts of the buckets over the file's span are the series, stored
// with the findings. A bucket is a burst when its count is at least
// factor × the median count, its modified z-score
// 0.6745 × (count − median) / MAD is over z, and it holds at least
// min_count lines. The median and the MAD (the median of the absolute
// deviations from the median) are taken over the buckets that hold a
// line: in a log that sleeps at night, empty buckets would make the
// median 0 and every busy bucket a burst. Medians of an even count are
// the lower middle value. When the MAD is 0, a count above the median
// has an infinite z-score and the median itself has 0. Consecutive burst
// buckets are one finding, from the first line counted in any of them to
// the last one; its time is that first line's own timestamp.
//
// Bucket sizes. An explicit bucket is used as it is. `auto` picks the
// smallest size of the divisor chain 1s, 5s, 15s, 30s, 1m, 5m, 15m,
// 30m, 1h, 3h, 6h, 12h, 1d that cuts the span (latest minus earliest
// time) into at most 1,000 buckets. A series never has more than
// maxSeriesBuckets buckets: past that, the bucket doubles until it fits
// (an explicit 1s over years, or more than about 179 years at 1d).
//
// Across chunks. The span, and so the bucket of auto, is known only at
// the end, so each chunk counts in slots of the bucket its own span
// would pick, which the merge adds up. Each size of the chain divides
// the next (and 1d each doubling of it), and an explicit bucket only
// doubles, so the bucket of a chunk's span divides the bucket of any
// longer span: slots of a finer bucket are whole parts of a coarser
// one, and regrouping them gives exactly the counts a single walk would
// give. A slot also keeps its first and last line, so the finding of a
// burst needs no second read.

// densityTargetBuckets is how many buckets auto aims at over the span.
const densityTargetBuckets = 1000

// maxSeriesBuckets is the most buckets a density series holds.
const maxSeriesBuckets = 1 << 16

// densityLadder are the bucket sizes auto picks from, in milliseconds:
// a divisor chain, each size a whole multiple of the one before.
var densityLadder = millis(time.Second, 5*time.Second, 15*time.Second, 30*time.Second,
	time.Minute, 5*time.Minute, 15*time.Minute, 30*time.Minute,
	time.Hour, 3*time.Hour, 6*time.Hour, 12*time.Hour, 24*time.Hour)

// millis converts durations to milliseconds.
func millis(ds ...time.Duration) []int64 {
	out := make([]int64, len(ds))
	for i, d := range ds {
		out[i] = d.Milliseconds()
	}
	return out
}

// floorDiv divides rounding toward minus infinity, so that slot numbers
// of times before 1970 line up as those after it do.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// densityGrid gives the slot granularity and the final bucket of a
// density measure for a span of time.
type densityGrid struct {
	// explicit is the signature's bucket in milliseconds; 0 for auto.
	explicit int64
}

// slotsFor is how many buckets of b ms a span of span ms can touch.
func slotsFor(span, b int64) int64 {
	return span/b + 2
}

// ladderIndex returns the index of the smallest ladder size that cuts a
// span into at most densityTargetBuckets buckets, or the last index.
func ladderIndex(span int64) int {
	for i, b := range densityLadder {
		if b*densityTargetBuckets >= span {
			return i
		}
	}
	return len(densityLadder) - 1
}

// bucket returns the bucket of the series of a span.
func (g densityGrid) bucket(span int64) int64 {
	b := g.explicit
	if b == 0 {
		b = densityLadder[ladderIndex(span)]
	}
	for slotsFor(span, b) > maxSeriesBuckets {
		b *= 2
	}
	return b
}

// level returns the slot granularity of a span: the bucket that span
// would pick, which divides the bucket of every longer span.
func (g densityGrid) level(span int64) int64 {
	return g.bucket(span)
}

// densitySlot is the lines of one slot of time.
type densitySlot struct {
	count int64
	// first and last are the indexes of the first and the last line
	// counted here (chunk-relative in a walk, from the text's first line
	// in the merge); firstStart and lastNext their start and end offsets.
	first, last          int64
	firstStart, lastNext int64
	// firstLen is the first line's length without its ending.
	firstLen int64
	// firstMs is the first line's own timestamp.
	firstMs int64
	// The first line's excerpt, kept for the finding of a burst.
	excerpt       []byte
	hasEx, masked bool
}

// merge adds src, a part of the same slot of time, to s: the counts add
// up, the first line is the earlier one and the last the later one.
func (s *densitySlot) merge(src *densitySlot) {
	switch {
	case src.count == 0:
		return
	case s.count == 0:
		*s = *src
		return
	}
	s.count += src.count
	if src.first < s.first {
		s.first, s.firstStart, s.firstLen, s.firstMs = src.first, src.firstStart, src.firstLen, src.firstMs
		s.excerpt, s.hasEx, s.masked = src.excerpt, src.hasEx, src.masked
	}
	if src.last > s.last {
		s.last, s.lastNext = src.last, src.lastNext
	}
}

// densityHist counts lines in slots of time: slot k covers the times
// [k·level, (k+1)·level). The slots cover the span from the earliest
// to the latest time counted, and no more.
type densityHist struct {
	grid  densityGrid
	level int64 // 0 until the first time is counted
	lo    int64 // the slot number of slots[0]
	slots []densitySlot
	// minMs and maxMs are the earliest and latest times counted.
	minMs, maxMs int64
}

// slot returns the slot of time ms, widening the span and coarsening
// the slots when the span needs it. The pointer is valid until the next
// call.
func (h *densityHist) slot(ms int64) *densitySlot {
	h.widen(ms, ms)
	k := floorDiv(ms, h.level)
	h.cover(k)
	return &h.slots[k-h.lo]
}

// widen makes the span reach lo and hi, and coarsens the slots when the
// wider span needs a coarser level.
func (h *densityHist) widen(lo, hi int64) {
	if h.level == 0 {
		h.minMs, h.maxMs = lo, hi
		h.level = h.grid.level(hi - lo)
		h.lo = floorDiv(lo, h.level)
		return
	}
	if lo >= h.minMs && hi <= h.maxMs {
		return
	}
	h.minMs, h.maxMs = min(h.minMs, lo), max(h.maxMs, hi)
	if need := h.grid.level(h.maxMs - h.minMs); need > h.level {
		h.coarsen(need)
	}
}

// cover grows the slots to hold slot number k.
func (h *densityHist) cover(k int64) {
	if len(h.slots) == 0 {
		h.lo = k
	}
	if k < h.lo {
		grown := make([]densitySlot, h.lo-k, h.lo-k+int64(len(h.slots)))
		h.slots, h.lo = append(grown, h.slots...), k
	}
	if end := h.lo + int64(len(h.slots)); k >= end {
		h.slots = append(h.slots, make([]densitySlot, k-end+1)...)
	}
}

// coarsen regroups the slots into slots of level ms, a multiple of the
// current level.
func (h *densityHist) coarsen(level int64) {
	ratio := level / h.level
	if len(h.slots) == 0 {
		h.level, h.lo = level, floorDiv(h.lo, ratio)
		return
	}
	lo := floorDiv(h.lo, ratio)
	hi := floorDiv(h.lo+int64(len(h.slots))-1, ratio)
	out := make([]densitySlot, hi-lo+1)
	for i := range h.slots {
		out[floorDiv(h.lo+int64(i), ratio)-lo].merge(&h.slots[i])
	}
	h.slots, h.lo, h.level = out, lo, level
}

// absorb adds o, the slots of the next chunk, whose line indexes start
// at base.
func (h *densityHist) absorb(o *densityHist, base int64) {
	if o.level == 0 {
		return
	}
	h.widen(o.minMs, o.maxMs)
	if o.level > h.level {
		h.coarsen(o.level)
	}
	ratio := h.level / o.level
	for i := range o.slots {
		src := o.slots[i]
		if src.count == 0 {
			continue
		}
		src.first += base
		src.last += base
		k := floorDiv(o.lo+int64(i), ratio)
		h.cover(k)
		h.slots[k-h.lo].merge(&src)
	}
}

// densitySpec is a density measure's parameters, durations in
// milliseconds.
type densitySpec struct {
	grid      densityGrid
	factor, z float64
	minCount  int64
}

// densityLeading are the lines of a chunk before its first stamped
// line: they belong to the bucket of the last stamped line before the
// chunk.
type densityLeading struct {
	count, last, lastNext int64
}

// densityEdge is what a chunk hands the merge for a density measure.
type densityEdge struct {
	hist    densityHist
	leading densityLeading
}

// densityWalker counts one chunk's lines.
type densityWalker struct {
	ex   *excerpter
	edge densityEdge
}

func newDensityWalker(cs *compiledSig, ex *excerpter) *densityWalker {
	return &densityWalker{ex: ex, edge: densityEdge{hist: densityHist{grid: cs.density.grid}}}
}

// line counts line l, at index idx of the chunk, in the slot of the last
// stamped line at or before it (times).
//
// A slot's first line is always a stamped line: a line without a
// timestamp has the time of the line before it, so it falls in the slot
// that line already opened. Its excerpt is built then, once per slot.
func (w *densityWalker) line(l *line, idx int64, times *timeTracker, out sink) {
	if !times.last.ok {
		lead := &w.edge.leading
		lead.count++
		lead.last, lead.lastNext = idx, l.next
		return
	}
	s := w.edge.hist.slot(times.last.ms)
	if s.count == 0 {
		s.first, s.firstStart, s.firstLen, s.firstMs = idx, l.start, l.length, times.last.ms
		if out.mayStore() {
			s.excerpt, s.masked = w.ex.build(nil, l.view)
			s.hasEx = true
		}
	}
	s.count++
	s.last, s.lastNext = idx, l.next
}

// densityMerge adds up a density measure's slots in the merge.
type densityMerge struct {
	spec densitySpec
	hist densityHist
}

// add merges a chunk whose first line has index base; carry is the last
// stamped line before the chunk.
func (dm *densityMerge) add(edge *densityEdge, base int64, carry stampRef) {
	if lead := edge.leading; lead.count > 0 && carry.ok {
		s := dm.hist.slot(carry.ms)
		s.count += lead.count
		if g := base + lead.last; g > s.last {
			s.last, s.lastNext = g, lead.lastNext
		}
	}
	dm.hist.absorb(&edge.hist, base)
}

// finish regroups the slots into the series' buckets, adds a finding
// for each burst to out, in line order, and returns the series (nil when
// no line has a timestamp).
func (dm *densityMerge) finish(cs *compiledSig, out sink) *Series {
	h := &dm.hist
	if h.level == 0 {
		return nil
	}
	b := h.grid.bucket(h.maxMs - h.minMs)
	if b > h.level {
		h.coarsen(b)
	}
	counts := make([]int64, len(h.slots))
	for i := range h.slots {
		counts[i] = h.slots[i].count
	}
	var bursts []rawFinding
	isBurst := dm.spec.burstTest(counts)
	for i := 0; i < len(h.slots); {
		if !isBurst(counts[i]) {
			i++
			continue
		}
		f := burstFinding(&h.slots[i])
		for i++; i < len(h.slots) && isBurst(counts[i]); i++ {
			widenBurst(&f, &h.slots[i])
		}
		bursts = append(bursts, f)
	}
	slices.SortFunc(bursts, func(a, b rawFinding) int {
		return cmp.Or(cmp.Compare(a.startIdx, b.startIdx), cmp.Compare(a.endIdx, b.endIdx))
	})
	for i := range bursts {
		out.add(cs, &bursts[i])
	}
	return &Series{BucketMS: b, StartMS: h.lo * b, Counts: counts}
}

// burstTest returns the test of a bucket's count: at least factor × the
// median, a modified z-score over z, and at least min_count lines, the
// median and the MAD taken over the buckets that hold a line.
func (s densitySpec) burstTest(counts []int64) func(int64) bool {
	var held []int64
	for _, c := range counts {
		if c > 0 {
			held = append(held, c)
		}
	}
	if len(held) == 0 {
		return func(int64) bool { return false }
	}
	med := lowerMedian(held)
	deviations := make([]int64, len(held))
	for i, c := range held {
		deviations[i] = max(c-med, med-c)
	}
	mad := lowerMedian(deviations)
	return func(c int64) bool {
		return c >= s.minCount && float64(c) >= s.factor*float64(med) && modifiedZ(c, med, mad) > s.z
	}
}

// lowerMedian returns the lower median of values (it sorts them).
func lowerMedian(values []int64) int64 {
	slices.Sort(values)
	return values[(len(values)-1)/2]
}

// modifiedZ is the modified z-score of x: 0.6745 × (x − med) / mad, and
// for a MAD of 0, +Inf above the median and 0 at or below it.
func modifiedZ(x, med, mad int64) float64 {
	if mad == 0 {
		if x > med {
			return math.Inf(1)
		}
		return 0
	}
	return 0.6745 * float64(x-med) / float64(mad)
}

// burstFinding is the finding of a burst that starts with slot s.
func burstFinding(s *densitySlot) rawFinding {
	return rawFinding{startIdx: s.first, endIdx: s.last, startOff: s.firstStart, endOff: s.lastNext, firstLen: s.firstLen,
		excerpt: s.excerpt, hasEx: s.hasEx, masked: s.masked, vs: -1, ve: -1, tm: timeRef{ms: s.firstMs, state: timeKnown}}
}

// widenBurst extends a burst's finding over slot s: from the earlier
// first line to the later last line.
func widenBurst(f *rawFinding, s *densitySlot) {
	if s.first < f.startIdx {
		f.startIdx, f.startOff, f.firstLen = s.first, s.firstStart, s.firstLen
		f.excerpt, f.hasEx, f.masked = s.excerpt, s.hasEx, s.masked
		f.tm = timeRef{ms: s.firstMs, state: timeKnown}
	}
	if s.last > f.endIdx {
		f.endIdx, f.endOff = s.last, s.lastNext
	}
}

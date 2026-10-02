package sigscan

import (
	"bytes"
	"sync/atomic"
)

// line is one line of the text as the matchers see it.
type line struct {
	// view is the line without its ending ("\n" or "\r\n"), without a
	// byte-order mark at the start of the text, cut at the line-view
	// size. It is borrowed: valid only while the line is handled.
	view []byte
	// length is the length of that line before the cut.
	length int64
	// start is the offset of the line's first byte in the text; next
	// the offset just past its ending (the end of the text for a last
	// line without one).
	start, next int64
	// shift is how many bytes of the line come before the view: 3 for
	// a first line that starts with a byte-order mark, else 0. A value
	// span in the view plus shift is a position in the line.
	shift int
	// stamped and ms are the line's own timestamp, in the file's frame,
	// and off the zone offset the line writes (timeTracker.observe fills
	// them before the line is matched; stamped is false in a file
	// without timestamps).
	stamped bool
	ms      int64
	off     int
}

// rawFinding is a finding as a walk reports it: line indexes relative
// to the walk, the excerpt in a buffer the walk reuses.
type rawFinding struct {
	startIdx, endIdx int64
	startOff, endOff int64
	// firstLen is the length of the first line without its ending.
	firstLen int64
	excerpt  []byte
	hasEx    bool
	// vs and ve are the value span in the first line (-1: none).
	vs, ve int
	// masked says the finding's text is masked: its signature redacts,
	// or a redactor masked something in the excerpt.
	masked bool
	// tm is the time of the finding's first line.
	tm timeRef
}

// sink receives the findings of one signature from a walk.
type sink interface {
	// storing reports whether a finding added now is stored, so that
	// the walk builds its excerpt; when false it is only counted.
	storing() bool
	// mayStore reports whether a finding of a block or run that begins
	// now may be stored when it ends, possibly in a later chunk.
	mayStore() bool
	add(cs *compiledSig, f *rawFinding)
}

// toFinding turns a raw finding into a Finding, numbering its lines
// from lineBase.
func toFinding(cs *compiledSig, f *rawFinding, lineBase int64) Finding {
	out := Finding{
		StartLine: lineBase + f.startIdx, EndLine: lineBase + f.endIdx,
		StartOffset: f.startOff, EndOffset: f.endOff,
		TimeMS: f.tm.pointer(), TimeOffsetMinutes: f.tm.offsetMinutes(),
		Masked: f.masked, firstLen: f.firstLen,
	}
	if f.hasEx {
		out.Excerpt = string(f.excerpt)
	}
	if f.vs >= 0 {
		vs, ve := f.vs, f.ve
		out.ValueStart, out.ValueEnd = &vs, &ve
	}
	return out
}

// chunkSink keeps one signature's findings in one chunk: the first cap
// of them, and the count of all. Once the merge has stored cap findings
// of the signature (full), the chunk only counts.
type chunkSink struct {
	findings []Finding
	// pending are the indexes in findings of those whose time the merge
	// decides (timePending), in order.
	pending []int32
	count   int64
	cap     int
	full    *atomic.Bool
}

func (s *chunkSink) storing() bool  { return len(s.findings) < s.cap && !s.full.Load() }
func (s *chunkSink) mayStore() bool { return !s.full.Load() }

func (s *chunkSink) add(cs *compiledSig, f *rawFinding) {
	s.count++
	if s.storing() {
		// Line numbers stay chunk-relative (0-based) until the merge
		// knows how many lines come before the chunk.
		s.findings = append(s.findings, toFinding(cs, f, 0))
		if f.tm.state == timePending {
			s.pending = append(s.pending, int32(len(s.findings)-1)) // #nosec G115 -- at most Cap findings
		}
	}
}

// chunkResult is what a walk of one chunk (or batch) found, for the
// merge.
type chunkResult struct {
	seq int
	// lines is the number of lines the chunk owns; end is the offset
	// just past its last line (the end of the text for the last chunk).
	lines int64
	end   int64
	// final says the walk reached the end of the text.
	final bool
	sigs  []sigChunk
	// The first and last lines' views and lengths, kept when a `same`
	// run is selected, to compare lines across the chunk's edges.
	firstView, lastView []byte
	firstLen, lastLen   int64
	// span says where the chunk's lines lie, so the merge can walk them
	// again.
	span chunkSpan
	// lastStamp is the chunk's last stamped line (ok false when it has
	// none): the merge carries it to settle the pending times of the
	// chunks after it.
	lastStamp stampRef
	// stamped is how many of the chunk's lines have a timestamp of
	// their own (the unprefixed measure's share).
	stamped int64
	// lastLine is the chunk's last line, as the start of a time-gap
	// finding (ok false without a time-gap measure).
	lastLine lineRef
}

// chunkSpan places a chunk's lines in the text: they start at start
// and are the lines that start at or before limit.
type chunkSpan struct {
	start, limit int64
	// batch is the stream batch that held the lines (stream path only).
	batch *batch
}

// sigChunk is what one chunk found for one signature.
type sigChunk struct {
	// findings are the chunk's first findings (chunk-relative lines),
	// count all of them. For a run, only those that touch neither edge.
	findings []Finding
	// pending are the indexes in findings whose time the merge decides.
	pending []int32
	count   int64
	// open is the block still open after the chunk's last line, with
	// chunk-relative line indexes (nil when none, and always nil for a
	// final chunk, which closed it).
	open *blockState
	runs runEdges
	// long is a long-line measure's lengths and candidates at the
	// chunk's edges.
	long *longLineEdge
	// back, gap and density are the time measures' edges.
	back    *backFirst
	gap     *gapEdge
	density *densityEdge
}

// walk runs every selected signature over the lines of one chunk, in
// order. One goroutine owns a walk.
type walk struct {
	e          *engine
	gs         *gateScratch
	blockOf    []*blockWalker // by signature index; nil for other kinds
	runOf      []*runWalker
	sinks      []chunkSink
	openBlocks []int32
	idx        int64
	exBuf      []byte
	ex         *excerpter
	// The previous line's view and length, for `same` runs.
	prevView []byte
	prevLen  int64
	res      *chunkResult
	// times follows the chunk's stamped lines and gives each finding
	// the time of its first line.
	times timeTracker
	// longOf are the long-line measures' walkers, by signature index.
	longOf []*longLineWalker
	// The time measures' walkers, by signature index; nil in a file
	// without a timestamp format, where they find nothing.
	backOf    []*backWalker
	gapOf     []*gapWalker
	densityOf []*densityWalker
	// prevStart and prevTm are the previous line's start and the time
	// a finding starting there would have, for time-gap.
	prevStart int64
	prevTm    timeRef
	// end is the offset just past the last line walked.
	end int64
}

// newWalk returns a walk for one chunk. gs is the goroutine's gate
// scratch, reused from chunk to chunk.
func newWalk(e *engine, gs *gateScratch) *walk {
	n := len(e.sigs)
	w := &walk{e: e, gs: gs, blockOf: make([]*blockWalker, n), runOf: make([]*runWalker, n), sinks: make([]chunkSink, n),
		res: &chunkResult{sigs: make([]sigChunk, n)}, ex: newExcerpter(e), times: timeTracker{src: e.times}}
	for i := range w.sinks {
		w.sinks[i] = chunkSink{cap: e.capOf(int32(i)), full: &e.full[i]} // #nosec G115 -- at most the loaded signatures, twice
	}
	for _, si := range e.blocks {
		w.blockOf[si] = newBlockWalker(e.sigs[si], w.ex, &w.times)
	}
	for _, si := range e.runs {
		w.runOf[si] = newRunWalker(e.sigs[si], w.ex, &w.times)
	}
	if len(e.longLines) > 0 {
		w.longOf = make([]*longLineWalker, n)
		for _, si := range e.longLines {
			w.longOf[si] = newLongLineWalker(e.sigs[si], w.ex)
		}
	}
	if e.times != nil {
		w.newTimeWalkers(n)
	}
	return w
}

// newTimeWalkers makes the walkers of the time measures, for a file
// with a timestamp format.
func (w *walk) newTimeWalkers(n int) {
	e := w.e
	w.backOf = make([]*backWalker, n)
	for _, si := range e.backs {
		w.backOf[si] = &backWalker{cs: e.sigs[si], spec: *e.sigs[si].back, ex: w.ex}
	}
	w.gapOf = make([]*gapWalker, n)
	for _, si := range e.gaps {
		w.gapOf[si] = newGapWalker(e.sigs[si], w.ex, e.gapLimit)
	}
	w.densityOf = make([]*densityWalker, n)
	for _, si := range e.densities {
		w.densityOf[si] = newDensityWalker(e.sigs[si], w.ex)
	}
}

// line handles one line. It is the per-line path of a scan: the line's
// timestamp, the gate, then only the signatures the line can concern.
func (w *walk) line(l *line) bool {
	e := w.e
	if e.times != nil {
		w.times.observe(l)
	}
	e.gate.evaluate(l.view, w.gs)
	idx := w.idx
	// An open block takes the line first; only a line it refused may
	// start another block of its signature.
	if len(w.openBlocks) > 0 {
		kept := w.openBlocks[:0]
		for _, si := range w.openBlocks {
			b := w.blockOf[si]
			b.feed(l, idx, &w.sinks[si])
			if b.open {
				kept = append(kept, si)
			}
		}
		w.openBlocks = kept
	}
	for _, si := range w.gs.passed {
		w.anchorLine(si, l, idx)
	}
	for _, si := range e.lineAnchored {
		w.anchorLine(si, l, idx)
	}
	for _, si := range e.valueSigs {
		addValueFinding(e.sigs[si], l, idx, &w.sinks[si], &w.exBuf, w.ex, w.times.at(l.start))
	}
	for _, si := range e.longLines {
		w.longOf[si].line(l, idx, &w.times, &w.sinks[si])
	}
	if e.times != nil {
		w.timeMeasures(l, idx)
	}
	if l.stamped {
		w.res.stamped++
	}
	if len(e.runs) > 0 {
		same := e.hasSame && idx > 0 && keysEqual(w.prevView, w.prevLen, l.view, l.length)
		for _, si := range e.runs {
			w.runOf[si].step(l, idx, same, &w.sinks[si])
		}
	}
	if e.hasSame && idx == 0 {
		w.res.firstView, w.res.firstLen = bytes.Clone(l.view), l.length
	}
	if e.hasSame || len(e.gaps) > 0 {
		w.prevView, w.prevLen = append(w.prevView[:0], l.view...), l.length
	}
	if len(e.gaps) > 0 {
		w.prevStart, w.prevTm = l.start, w.times.at(l.start)
	}
	w.end = l.next
	w.idx++
	return true
}

// timeMeasures runs the time measures on line l: they look at stamped
// lines (density counts every line from the first stamped one on).
func (w *walk) timeMeasures(l *line, idx int64) {
	e := w.e
	for _, si := range e.backs {
		w.backOf[si].line(l, idx, &w.sinks[si])
	}
	if len(e.gaps) > 0 {
		before := prevLine{view: w.prevView, length: w.prevLen, start: w.prevStart, tm: w.prevTm}
		for _, si := range e.gaps {
			w.gapOf[si].line(l, idx, &before, &w.sinks[si])
		}
	}
	for _, si := range e.densities {
		w.densityOf[si].line(l, idx, &w.times, &w.sinks[si])
	}
}

// anchorLine tests signature si's anchor on a line that passed its
// gate (or on every line, for a signature without one).
func (w *walk) anchorLine(si int32, l *line, idx int64) {
	cs := w.e.sigs[si]
	switch {
	case cs.regex != nil:
		w.confirmRegex(cs, l, idx)
	case cs.block != nil:
		b := w.blockOf[si]
		if b.open || b.joinedAt == idx {
			return
		}
		b.tryOpen(l, idx, &w.sinks[si])
		if b.open {
			w.openBlocks = append(w.openBlocks, si)
		}
	case cs.run != nil && cs.run.mode == runMatch:
		if matchesRun(cs.run, l.view) {
			w.runOf[si].qualifiedAt = idx
		}
	}
}

// matchesRun reports whether a line qualifies for a `match` run (the
// gate already passed).
func matchesRun(p *runProgram, view []byte) bool {
	return p.match.Match(view) && (p.not == nil || !p.not.Match(view))
}

// confirmRegex runs a regex signature on a line.
func (w *walk) confirmRegex(cs *compiledSig, l *line, idx int64) {
	addRegexFinding(cs, l, idx, &w.sinks[cs.idx], &w.exBuf, w.ex, w.times.at(l.start))
}

// addRegexFinding confirms a regex signature on line l and adds the
// finding to out, with the time tm. The excerpt and value span are
// built only when out stores the finding.
func addRegexFinding(cs *compiledSig, l *line, idx int64, out sink, exBuf *[]byte, ex *excerpter, tm timeRef) {
	store := out.storing()
	ok, vs, ve := cs.regex.confirm(l.view, store)
	if !ok {
		return
	}
	f := rawFinding{startIdx: idx, endIdx: idx, startOff: l.start, endOff: l.next, firstLen: l.length, vs: -1, ve: -1, tm: tm}
	if store {
		if vs >= 0 {
			f.vs, f.ve = vs+l.shift, ve+l.shift
		}
		// Every secret on the line is masked, by every redacting
		// signature, whatever signature made the finding.
		var masked bool
		*exBuf, masked = ex.build((*exBuf)[:0], l.view)
		f.excerpt, f.hasEx, f.masked = *exBuf, true, masked || cs.redact
	}
	out.add(cs, &f)
}

// finishTimeMeasures hands the time measures' edges to the result,
// and, for time-gap, the chunk's last line: it may be the line just
// before the next chunk's first stamped line.
func (w *walk) finishTimeMeasures(res *chunkResult) {
	e := w.e
	for _, si := range e.backs {
		res.sigs[si].back = w.backOf[si].first
	}
	for _, si := range e.gaps {
		res.sigs[si].gap = &w.gapOf[si].edge
	}
	for _, si := range e.densities {
		res.sigs[si].density = &w.densityOf[si].edge
	}
	if len(e.gaps) > 0 && w.idx > 0 {
		last := prevLine{view: w.prevView, length: w.prevLen, start: w.prevStart, tm: w.prevTm}
		res.lastLine = last.ref(w.idx-1, w.ex, &w.sinks[e.gaps[0]])
	}
}

// addValueFinding runs a value measure on line l and adds a finding of
// its first value to out, with the time tm; the value span is the
// value, and the excerpt is built only when out stores the finding.
func addValueFinding(cs *compiledSig, l *line, idx int64, out sink, exBuf *[]byte, ex *excerpter, tm timeRef) {
	v, ok := cs.value.next(l.view, 0)
	if !ok {
		return
	}
	f := rawFinding{startIdx: idx, endIdx: idx, startOff: l.start, endOff: l.next, firstLen: l.length,
		vs: v[0] + l.shift, ve: v[1] + l.shift, tm: tm}
	if out.storing() {
		var masked bool
		*exBuf, masked = ex.build((*exBuf)[:0], l.view)
		f.excerpt, f.hasEx, f.masked = *exBuf, true, masked || cs.redact
	}
	out.add(cs, &f)
}

// finish ends the walk after the chunk's last line and returns what it
// found. final says the chunk ends the text: open blocks close there.
// Runs are always reported, never closed: the merge closes them.
func (w *walk) finish(final bool) *chunkResult {
	res := w.res
	res.lines, res.end, res.final = w.idx, w.end, final
	res.lastStamp = w.times.last
	if w.e.hasSame && w.idx > 0 {
		res.lastView, res.lastLen = bytes.Clone(w.prevView), w.prevLen
	}
	for _, si := range w.e.blocks {
		b := w.blockOf[si]
		if !b.open {
			continue
		}
		if final {
			b.close(&w.sinks[si])
			continue
		}
		st := b.st
		st.excerpt = bytes.Clone(st.excerpt)
		res.sigs[si].open = &st
	}
	for _, si := range w.e.runs {
		res.sigs[si].runs = w.runOf[si].edges(w.prevView, &w.sinks[si])
	}
	for _, si := range w.e.longLines {
		res.sigs[si].long = w.longOf[si].finish()
	}
	if w.e.times != nil {
		w.finishTimeMeasures(res)
	}
	for si := range w.sinks {
		sc := &res.sigs[si]
		sc.findings, sc.pending, sc.count = w.sinks[si].findings, w.sinks[si].pending, w.sinks[si].count
	}
	return res
}

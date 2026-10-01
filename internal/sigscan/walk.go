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
}

// rawFinding is a finding as a walk reports it: line indexes relative
// to the walk, the excerpt in a buffer the walk reuses.
type rawFinding struct {
	startIdx, endIdx int64
	startOff, endOff int64
	excerpt          []byte
	hasEx            bool
	// vs and ve are the value span in the first line (-1: none).
	vs, ve int
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
		Masked: cs.redact,
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
	count    int64
	cap      int
	full     *atomic.Bool
}

func (s *chunkSink) storing() bool  { return len(s.findings) < s.cap && !s.full.Load() }
func (s *chunkSink) mayStore() bool { return !s.full.Load() }

func (s *chunkSink) add(cs *compiledSig, f *rawFinding) {
	s.count++
	if s.storing() {
		// Line numbers stay chunk-relative (0-based) until the merge
		// knows how many lines come before the chunk.
		s.findings = append(s.findings, toFinding(cs, f, 0))
	}
}

// chunkResult is what a walk of one chunk (or batch) found, for the
// merge.
type chunkResult struct {
	seq int
	// lines is the number of lines the chunk owns.
	lines int64
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
	count    int64
	// open is the block still open after the chunk's last line, with
	// chunk-relative line indexes (nil when none, and always nil for a
	// final chunk, which closed it).
	open *blockState
	runs runEdges
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
	// The previous line's view and length, for `same` runs.
	prevView []byte
	prevLen  int64
	res      *chunkResult
}

// newWalk returns a walk for one chunk. gs is the goroutine's gate
// scratch, reused from chunk to chunk.
func newWalk(e *engine, gs *gateScratch) *walk {
	n := len(e.sigs)
	w := &walk{e: e, gs: gs, blockOf: make([]*blockWalker, n), runOf: make([]*runWalker, n), sinks: make([]chunkSink, n),
		res: &chunkResult{sigs: make([]sigChunk, n)}}
	for i := range w.sinks {
		w.sinks[i] = chunkSink{cap: e.cap, full: &e.full[i]}
	}
	for _, si := range e.blocks {
		w.blockOf[si] = newBlockWalker(e.sigs[si])
	}
	for _, si := range e.runs {
		w.runOf[si] = newRunWalker(e.sigs[si])
	}
	return w
}

// line handles one line. It is the per-line path of a scan: the gate,
// then only the signatures the line can concern.
func (w *walk) line(l *line) bool {
	e := w.e
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
	if len(e.runs) > 0 {
		same := e.hasSame && idx > 0 && keysEqual(w.prevView, w.prevLen, l.view, l.length)
		for _, si := range e.runs {
			w.runOf[si].step(l, idx, same, &w.sinks[si])
		}
	}
	if e.hasSame {
		if idx == 0 {
			w.res.firstView, w.res.firstLen = bytes.Clone(l.view), l.length
		}
		w.prevView, w.prevLen = append(w.prevView[:0], l.view...), l.length
	}
	w.idx++
	return true
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
	addRegexFinding(cs, l, idx, &w.sinks[cs.idx], &w.exBuf)
}

// addRegexFinding confirms a regex signature on line l and adds the
// finding to out. The excerpt and value span are built only when out
// stores the finding.
func addRegexFinding(cs *compiledSig, l *line, idx int64, out sink, exBuf *[]byte) {
	store := out.storing()
	ok, vs, ve := cs.regex.confirm(l.view, store)
	if !ok {
		return
	}
	f := rawFinding{startIdx: idx, endIdx: idx, startOff: l.start, endOff: l.next, vs: -1, ve: -1}
	if store {
		if vs >= 0 {
			f.vs, f.ve = vs+l.shift, ve+l.shift
		}
		var masks []span
		if cs.redact {
			// Every match of the anchor is masked, not only the one that
			// made the finding: a second secret on the line is a secret too.
			masks = redactSpans(cs.regex.re, cs.regex.capture, l.view)
		}
		*exBuf = appendExcerpt((*exBuf)[:0], l.view, masks)
		f.excerpt, f.hasEx = *exBuf, true
	}
	out.add(cs, &f)
}

// finish ends the walk after the chunk's last line and returns what it
// found. final says the chunk ends the text: open blocks close there.
// Runs are always reported, never closed: the merge closes them.
func (w *walk) finish(final bool) *chunkResult {
	res := w.res
	res.lines, res.final = w.idx, final
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
		res.sigs[si].runs = w.runOf[si].edges()
	}
	for si := range w.sinks {
		res.sigs[si].findings, res.sigs[si].count = w.sinks[si].findings, w.sinks[si].count
	}
	return res
}

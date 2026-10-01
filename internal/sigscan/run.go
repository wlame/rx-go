package sigscan

import (
	"bytes"
	"regexp"

	"github.com/wlame/rx-go/internal/signatures"
)

// runMode is what makes a line qualify for a run.
type runMode int

const (
	// runMatch: the line passes the gate and matches the regex (and not
	// `not`).
	runMatch runMode = iota
	// runBlank: the line view is empty or only spaces and tabs.
	runBlank
	// runSame: the line equals the line before it (view and length).
	runSame
	// runBinary: the line view holds a NUL byte or bytes that are not
	// UTF-8 (the binary measure).
	runBinary
	// runUnprefixed: the line has no timestamp of its own, in a file
	// with a timestamp format (the unprefixed measure).
	runUnprefixed
)

// runProgram is a run signature as the walk runs it.
type runProgram struct {
	mode       runMode
	match, not *regexp.Regexp
	min        int64
}

func compileRun(spec *signatures.RunSpec) *runProgram {
	p := &runProgram{match: spec.Match, not: spec.Not, min: int64(spec.Min)}
	switch {
	case spec.Blank:
		p.mode = runBlank
	case spec.Same:
		p.mode = runSame
	}
	return p
}

// runPiece is a run of consecutive lines as a walk saw it: where it
// starts and ends, and the excerpt of its first line. A piece that
// touches a chunk edge may continue in the next or previous chunk.
type runPiece struct {
	startIdx, endIdx int64
	startOff, endOff int64
	excerpt          []byte
	hasEx            bool
	vs, ve           int
	masked           bool
	// tm is the first line's time.
	tm timeRef
}

// lines is the number of lines in the piece.
func (p *runPiece) lines() int64 { return p.endIdx - p.startIdx + 1 }

// runWalker follows one run signature over the lines of one chunk.
//
// For `match` and `blank` a run is a maximal stretch of qualifying
// lines. For `same` every line is in a group of equal consecutive
// lines; a group of min lines or more is a finding, the first line of
// the group included.
//
// A run that touches the chunk's first line (leading) or is still open
// at its last line (trailing) may continue across the edge, so the
// walker does not judge it: it reports it, and the merge joins it with
// its neighbor and applies min. Every other run is judged here.
type runWalker struct {
	cs   *compiledSig
	prog *runProgram
	ex   *excerpter
	// times gives a run the time of its first line (the walk's tracker).
	times *timeTracker
	open  bool
	cur   runPiece
	// qualifiedAt is the index of the last line a `match` run's regex
	// confirmed; the walk sets it before step.
	qualifiedAt int64
	// leading is the run that began on the chunk's first line, once it
	// ended inside the chunk; hasLeading says there is one.
	leading    runPiece
	hasLeading bool
}

func newRunWalker(cs *compiledSig, ex *excerpter, times *timeTracker) *runWalker {
	return &runWalker{cs: cs, prog: cs.run, ex: ex, times: times, qualifiedAt: -1}
}

// step takes line l (index idx). sameAsPrevious says the line equals
// the line before it in the chunk (always false on the chunk's first
// line).
func (r *runWalker) step(l *line, idx int64, sameAsPrevious bool, out sink) {
	var qualifies bool
	switch r.prog.mode {
	case runMatch:
		qualifies = r.qualifiedAt == idx
	case runBlank:
		qualifies = isBlank(l.view)
	case runBinary:
		qualifies = holdsBinary(l.view, int64(len(l.view)) < l.length)
	case runUnprefixed:
		qualifies = r.times.src != nil && !l.stamped
	case runSame:
		if !sameAsPrevious {
			r.end(out)
			r.begin(l, idx, out)
			return
		}
		r.extend(l, idx)
		// Every line of a group equals its first, so the excerpt is built
		// from the group's second line: a group of one line is never a
		// finding, and a file of distinct lines builds no excerpt.
		r.buildExcerpt(l.view, l, out)
		return
	}
	if !qualifies {
		r.end(out)
		return
	}
	if !r.open {
		r.begin(l, idx, out)
		return
	}
	r.extend(l, idx)
}

// begin opens a run on line l. A `match` or `blank` run builds its
// excerpt now, from its first line; a `same` run when it has a second.
func (r *runWalker) begin(l *line, idx int64, out sink) {
	r.open = true
	r.cur = runPiece{startIdx: idx, endIdx: idx, startOff: l.start, endOff: l.next, excerpt: r.cur.excerpt[:0], vs: -1, ve: -1,
		tm: r.times.at(l.start)}
	if r.prog.mode != runSame {
		r.buildExcerpt(l.view, l, out)
	}
}

// buildExcerpt builds the open run's excerpt from view, a line equal to
// its first (l, when given, is that line), once, when the run may be
// kept: a run's fate is known only when it ends, possibly in a later
// chunk. Every redactor masks it (excerpter.build).
func (r *runWalker) buildExcerpt(view []byte, l *line, out sink) {
	if r.cur.hasEx || !out.mayStore() {
		return
	}
	r.cur.hasEx = true
	if r.prog.mode == runMatch {
		r.cur.excerpt, r.cur.masked, r.cur.vs, r.cur.ve = anchorExcerpt(r.cur.excerpt, r.ex, r.cs, r.prog.match, l)
		return
	}
	r.cur.excerpt, r.cur.masked = r.ex.build(r.cur.excerpt, view)
}

// extend adds line l to the open run.
func (r *runWalker) extend(l *line, idx int64) {
	r.cur.endIdx = idx
	r.cur.endOff = l.next
}

// end closes the open run, if any: a leading run is kept for the merge,
// any other is a finding when it is at least min lines long.
func (r *runWalker) end(out sink) {
	if !r.open {
		return
	}
	r.open = false
	if r.cur.startIdx == 0 {
		r.leading = clonePiece(r.cur)
		r.hasLeading = true
		return
	}
	if r.cur.lines() >= r.prog.min {
		emitPiece(r.cs, &r.cur, out)
	}
}

// emitPiece hands a run to a sink as a finding.
func emitPiece(cs *compiledSig, p *runPiece, out sink) {
	out.add(cs, &rawFinding{startIdx: p.startIdx, endIdx: p.endIdx, startOff: p.startOff, endOff: p.endOff,
		excerpt: p.excerpt, hasEx: p.hasEx, vs: p.vs, ve: p.ve, masked: p.masked, tm: p.tm})
}

// clonePiece copies a piece so it outlives the walker's buffers.
func clonePiece(p runPiece) runPiece {
	p.excerpt = bytes.Clone(p.excerpt)
	return p
}

// runEdges is what a chunk reports about one run signature at its
// edges.
type runEdges struct {
	// leading is the run that began on the chunk's first line; none
	// when that line did not qualify (match and blank).
	leading    runPiece
	hasLeading bool
	// whole says the leading run reaches the chunk's last line: it
	// covers the chunk, and trailing is the same run.
	whole bool
	// trailing is the run still open at the chunk's last line that did
	// not begin on its first line.
	trailing    runPiece
	hasTrailing bool
}

// edges reports the walker's edge runs after the chunk's last line.
// lastView is that line's view: a `same` run still open has not built
// its excerpt when it has one line, and its lines equal the last.
func (r *runWalker) edges(lastView []byte, out sink) runEdges {
	e := runEdges{leading: r.leading, hasLeading: r.hasLeading}
	if !r.open {
		return e
	}
	if r.prog.mode == runSame {
		r.buildExcerpt(lastView, nil, out)
	}
	if r.cur.startIdx == 0 {
		e.leading, e.hasLeading, e.whole = clonePiece(r.cur), true, true
		return e
	}
	e.trailing, e.hasTrailing = clonePiece(r.cur), true
	return e
}

// keysEqual reports whether two lines are equal for a `same` run: the
// same view and the same length (a line longer than the view compares
// its view and its full length).
func keysEqual(aView []byte, aLen int64, bView []byte, bLen int64) bool {
	return aLen == bLen && bytes.Equal(aView, bView)
}

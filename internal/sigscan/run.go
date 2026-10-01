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
	open bool
	cur  runPiece
	// qualifiedAt is the index of the last line a `match` run's regex
	// confirmed; the walk sets it before step.
	qualifiedAt int64
	// leading is the run that began on the chunk's first line, once it
	// ended inside the chunk; hasLeading says there is one.
	leading    runPiece
	hasLeading bool
}

func newRunWalker(cs *compiledSig) *runWalker {
	return &runWalker{cs: cs, prog: cs.run, qualifiedAt: -1}
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
	case runSame:
		if !sameAsPrevious {
			r.end(out)
			r.begin(l, idx, out)
			return
		}
		r.extend(l, idx)
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

// begin opens a run on line l.
func (r *runWalker) begin(l *line, idx int64, out sink) {
	r.open = true
	r.cur = runPiece{startIdx: idx, endIdx: idx, startOff: l.start, endOff: l.next, excerpt: r.cur.excerpt[:0], vs: -1, ve: -1}
	// The excerpt is built for every run that may be kept: a run's fate
	// is known only when it ends, possibly in a later chunk.
	if out.mayStore() {
		r.cur.hasEx = true
		if r.prog.mode == runMatch {
			r.cur.excerpt, r.cur.vs, r.cur.ve = anchorExcerpt(r.cur.excerpt, r.cs, r.prog.match, l.view)
			if r.cur.vs >= 0 {
				r.cur.vs += l.shift
				r.cur.ve += l.shift
			}
		} else {
			r.cur.excerpt = appendExcerpt(r.cur.excerpt, l.view, nil)
		}
	}
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
		excerpt: p.excerpt, hasEx: p.hasEx, vs: p.vs, ve: p.ve})
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
func (r *runWalker) edges() runEdges {
	e := runEdges{leading: r.leading, hasLeading: r.hasLeading}
	if !r.open {
		return e
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

package sigscan

import (
	"fmt"
	"regexp"

	"github.com/wlame/rx-go/internal/signatures"
)

// blockRule is one rule of a block's states, with its state names
// turned into indexes: a line the rule matches (re nil: every line)
// joins the block, which then closes after the line (end) or moves to
// state next.
type blockRule struct {
	re   *regexp.Regexp
	next int
	end  bool
}

// blockProgram is a block signature as the walk runs it.
type blockProgram struct {
	// anchor is the start regex: the start rules joined, tried first on
	// an idle line (it is also what the keyword gate was derived from).
	anchor *regexp.Regexp
	// not, when set, must not match the start line.
	not *regexp.Regexp
	// rules[state] are the rules of each state, in order; state 0 is
	// "start", whose rules open a block on an idle line.
	rules [][]blockRule
	// lines > 0: the block is the start line and the next lines lines,
	// whatever they hold; rules is then unused.
	lines              int
	minLines, maxLines int
}

// compileBlock turns a BlockSpec into a program: state names become
// indexes, "start" first.
func compileBlock(spec *signatures.BlockSpec) (*blockProgram, error) {
	p := &blockProgram{anchor: spec.Start, not: spec.Not, lines: spec.Lines, minLines: spec.MinLines, maxLines: spec.MaxLines}
	if p.anchor == nil {
		return nil, fmt.Errorf("block has no start regex")
	}
	if p.lines > 0 {
		return p, nil
	}
	index := map[string]int{signatures.StateStart: 0}
	for _, r := range spec.States {
		if _, ok := index[r.State]; !ok {
			index[r.State] = len(index)
		}
	}
	p.rules = make([][]blockRule, len(index))
	for _, r := range spec.States {
		rule := blockRule{end: r.End}
		if !everyLineRegex(r.Regex) {
			rule.re = r.Regex
		}
		if !r.End {
			next, ok := index[r.Next]
			if !ok {
				return nil, fmt.Errorf("block rule moves to state %q, which has no rules", r.Next)
			}
			rule.next = next
		}
		p.rules[index[r.State]] = append(p.rules[index[r.State]], rule)
	}
	if len(p.rules[0]) == 0 {
		return nil, fmt.Errorf("block has no start rule")
	}
	return p, nil
}

// blockState is an open block: where it starts, which state it is in,
// and how far it reaches. Line positions are line indexes relative to
// the walk that holds the state (negative for a block carried in from
// an earlier chunk).
type blockState struct {
	state int
	// remaining counts the lines a `lines: N` block still takes.
	remaining int
	startIdx  int64
	startOff  int64
	// lines is how many lines the block holds, the start included.
	lines int
	// lastIdx and lastEnd are the block's last joined line that is not
	// blank, and the offset after it: where the finding ends once
	// trailing blank lines are dropped. The start line always counts.
	lastIdx int64
	lastEnd int64
	// The start line's excerpt and value span, when they were built,
	// and whether a mask is in the excerpt.
	excerpt []byte
	hasEx   bool
	vs, ve  int
	masked  bool
}

// blockWalker runs one block signature over consecutive lines. Blocks
// of one signature never overlap, so a walker holds at most one open
// block.
type blockWalker struct {
	cs   *compiledSig
	prog *blockProgram
	ex   *excerpter
	open bool
	st   blockState
	// joinedAt is the index of the last line an open block joined: such
	// a line is never tested as a new start, even when the block closed
	// after it.
	joinedAt int64
}

func newBlockWalker(cs *compiledSig, ex *excerpter) *blockWalker {
	return &blockWalker{cs: cs, prog: cs.block, ex: ex, joinedAt: -1 << 62}
}

// feed offers line l (index idx) to the open block. It reports whether
// the block took the line; a block that did not take it has closed
// before it, and the line may then be tested as a new start.
func (b *blockWalker) feed(l *line, idx int64, out sink) bool {
	p := b.prog
	closeAfter := false
	if p.lines > 0 {
		b.st.remaining--
		closeAfter = b.st.remaining == 0
	} else {
		rule, ok := b.firstRule(b.st.state, l.view)
		if !ok {
			b.close(out)
			return false
		}
		closeAfter = rule.end
		b.st.state = rule.next
	}
	b.join(l, idx)
	if closeAfter || b.st.lines >= p.maxLines {
		b.close(out)
	}
	return true
}

// firstRule returns the first rule of state that matches view.
func (b *blockWalker) firstRule(state int, view []byte) (blockRule, bool) {
	for _, r := range b.prog.rules[state] {
		if r.re == nil || r.re.Match(view) {
			return r, true
		}
	}
	return blockRule{}, false
}

// join adds line l to the open block.
func (b *blockWalker) join(l *line, idx int64) {
	b.st.lines++
	b.joinedAt = idx
	if !isBlank(l.view) {
		b.st.lastIdx = idx
		b.st.lastEnd = l.next
	}
}

// tryOpen tests line l as the start of a block: the anchor matches,
// `not` does not, and (states form) a start rule matches. The caller
// has checked the keyword gate.
func (b *blockWalker) tryOpen(l *line, idx int64, out sink) {
	p := b.prog
	if !p.anchor.Match(l.view) || (p.not != nil && p.not.Match(l.view)) {
		return
	}
	st := blockState{startIdx: idx, startOff: l.start, lines: 1, lastIdx: idx, lastEnd: l.next, remaining: p.lines, vs: -1, ve: -1}
	closeAfter := false
	if p.lines == 0 {
		rule := p.rules[0][0]
		if len(p.rules[0]) > 1 {
			var ok bool
			if rule, ok = b.firstRule(0, l.view); !ok {
				return
			}
		}
		st.state = rule.next
		closeAfter = rule.end
	}
	if out.mayStore() {
		st.hasEx = true
		st.excerpt, st.masked, st.vs, st.ve = anchorExcerpt(b.st.excerpt[:0], b.ex, b.cs, p.anchor, l)
	}
	b.open, b.st, b.joinedAt = true, st, idx
	if closeAfter || p.maxLines <= 1 {
		b.close(out)
	}
}

// close ends the open block: trailing blank lines are dropped, and the
// block is a finding when it still holds at least min_lines lines.
// Either way it owned the lines it joined.
func (b *blockWalker) close(out sink) {
	b.open = false
	st := &b.st
	if st.lastIdx-st.startIdx+1 < int64(b.prog.minLines) {
		return
	}
	out.add(b.cs, &rawFinding{startIdx: st.startIdx, endIdx: st.lastIdx, startOff: st.startOff, endOff: st.lastEnd,
		excerpt: st.excerpt, hasEx: st.hasEx, vs: st.vs, ve: st.ve, masked: st.masked})
}

// finishAtEnd closes an open block at the end of the text.
func (b *blockWalker) finishAtEnd(out sink) {
	if b.open {
		b.close(out)
	}
}

// sameState reports whether two walkers of one signature are in the
// same state: both idle, or both holding the block that started on the
// same line. From such a line on, two walks over the same lines agree.
func (b *blockWalker) sameState(o *blockWalker) bool {
	if b.open != o.open {
		return false
	}
	return !b.open || b.st.startIdx == o.st.startIdx
}

// anchorExcerpt builds the excerpt of an anchor line l into dst,
// masked by every redactor (excerpter.build), and reports whether it
// holds a mask. The finding is marked masked when its own signature
// redacts, too. For a redacting signature the anchor's first match is
// the value (vs, ve, counted from the line's first byte); otherwise they
// are -1.
func anchorExcerpt(dst []byte, ex *excerpter, cs *compiledSig, anchor *regexp.Regexp, l *line) (excerpt []byte, masked bool, vs, ve int) {
	excerpt, masked = ex.build(dst, l.view)
	vs, ve = -1, -1
	if cs.redact {
		if loc := anchor.FindIndex(l.view); loc != nil {
			vs, ve = loc[0]+l.shift, loc[1]+l.shift
		}
	}
	return excerpt, masked || cs.redact, vs, ve
}

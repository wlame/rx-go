package sigscan

import "context"

// The merge takes the chunk results in text order and builds the answer
// a walk of the whole text in one piece would give.
//
// Line numbers: a chunk numbers its lines from 0; the merge adds the
// lines of every chunk before it.
//
// Regex findings need nothing more: a regex signature looks at one line
// at a time.
//
// Runs are joined: a chunk reports the run that begins on its first
// line (leading) and the run still open at its last line (trailing)
// without judging them. The merge carries a trailing run into the next
// chunk and extends it with that chunk's leading run when the two touch
// (both edge lines qualify; for `same`, the two edge lines are equal),
// and applies min once the run has ended.
//
// Blocks are the subtle part. A chunk's walk starts with no open block,
// but the true walk of the text may enter the chunk inside a block that
// started earlier. When it does, the merge walks the chunk's lines
// again for that signature, twice at once: the true walk, which goes on
// with the carried block, and a copy of the chunk's own walk, which
// starts with none. The findings the true walk makes replace the
// chunk's until the two walks are in the same state after the same line
// (both idle, or holding the same block): from there on they agree, so
// the chunk's own findings after that line are taken as they are, and
// the ones before it, which the copy counted, are dropped. When the two
// never agree inside the chunk, the true walk's state at its end is
// carried into the next chunk in the same way.
//
// The cap is applied here, in text order: the first Cap findings of a
// signature are stored, and every finding is counted.
//
// Times: the merge carries the last stamped line of the text merged so
// far (lastStamp). A chunk's findings whose first line had no stamped
// line before it in the chunk are pending; before anything else, the
// merge settles them from lastStamp by the lookback rule (times.go),
// together with the chunk's edge runs and open block, which may become
// findings later. The replay walks the chunk again with a tracker that
// starts from lastStamp, so the blocks it finishes get their times
// directly.

// replayCheckLines is how many lines a replay walks between two looks
// at the scan's context, so a cancel ends a long replay soon.
const replayCheckLines = 1024

// merger holds the state of the merge between chunks.
type merger struct {
	// ctx is the scan's context: a replay stops when it is canceled.
	ctx  context.Context
	e    *engine
	sigs []sigMerge
	// base is the number of lines before the next chunk.
	base int64
	// The last line's view and length, for `same` runs.
	lastView []byte
	lastLen  int64
	// rewalk walks the lines of a chunk again, from its first line,
	// until fn returns false.
	rewalk  func(res *chunkResult, fn func(l *line) bool) error
	scratch *gateScratch
	// onRewalkLine, when set, is called for each line a replay walks
	// (tests).
	onRewalkLine func()
	// ex builds the excerpts of the blocks a replay finds.
	ex *excerpter
	// lastStamp is the last stamped line of the text before the next
	// chunk.
	lastStamp stampRef
	// stamped is how many lines merged so far have a timestamp of their
	// own.
	stamped int64
}

// sigMerge is the merge's state for one signature.
type sigMerge struct {
	stored []Finding
	count  int64
	// carry is a block open across the edge before the next chunk, its
	// line indexes relative to that chunk's first line (so negative).
	carry *blockState
	// run is a run open across that edge, with absolute 0-based line
	// indexes.
	run    runPiece
	hasRun bool
	// long is a long-line measure's merge.
	long *longLineMerge
}

func newMerger(ctx context.Context, e *engine, onRewalkLine func(), rewalk func(res *chunkResult, fn func(l *line) bool) error) *merger {
	m := &merger{ctx: ctx, e: e, sigs: make([]sigMerge, len(e.sigs)), rewalk: rewalk, scratch: e.gate.newScratch(),
		onRewalkLine: onRewalkLine, ex: newExcerpter(e)}
	for _, si := range e.longLines {
		m.sigs[si].long = &longLineMerge{spec: *e.sigs[si].long}
	}
	return m
}

// add merges the next chunk, in text order.
func (m *merger) add(res *chunkResult) error {
	if res.lines == 0 {
		// An empty chunk (a long line swallowed its start) changes
		// nothing, and its edges are not edges of any line.
		return nil
	}
	m.settleTimes(res)
	replayed := make(map[int32]bool)
	var replay []int32
	for _, si := range m.e.blocks {
		if m.sigs[si].carry != nil {
			replay = append(replay, si)
			replayed[si] = true
		}
	}
	if len(replay) > 0 {
		if err := m.replayBlocks(res, replay); err != nil {
			return err
		}
	}
	for si, cs := range m.e.sigs {
		idx := int32(si)
		sc := &res.sigs[si]
		switch {
		case cs.run != nil:
			m.mergeRuns(idx, res)
		case cs.block != nil && !replayed[idx]:
			m.take(idx, sc.findings, sc.count)
			m.sigs[si].carry = carried(sc.open, res.lines)
		case cs.regex != nil, cs.value != nil:
			m.take(idx, sc.findings, sc.count)
		case cs.long != nil:
			m.sigs[si].long.add(m, idx, sc, m.base, res.lines)
		}
	}
	m.base += res.lines
	m.stamped += res.stamped
	if m.e.hasSame {
		m.lastView, m.lastLen = res.lastView, res.lastLen
	}
	if res.lastStamp.ok {
		m.lastStamp = res.lastStamp
	}
	return nil
}

// settleTimes decides the pending times of a chunk's findings, edge
// runs and open blocks from the last stamped line before the chunk.
func (m *merger) settleTimes(res *chunkResult) {
	if m.e.times == nil {
		return
	}
	carry, lookback := m.lastStamp, m.e.times.lookback
	for si := range res.sigs {
		sc := &res.sigs[si]
		for _, i := range sc.pending {
			f := &sc.findings[i]
			tm := timeRef{state: timePending}
			tm.settle(f.StartOffset, carry, lookback)
			f.TimeMS = tm.pointer()
		}
		sc.pending = nil
		sc.runs.leading.tm.settle(sc.runs.leading.startOff, carry, lookback)
		sc.runs.trailing.tm.settle(sc.runs.trailing.startOff, carry, lookback)
		if sc.open != nil {
			sc.open.tm.settle(sc.open.startOff, carry, lookback)
		}
		if sc.long != nil {
			for i := range sc.long.cands {
				c := &sc.long.cands[i]
				c.tm.settle(c.start, carry, lookback)
			}
		}
	}
}

// finish closes what is still open at the end of the text.
func (m *merger) finish() {
	for _, si := range m.e.runs {
		m.closeRun(si)
	}
	for _, si := range m.e.blocks {
		ms := &m.sigs[si]
		if ms.carry == nil {
			continue
		}
		b := newBlockWalker(m.e.sigs[si], m.ex, nil)
		b.open, b.st = true, *ms.carry
		b.close(m.storeSink(si, m.base+1, true))
		ms.carry = nil
	}
	for _, si := range m.e.longLines {
		m.sigs[si].long.decide(m.e.sigs[si], m.storeSink(si, 1, true), true)
	}
	m.dropUnprefixedBelowShare()
}

// dropUnprefixedBelowShare empties the result of an unprefixed measure
// when the file's stamped lines are under its min_share: in such a file
// lines without a timestamp are the rule, not a finding. The share is
// known only once every line is merged.
func (m *merger) dropUnprefixedBelowShare() {
	for _, si := range m.e.runs {
		cs := m.e.sigs[si]
		if cs.run.mode != runUnprefixed {
			continue
		}
		if m.base == 0 || float64(m.stamped) < cs.minShare*float64(m.base) {
			m.sigs[si].stored, m.sigs[si].count = nil, 0
		}
	}
}

// result is the merged answer.
func (m *merger) result() map[string]*SignatureResult {
	out := make(map[string]*SignatureResult, len(m.e.sigs))
	for si, cs := range m.e.sigs {
		findings := m.sigs[si].stored
		if findings == nil {
			findings = []Finding{}
		}
		out[cs.sig.ID] = &SignatureResult{Findings: findings, Count: m.sigs[si].count}
	}
	return out
}

// take adds a chunk's findings of a signature, numbered against the
// text, and its count.
func (m *merger) take(si int32, findings []Finding, count int64) {
	ms := &m.sigs[si]
	ms.count += count
	for _, f := range findings {
		if len(ms.stored) >= m.e.cap {
			break
		}
		f.StartLine += m.base + 1
		f.EndLine += m.base + 1
		ms.stored = append(ms.stored, f)
	}
	m.noteFull(si)
}

// noteFull tells the workers when a signature has all the findings it
// can store, so they stop building them.
func (m *merger) noteFull(si int32) {
	if len(m.sigs[si].stored) >= m.e.cap {
		m.e.full[si].Store(true)
	}
}

// carried turns a chunk's open block into the carry for the edge after
// the chunk: its line indexes become relative to the next chunk.
func carried(open *blockState, chunkLines int64) *blockState {
	if open == nil {
		return nil
	}
	st := *open
	st.startIdx -= chunkLines
	st.lastIdx -= chunkLines
	return &st
}

// mergeRuns joins a run signature's edge runs with the carry.
func (m *merger) mergeRuns(si int32, res *chunkResult) {
	ms := &m.sigs[si]
	edges := res.sigs[si].runs
	prog := m.e.sigs[si].run
	touching := ms.hasRun && edges.hasLeading &&
		(prog.mode != runSame || keysEqual(m.lastView, m.lastLen, res.firstView, res.firstLen))
	switch {
	case touching:
		ms.run.endIdx = m.base + edges.leading.endIdx
		ms.run.endOff = edges.leading.endOff
		if edges.whole {
			return
		}
		m.closeRun(si)
	default:
		m.closeRun(si)
		if edges.hasLeading {
			leading := m.absolute(edges.leading)
			if edges.whole {
				ms.run, ms.hasRun = leading, true
				return
			}
			m.emitRun(si, &leading)
		}
	}
	m.take(si, res.sigs[si].findings, res.sigs[si].count)
	if edges.hasTrailing {
		ms.run, ms.hasRun = m.absolute(edges.trailing), true
	}
}

// absolute renumbers a chunk's run piece against the text.
func (m *merger) absolute(p runPiece) runPiece {
	p.startIdx += m.base
	p.endIdx += m.base
	return p
}

// closeRun ends the carried run of a signature, a finding when it is at
// least min lines long.
func (m *merger) closeRun(si int32) {
	ms := &m.sigs[si]
	if !ms.hasRun {
		return
	}
	ms.hasRun = false
	m.emitRun(si, &ms.run)
}

// emitRun adds a run with absolute line indexes when it is long enough.
func (m *merger) emitRun(si int32, p *runPiece) {
	if p.lines() < m.e.sigs[si].run.min {
		return
	}
	emitPiece(m.e.sigs[si], p, m.storeSink(si, 1, true))
}

// storeSink is a sink into the merged answer of signature si, numbering
// lines from lineBase. counts says whether a finding added also counts
// (it does not while a replay regenerates findings already counted).
func (m *merger) storeSink(si int32, lineBase int64, counts bool) *mergeSink {
	return &mergeSink{m: m, si: si, lineBase: lineBase, counts: counts}
}

// mergeSink adds findings to the merged answer.
type mergeSink struct {
	m        *merger
	si       int32
	lineBase int64
	counts   bool
	added    int64
}

func (s *mergeSink) storing() bool  { return len(s.m.sigs[s.si].stored) < s.m.e.cap }
func (s *mergeSink) mayStore() bool { return s.storing() }

func (s *mergeSink) add(cs *compiledSig, f *rawFinding) {
	ms := &s.m.sigs[s.si]
	s.added++
	if s.counts {
		ms.count++
	}
	if s.storing() {
		ms.stored = append(ms.stored, toFinding(cs, f, s.lineBase))
		s.m.noteFull(s.si)
	}
}

// countSink only counts findings: the replay's copy of a chunk's own
// walk.
type countSink struct{ n int64 }

func (s *countSink) storing() bool                 { return false }
func (s *countSink) mayStore() bool                { return false }
func (s *countSink) add(*compiledSig, *rawFinding) { s.n++ }

// replay is the state of one signature while the merge walks a chunk
// again.
type replay struct {
	si        int32
	truth     *blockWalker // the true walk, entering inside the carried block
	own       *blockWalker // a copy of the chunk's own walk, entering idle
	trueSink  *mergeSink
	ownSink   *countSink
	converged bool
	// extend: the two walks agree, but the chunk stored fewer findings
	// after that point than the answer still needs, so the true walk
	// goes on storing them (without counting: the chunk counted them).
	extend bool
	done   bool
}

// replayBlocks walks chunk res again for the block signatures that
// enter it inside an open block, and merges their findings.
func (m *merger) replayBlocks(res *chunkResult, sigs []int32) error {
	// The replay's tracker starts from the stamps of the whole text
	// before the chunk, so nothing it finds is pending.
	times := &timeTracker{src: m.e.times, last: m.lastStamp, settled: true}
	reps := make([]*replay, len(sigs))
	for i, si := range sigs {
		cs := m.e.sigs[si]
		rp := &replay{si: si, truth: newBlockWalker(cs, m.ex, times), own: newBlockWalker(cs, m.ex, times),
			trueSink: m.storeSink(si, m.base+1, true), ownSink: &countSink{}}
		rp.truth.open, rp.truth.st, rp.truth.joinedAt = true, *m.sigs[si].carry, -1
		m.sigs[si].carry = nil
		reps[i] = rp
	}
	active := len(reps)
	idx := int64(0)
	var canceled error
	err := m.rewalk(res, func(l *line) bool {
		// A replay that does not converge walks a whole chunk on this
		// goroutine; looking at the context every replayCheckLines lines
		// lets a cancel end it soon.
		if idx%replayCheckLines == replayCheckLines-1 {
			if canceled = m.ctx.Err(); canceled != nil {
				return false
			}
		}
		if m.onRewalkLine != nil {
			m.onRewalkLine()
		}
		times.observe(l)
		m.e.gate.evaluate(l.view, m.scratch)
		for _, rp := range reps {
			if rp.done {
				continue
			}
			passed := m.e.gate.passes(rp.si, m.scratch)
			stepBlock(rp.truth, l, idx, passed, rp.trueSink)
			if !rp.extend {
				stepBlock(rp.own, l, idx, passed, rp.ownSink)
				if rp.truth.sameState(rp.own) {
					m.converge(rp, res)
				}
			}
			if rp.extend && !rp.trueSink.storing() {
				rp.done = true
			}
			if rp.done {
				active--
			}
		}
		idx++
		return active > 0
	})
	if err != nil {
		return err
	}
	if canceled != nil {
		return canceled
	}
	for _, rp := range reps {
		if rp.done {
			continue
		}
		if res.final {
			rp.truth.finishAtEnd(rp.trueSink)
			continue
		}
		if rp.extend {
			continue
		}
		// The walks never agreed inside the chunk: the true walk is the
		// answer for all of it, and its state goes on to the next edge.
		if rp.truth.open {
			m.sigs[rp.si].carry = carried(&rp.truth.st, res.lines)
		}
	}
	return nil
}

// stepBlock is one line of a block walk: an open block takes the line
// or closes before it; a line no block took may start one when the gate
// passed.
func stepBlock(b *blockWalker, l *line, idx int64, passed bool, out sink) {
	if b.open {
		b.feed(l, idx, out)
	}
	if !b.open && b.joinedAt != idx && passed {
		b.tryOpen(l, idx, out)
	}
}

// converge is called on the first line after which the true walk and
// the chunk's own walk agree: the chunk's findings from there on are the
// answer's, and the ones the own walk made before (counted by ownSink)
// are dropped.
func (m *merger) converge(rp *replay, res *chunkResult) {
	rp.converged = true
	ms := &m.sigs[rp.si]
	sc := &res.sigs[rp.si]
	dropped := rp.ownSink.n
	ms.count += sc.count - dropped
	ms.carry = carried(sc.open, res.lines)
	var rest []Finding
	if dropped < int64(len(sc.findings)) {
		rest = sc.findings[dropped:]
	}
	need := m.e.cap - len(ms.stored)
	truncated := sc.count > int64(len(sc.findings))
	if need <= 0 || !truncated || len(rest) >= need {
		m.take(rp.si, rest, 0)
		rp.done = true
		return
	}
	// The chunk stopped storing before the answer has enough: the true
	// walk, which now walks exactly like the chunk's, stores the rest.
	rp.extend = true
	rp.trueSink.counts = false
}

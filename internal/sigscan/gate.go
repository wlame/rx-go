package sigscan

import (
	"slices"

	"github.com/wlame/rx-go/internal/signatures"
)

// gate decides, for one line, which signatures may be tested on it: a
// signature with keyword clauses only when the line holds a member of
// every clause (compared ASCII case-insensitively), a signature without
// clauses always.
//
// All keywords of all selected signatures go into one automaton. Each
// keyword carries the ids of the clauses it belongs to (a keyword may be
// a member of several clauses, of several signatures), and each clause
// knows its signature. One pass over the line finds every keyword
// present; each clause seen for the first time on the line counts once
// for its signature, and a signature whose count reaches its number of
// clauses passes.
//
// A gate is never changed after it is built; the per-line counters live
// in a gateScratch, one per goroutine.
type gate struct {
	dfa *keywordDFA // nil when no selected signature has clauses
	// clauseSig maps a clause id to the index of its signature.
	clauseSig []int32
	// need maps a signature index to its number of clauses; 0 for a
	// signature without a gate.
	need []int32
}

// newGate builds the gate of sigs; a signature's index in sigs is the
// index the gate reports.
func newGate(sigs []*signatures.Signature) (*gate, error) {
	g := &gate{need: make([]int32, len(sigs))}
	keywordIDs := map[string][]int32{}
	var keywords []string
	for si, sig := range sigs {
		// #nosec G115 -- an anchor keeps at most 32 clauses, and a
		// selection is at most the loaded signatures (a few thousand),
		// so every count here is far inside an int32.
		g.need[si] = int32(len(sig.Clauses))
		for _, clause := range sig.Clauses {
			id := int32(len(g.clauseSig))                // #nosec G115 -- see above
			g.clauseSig = append(g.clauseSig, int32(si)) // #nosec G115 -- see above
			for _, member := range clause {
				if _, seen := keywordIDs[member]; !seen {
					keywords = append(keywords, member)
				}
				keywordIDs[member] = append(keywordIDs[member], id)
			}
		}
	}
	if len(keywords) == 0 {
		return g, nil
	}
	ids := make([][]int32, len(keywords))
	for i, k := range keywords {
		ids[i] = slices.Compact(keywordIDs[k])
	}
	dfa, err := buildDFA(keywords, ids)
	if err != nil {
		return nil, err
	}
	g.dfa = dfa
	return g, nil
}

// gateScratch holds one goroutine's counters for the gate. Stamps
// replace clearing: a counter belongs to the current line only when its
// stamp is the line's generation, so starting a line costs one
// increment, not a pass over every clause.
type gateScratch struct {
	gen         uint32
	clauseStamp []uint32
	sigStamp    []uint32
	sigHits     []int32
	// passed lists the gated signatures that passed on the current line.
	passed []int32
}

// newScratch returns counters sized for the gate.
func (g *gate) newScratch() *gateScratch {
	return &gateScratch{
		clauseStamp: make([]uint32, len(g.clauseSig)),
		sigStamp:    make([]uint32, len(g.need)),
		sigHits:     make([]int32, len(g.need)),
	}
}

// evaluate runs the automaton over view and records which gated
// signatures pass; passes then answers for each signature.
//
// The loop is the hot path of a scan: one table lookup per byte, and
// work only where a keyword ends.
func (g *gate) evaluate(view []byte, s *gateScratch) {
	s.passed = s.passed[:0]
	s.gen++
	if s.gen == 0 {
		// The stamps wrapped around after 2^32 lines: forget them all.
		clear(s.clauseStamp)
		clear(s.sigStamp)
		s.gen = 1
	}
	d := g.dfa
	if d == nil {
		return
	}
	row := uint32(0)
	lastHit := ^uint32(0)
	for _, b := range view {
		v := d.trans[row+uint32(d.class[b])]
		row = v >> 1
		if v&1 == 0 || row == lastHit {
			continue
		}
		// A keyword ends here. The same state reached again on the
		// next byte ("aaaa" for the keyword "aaa") adds nothing.
		lastHit = row
		g.countClauses(d.outputOfRow(row), s)
	}
}

// countClauses counts each clause of out once per line for its
// signature, and lists a signature when its last clause is seen.
func (g *gate) countClauses(out []int32, s *gateScratch) {
	for _, clause := range out {
		if s.clauseStamp[clause] == s.gen {
			continue
		}
		s.clauseStamp[clause] = s.gen
		sig := g.clauseSig[clause]
		if s.sigStamp[sig] != s.gen {
			s.sigStamp[sig] = s.gen
			s.sigHits[sig] = 0
		}
		s.sigHits[sig]++
		if s.sigHits[sig] == g.need[sig] {
			s.passed = append(s.passed, sig)
		}
	}
}

// passes reports whether signature sig may be tested on the line last
// evaluated: always for a signature without clauses, and for a gated
// one when every clause had a member on the line.
func (g *gate) passes(sig int32, s *gateScratch) bool {
	need := g.need[sig]
	return need == 0 || (s.sigStamp[sig] == s.gen && s.sigHits[sig] == need)
}

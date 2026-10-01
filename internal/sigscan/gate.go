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
// One exception: a line that holds a fold letter (foldLetters) passes
// every signature. Under (?i) Go's regexp matches k to the Kelvin sign
// and s to the long s, and a keyword holds only the ASCII letter, so the
// keywords alone would turn such a match away. The fold letters are
// keywords of the same automaton with the id foldID, so finding them
// costs no extra pass over the line. A letter cut in half by the end of
// the line view is not in the view, and the anchor, which sees the same
// view, cannot match through it either.
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
	// gated lists the signatures with clauses, in index order: the
	// signatures a fold letter lets through.
	gated []int32
}

// foldLetters are the two letters outside ASCII that Go's
// case-insensitive matching equates with ASCII letters: the Kelvin sign
// (U+212A, UTF-8 E2 84 AA) with k, and the long s (U+017F, UTF-8 C5 BF)
// with s. No other ASCII letter folds to a rune outside ASCII.
var foldLetters = []string{"\u212a", "\u017f"}

// foldID is the id the automaton reports for a fold letter. Clause ids
// count from 0, so it is never one of them, and as the smallest id it
// comes first in a state's sorted output.
const foldID int32 = -1

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
		if len(sig.Clauses) > 0 {
			g.gated = append(g.gated, int32(si)) // #nosec G115 -- see above
		}
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
	if len(g.gated) == 0 {
		return g, nil
	}
	// The fold letters join the keywords. A declared keyword that is a
	// fold letter itself shares its entry, and reports both ids.
	for _, letter := range foldLetters {
		if _, seen := keywordIDs[letter]; !seen {
			keywords = append(keywords, letter)
		}
		keywordIDs[letter] = append(keywordIDs[letter], foldID)
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
	// foldGen is the generation of the last line that held a fold
	// letter: on that line every signature passes.
	foldGen uint32
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
		s.foldGen = 0
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
		if g.countClauses(d.outputOfRow(row), s) {
			// A fold letter: every signature passes, and nothing
			// later on the line can change that.
			return
		}
	}
}

// countClauses counts each clause of out once per line for its
// signature, and lists a signature when its last clause is seen. It
// returns true when out holds a fold letter, after listing every gated
// signature as passed.
func (g *gate) countClauses(out []int32, s *gateScratch) bool {
	for _, clause := range out {
		if clause == foldID {
			s.foldGen = s.gen
			s.passed = append(s.passed[:0], g.gated...)
			return true
		}
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
	return false
}

// passes reports whether signature sig may be tested on the line last
// evaluated: always for a signature without clauses or on a line that
// holds a fold letter, and for a gated one when every clause had a
// member on the line.
func (g *gate) passes(sig int32, s *gateScratch) bool {
	need := g.need[sig]
	return need == 0 || s.foldGen == s.gen || (s.sigStamp[sig] == s.gen && s.sigHits[sig] == need)
}

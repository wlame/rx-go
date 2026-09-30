package signatures

import (
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The bounds of the keyword walk.
const (
	// minKeywordBytes is the shortest literal a clause may hold: a
	// clause with a shorter member is dropped, since a two-byte keyword
	// is on almost every line and gates nothing.
	minKeywordBytes = 3
	// maxSetLiterals is the most literals one set may hold: an exact,
	// prefix or suffix set, a cross product, or a clause.
	maxSetLiterals = 64
	// maxCrossBytes is the longest literal a cross product may make. A
	// single literal of the regex keeps its full length.
	maxCrossBytes = 32
	// maxClauses is the most clauses one anchor keeps, the first ones
	// in the regex. Dropping a clause only lets more lines through the
	// gate; the bound keeps pruneClauses, which compares every pair,
	// cheap for any regex a file may hold.
	maxClauses = 32
)

// deriveClauses parses an anchor regex and walks its syntax tree for
// keyword clauses: sets of lowered literals of at least 3 bytes such
// that every line the regex matches holds a member of every set
// (compared ASCII case-insensitively). It returns nil when the regex
// gives no clause.
//
// The regex is parsed with the flags regexp.Compile uses (syntax.Perl),
// so the tree walked is the tree that matches.
func deriveClauses(source string) ([][]string, error) {
	tree, err := syntax.Parse(source, syntax.Perl)
	if err != nil {
		return nil, err
	}
	clauses := analyze(tree).clauses
	if len(clauses) > maxClauses {
		clauses = clauses[:maxClauses]
	}
	return pruneClauses(clauses), nil
}

// facts is what the walk knows about the strings one node of the
// syntax tree matches. Every field is a promise about every match of
// the node; a nil set means "nothing known".
//
//   - exact: the complete list of strings the node can match, when it is
//     short (a literal, an alternation of literals). An empty-width
//     assertion (^, $, \b) matches only "".
//   - prefix: every match starts with a member.
//   - suffix: every match ends with a member.
//   - clauses: every match contains a member of each clause. Only usable
//     clauses are kept (see usableClause).
//
// Literals are kept lowered to ASCII lower case throughout.
type facts struct {
	exact, prefix, suffix []string
	clauses               [][]string
}

// exactFacts are the facts of a node whose matches are exactly set: it
// is its own prefix and suffix, and a clause when usable.
func exactFacts(set []string) facts {
	f := facts{exact: set, prefix: set, suffix: set}
	if clause := usableClause(set); clause != nil {
		f.clauses = [][]string{clause}
	}
	return f
}

// emptyWidth are the facts of a node that matches only the empty
// string: an assertion such as ^, $ or \b. It joins the literals around
// it as if it were not there ("\bERROR\b:" holds "error:").
var emptyWidth = exactFacts([]string{""})

// analyze returns the facts of one node of the syntax tree. The walk is
// recursive; the parser bounds the depth of the tree.
func analyze(re *syntax.Regexp) facts {
	switch re.Op {
	case syntax.OpLiteral:
		return analyzeSequence(literalItems(re))
	case syntax.OpConcat:
		return analyzeSequence(concatItems(re))
	case syntax.OpAlternate:
		return analyzeAlternation(re.Sub)
	case syntax.OpCapture:
		// A capture group matches what its content matches.
		return analyze(re.Sub[0])
	case syntax.OpPlus:
		return repeated(analyze(re.Sub[0]))
	case syntax.OpRepeat:
		// x{n,m} with n ≥ 1 holds at least one x; x{1,1} is x itself.
		if re.Min < 1 {
			return facts{}
		}
		inner := analyze(re.Sub[0])
		if re.Min == 1 && re.Max == 1 {
			return inner
		}
		return repeated(inner)
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText,
		syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return emptyWidth
	}
	// A star, a question mark, a class, any character: the node may
	// match the empty string or one of many characters, so nothing is
	// known about it.
	return facts{}
}

// repeated are the facts of one or more copies of a node: each match
// starts with the node's prefix, ends with its suffix and holds its
// clauses, but its exact strings are lost.
func repeated(inner facts) facts {
	return facts{prefix: inner.prefix, suffix: inner.suffix, clauses: inner.clauses}
}

// concatItems lists the facts of a concatenation's children in order. A
// nested concatenation is flattened into it, and a literal becomes the
// pieces literalItems gives, so adjacent literals of different flags
// ("[Ee]rror" is a case-folded "E" then "rror") are joined.
func concatItems(re *syntax.Regexp) []facts {
	var items []facts
	for _, sub := range re.Sub {
		switch sub.Op {
		case syntax.OpConcat:
			items = append(items, concatItems(sub)...)
		case syntax.OpLiteral:
			items = append(items, literalItems(sub)...)
		default:
			items = append(items, analyze(sub))
		}
	}
	return items
}

// literalItems splits a literal into pieces whose bytes every match
// holds, lowered to ASCII lower case.
//
// A case-insensitive literal is stored by the parser in its smallest
// case (DELETE for (?i)delete); lowering it is enough for ASCII letters,
// since the gate compares ASCII case-insensitively. A non-ASCII letter
// that has another case (É and é) cannot be compared that way, so it
// ends the piece and is an unknown item itself. The same goes for
// U+FFFD: Go's regexp matches it against any byte that is not valid
// UTF-8, which the line does not hold as these bytes.
//
// A case-insensitive k or s also matches the Kelvin sign (U+212A) and
// the long s (U+017F). The gate treats them as the ASCII letters only:
// it is part of the signature's meaning, and the docs say so.
func literalItems(re *syntax.Regexp) []facts {
	folds := re.Flags&syntax.FoldCase != 0
	var items []facts
	var piece []byte
	flush := func() {
		if len(piece) > 0 {
			items = append(items, exactFacts([]string{string(piece)}))
			piece = piece[:0]
		}
	}
	for _, r := range re.Rune {
		if r == utf8.RuneError || (folds && r >= utf8.RuneSelf && unicode.SimpleFold(r) != r) {
			flush()
			items = append(items, facts{})
			continue
		}
		piece = utf8.AppendRune(piece, asciiLowerRune(r))
	}
	flush()
	return items
}

// analyzeSequence returns the facts of items matched one after the
// other.
//
// Adjacent items with exact sets are joined into runs by cross product
// ("A" then "KIA|SIA" is "AKIA|ASIA"), as long as the product stays
// within maxSetLiterals literals of maxCrossBytes bytes; past that the
// run ends and a new one starts. A run then joins the prefix set of the
// item after it and the suffix set of the item before it, under the
// same bounds: "A" then (3T[A-Z0-9]|KIA) is A3T|AKIA, because every
// match of the alternation starts with 3T or KIA. Each run is a clause;
// every other item adds its own clauses.
func analyzeSequence(items []facts) facts {
	segments := joinExactRuns(items)
	if len(segments) == 0 {
		return emptyWidth
	}
	if len(segments) == 1 && segments[0].exact != nil {
		return exactFacts(segments[0].exact)
	}
	var out facts
	for i, seg := range segments {
		if seg.exact == nil {
			out.clauses = append(out.clauses, seg.clauses...)
			continue
		}
		if clause := usableClause(extendRun(segments, i)); clause != nil {
			out.clauses = append(out.clauses, clause)
		}
	}
	out.prefix = sequencePrefix(segments)
	out.suffix = sequenceSuffix(segments)
	return out
}

// joinExactRuns merges adjacent items with exact sets by cross product
// and returns the segments: runs (exact set kept) and the other items
// as they are.
func joinExactRuns(items []facts) []facts {
	var segments []facts
	var run []string
	for _, item := range items {
		if item.exact == nil {
			if run != nil {
				segments = append(segments, exactFacts(run))
				run = nil
			}
			segments = append(segments, item)
			continue
		}
		if run != nil {
			if joined, ok := crossProduct(run, item.exact); ok {
				run = joined
				continue
			}
			segments = append(segments, exactFacts(run))
		}
		run = item.exact
	}
	if run != nil {
		segments = append(segments, exactFacts(run))
	}
	return segments
}

// extendRun returns the literals of the run at segments[i], joined with
// the prefix set of the next segment and the suffix set of the one
// before when those are known and the products stay within bounds. Only
// a segment that is not itself a run is joined: two runs side by side
// were split because their product was too large.
func extendRun(segments []facts, i int) []string {
	set := segments[i].exact
	if i+1 < len(segments) && segments[i+1].exact == nil && segments[i+1].prefix != nil {
		if joined, ok := crossProduct(set, segments[i+1].prefix); ok {
			set = joined
		}
	}
	if i > 0 && segments[i-1].exact == nil && segments[i-1].suffix != nil {
		if joined, ok := crossProduct(segments[i-1].suffix, set); ok {
			set = joined
		}
	}
	return set
}

// sequencePrefix is the prefix set of a sequence: the first run joined
// with the prefix of the segment after it, or the first segment's own
// prefix when it is not a run.
func sequencePrefix(segments []facts) []string {
	first := segments[0]
	if first.exact == nil {
		return first.prefix
	}
	if len(segments) > 1 && segments[1].exact == nil && segments[1].prefix != nil {
		if joined, ok := crossProduct(first.exact, segments[1].prefix); ok {
			return joined
		}
	}
	return first.exact
}

// sequenceSuffix is the suffix set of a sequence, the mirror of
// sequencePrefix.
func sequenceSuffix(segments []facts) []string {
	last := segments[len(segments)-1]
	if last.exact == nil {
		return last.suffix
	}
	if n := len(segments); n > 1 && segments[n-2].exact == nil && segments[n-2].suffix != nil {
		if joined, ok := crossProduct(segments[n-2].suffix, last.exact); ok {
			return joined
		}
	}
	return last.exact
}

// analyzeAlternation returns the facts of a choice between branches.
// Its exact, prefix and suffix sets are the unions of the branches'
// (when every branch has one). It gives one clause: the union of the
// best clause of each branch, since every match comes from one branch
// and holds a member of that branch's clause. A branch with no clause
// leaves the alternation with none.
func analyzeAlternation(subs []*syntax.Regexp) facts {
	branches := make([]facts, len(subs))
	for i, sub := range subs {
		branches[i] = analyze(sub)
	}
	out := facts{
		exact:  unionOf(branches, func(f facts) []string { return f.exact }),
		prefix: unionOf(branches, func(f facts) []string { return f.prefix }),
		suffix: unionOf(branches, func(f facts) []string { return f.suffix }),
	}
	var clause []string
	for _, b := range branches {
		best := bestClause(b.clauses)
		if best == nil {
			return out
		}
		var fits bool
		if clause, fits = appendUnique(clause, best); !fits {
			return out
		}
	}
	if usable := usableClause(clause); usable != nil {
		out.clauses = [][]string{usable}
	}
	return out
}

// unionOf returns the union of one set of every branch, or nil when a
// branch has no such set or the union holds more than maxSetLiterals
// literals.
func unionOf(branches []facts, set func(facts) []string) []string {
	var union []string
	for _, b := range branches {
		s := set(b)
		if s == nil {
			return nil
		}
		var fits bool
		if union, fits = appendUnique(union, s); !fits {
			return nil
		}
	}
	return union
}

// appendUnique appends the members of src that dst lacks. It stops and
// returns false as soon as dst would hold more than maxSetLiterals
// members, so a union over many branches never grows past the bound.
func appendUnique(dst, src []string) ([]string, bool) {
	for _, s := range src {
		if slices.Contains(dst, s) {
			continue
		}
		if len(dst) == maxSetLiterals {
			return dst, false
		}
		dst = append(dst, s)
	}
	return dst, true
}

// bestClause picks the clause that gates best: the one whose shortest
// member is longest (a long literal is rare on a line), then the one
// with fewer members, then the first. It returns nil for no clause.
func bestClause(clauses [][]string) []string {
	var best []string
	bestShortest := -1
	for _, c := range clauses {
		shortest := shortestLen(c)
		if shortest > bestShortest || (shortest == bestShortest && len(c) < len(best)) {
			best, bestShortest = c, shortest
		}
	}
	return best
}

// shortestLen is the length of the shortest member of a set.
func shortestLen(set []string) int {
	shortest := -1
	for _, s := range set {
		if shortest < 0 || len(s) < shortest {
			shortest = len(s)
		}
	}
	return shortest
}

// crossProduct joins every member of a with every member of b. A set
// that holds only "" (an assertion) joins as nothing. It refuses
// (false) a product of more than maxSetLiterals literals or with a
// literal longer than maxCrossBytes.
func crossProduct(a, b []string) ([]string, bool) {
	if len(a) == 1 && a[0] == "" {
		return b, true
	}
	if len(b) == 1 && b[0] == "" {
		return a, true
	}
	if len(a)*len(b) > maxSetLiterals {
		return nil, false
	}
	out := make([]string, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			if len(x)+len(y) > maxCrossBytes {
				return nil, false
			}
			out = append(out, x+y)
		}
	}
	return dedupe(out), true
}

// usableClause returns set as a clause, without repeats, when it can
// gate: not empty, at most maxSetLiterals members, every member at
// least minKeywordBytes long. Otherwise it returns nil: a short member
// would let almost every line through, so the clause is dropped (the
// rest of the walk still counts).
func usableClause(set []string) []string {
	if len(set) == 0 {
		return nil
	}
	for _, s := range set {
		if len(s) < minKeywordBytes {
			return nil
		}
	}
	clause := dedupe(set)
	if len(clause) > maxSetLiterals {
		return nil
	}
	return clause
}

// pruneClauses drops each clause another clause implies, keeping the
// earlier of two that imply each other, and returns nil when none is
// left. Dropping an implied clause changes no answer: a line that holds
// a member of the implying clause holds one of the implied clause too.
func pruneClauses(clauses [][]string) [][]string {
	kept := slices.Clone(clauses)
	// Go note: walking from the end lets a clause that equals an earlier
	// one be dropped while the earlier one is still there to imply it.
	for i := len(kept) - 1; i >= 0; i-- {
		for j := range kept {
			if j != i && implies(kept[j], kept[i]) {
				kept = slices.Delete(kept, i, i+1)
				break
			}
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// implies reports whether a line that holds a member of a always holds
// a member of b: every member of a contains a member of b.
func implies(a, b []string) bool {
	for _, x := range a {
		if !slices.ContainsFunc(b, func(y string) bool { return strings.Contains(x, y) }) {
			return false
		}
	}
	return true
}

// dedupe returns the members of set in order, each once.
func dedupe(set []string) []string {
	out := make([]string, 0, len(set))
	for _, s := range set {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// asciiLower lowers the ASCII letters of s and leaves every other byte
// as it is: the case folding the keyword gate compares with.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// asciiLowerRune lowers an ASCII letter and returns any other rune as
// it is.
func asciiLowerRune(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

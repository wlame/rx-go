package sigscan

import (
	"cmp"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"sync/atomic"

	"github.com/wlame/rx-go/internal/signatures"
)

// engine is a selection of signatures compiled for one scan: the gate
// over all their keywords, and each signature's matcher, block program
// or run program. It is built once per scan and only read afterwards,
// so every worker goroutine shares it.
type engine struct {
	sigs      []*compiledSig
	gate      *gate
	lineBytes int
	cap       int
	// lineAnchored are the signatures with an anchor but no keyword
	// clause: their anchor is tested on every line.
	lineAnchored []int32
	blocks, runs []int32
	// valueSigs are the value measures (entropy, base64-run): tested on
	// every line, one finding per line.
	valueSigs []int32
	// longLines are the long-line measures.
	longLines []int32
	// hasSame is true when a `same` run is selected: the walk then
	// keeps each line's view to compare the next line with.
	hasSame bool
	// full[i] is set by the merge once signature i has Cap findings
	// stored: a worker then only counts that signature's findings.
	full []atomic.Bool
	// redactors are every redacting signature of the scan (the selected
	// ones and Options.Redactors), and redactGate their keyword gate
	// (nil when there is none): every excerpt is masked by all of them.
	redactors  []redactor
	redactGate *gate
	// times reads the lines' timestamps; nil when the file has no
	// timestamp format. Scan sets it once it has read the head of the
	// file, before any walk starts.
	times *timeSource
}

// redactor is a redacting signature as an excerpt is masked by it: its
// anchor, and which group of a match is the value; or, for a value
// measure, the matcher of its values.
type redactor struct {
	re      *regexp.Regexp
	capture int
	values  valueMatcher
}

// appendValues appends to dst the values the redactor masks on view:
// the value of every match of its anchor, or every value its measure
// finds, at most maxRedactMatches + 1 of them.
func (r redactor) appendValues(dst []span, view []byte) []span {
	if r.values != nil {
		return appendValues(dst, r.values, view, maxRedactMatches)
	}
	return append(dst, redactMatches(r.re, r.capture, view)...)
}

// compiledSig is one signature as the scan runs it.
type compiledSig struct {
	idx    int32
	sig    *signatures.Signature
	kind   signatures.Kind
	redact bool
	regex  *regexMatcher
	block  *blockProgram
	run    *runProgram
	// value is the matcher of a value measure (entropy, base64-run).
	value valueMatcher
	// minShare is the unprefixed measure's min_share: its runs count
	// only when stamped lines are at least this share of the file.
	minShare float64
	// long is a long-line measure's parameters.
	long *longLineSpec
}

// compile builds the engine of a selection. A signature id given twice
// is scanned once. A measure is refused: this engine has none.
func compile(sigs []*signatures.Signature, opts Options) (*engine, error) {
	e := &engine{lineBytes: opts.LineBytes, cap: opts.Cap}
	if e.lineBytes <= 0 {
		e.lineBytes = DefaultLineBytes
	}
	if e.cap <= 0 {
		e.cap = DefaultCap
	}
	seen := map[string]bool{}
	var unique []*signatures.Signature
	for _, sig := range sigs {
		if !seen[sig.ID] {
			seen[sig.ID] = true
			unique = append(unique, sig)
		}
	}
	for i, sig := range unique {
		cs, err := compileSig(int32(i), sig)
		if err != nil {
			return nil, err
		}
		e.sigs = append(e.sigs, cs)
		// A measure is run by the machinery of the kind it is compiled
		// to (measures.go), so it is listed by what it was given.
		switch {
		case cs.block != nil:
			e.blocks = append(e.blocks, cs.idx)
		case cs.run != nil:
			e.runs = append(e.runs, cs.idx)
			e.hasSame = e.hasSame || cs.run.mode == runSame
		case cs.value != nil:
			e.valueSigs = append(e.valueSigs, cs.idx)
		case cs.long != nil:
			e.longLines = append(e.longLines, cs.idx)
		}
		// A json-block has no anchor and no keywords: its opener is
		// tried on every line, as an anchor without keywords is.
		if (sig.Anchor() != nil && sig.LinePass()) || (cs.block != nil && cs.block.json) {
			e.lineAnchored = append(e.lineAnchored, cs.idx)
		}
	}
	g, err := newGate(unique)
	if err != nil {
		return nil, err
	}
	e.gate = g
	e.full = make([]atomic.Bool, len(e.sigs))
	if err := e.compileRedactors(unique, opts.Redactors); err != nil {
		return nil, err
	}
	return e, nil
}

// compileRedactors keeps every redacting signature with something to
// mask, an anchor or a value measure's values, the selected ones first,
// each id once, and builds their keyword gate (a value measure has no
// keywords, so it masks every line it finds a value on).
func (e *engine) compileRedactors(selected, given []*signatures.Signature) error {
	seen := map[string]bool{}
	var sigs []*signatures.Signature
	for _, sig := range slices.Concat(selected, given) {
		if !sig.Redact || seen[sig.ID] {
			continue
		}
		r := redactor{re: sig.Anchor()}
		if sig.Regex != nil {
			r.capture = sig.Regex.Capture
		}
		if sig.Measure != nil {
			r.values = newValueMatcher(sig.Measure.Params)
		}
		if r.re == nil && r.values == nil {
			continue
		}
		seen[sig.ID] = true
		sigs = append(sigs, sig)
		e.redactors = append(e.redactors, r)
	}
	if len(sigs) == 0 {
		return nil
	}
	g, err := newGate(sigs)
	if err != nil {
		return err
	}
	e.redactGate = g
	return nil
}

// excerpter builds the excerpts of one goroutine, which owns its gate
// counters for the redactors.
type excerpter struct {
	e  *engine
	gs *gateScratch
	// values is reused from excerpt to excerpt.
	values []span
}

func newExcerpter(e *engine) *excerpter {
	x := &excerpter{e: e}
	if e.redactGate != nil {
		x.gs = e.redactGate.newScratch()
	}
	return x
}

// build appends the excerpt of a line view to dst with every value a
// redactor matches on the line masked (redaction), and reports whether
// a mask is in the excerpt. A redactor runs only on a line that passes
// its keyword gate; the values of all of them are masked together, so
// the bounds and the fail-closed rules apply to their union.
func (x *excerpter) build(dst, view []byte) ([]byte, bool) {
	if x.e.redactGate == nil {
		return appendExcerpt(dst, view), false
	}
	x.e.redactGate.evaluate(view, x.gs)
	x.values = x.values[:0]
	for i, r := range x.e.redactors {
		if len(x.values) > maxRedactMatches {
			break // already more than can be followed: the line is masked whole
		}
		if x.e.redactGate.passes(int32(i), x.gs) { // #nosec G115 -- at most the loaded signatures
			x.values = r.appendValues(x.values, view)
		}
	}
	if len(x.values) == 0 {
		return appendExcerpt(dst, view), false
	}
	slices.SortStableFunc(x.values, func(a, b span) int { return cmp.Compare(a[0], b[0]) })
	return appendRedactedExcerpt(dst, view, x.values)
}

// compileSig turns one loaded signature into what the walk runs.
func compileSig(idx int32, sig *signatures.Signature) (*compiledSig, error) {
	cs := &compiledSig{idx: idx, sig: sig, kind: sig.Kind, redact: sig.Redact}
	switch sig.Kind {
	case signatures.KindRegex:
		cs.regex = &regexMatcher{re: sig.Regex.Regex, not: sig.Regex.Not, capture: sig.Regex.Capture, entropy: sig.Regex.Entropy}
		if sig.Regex.Validate != "" {
			cs.regex.validate = validators[sig.Regex.Validate]
			if cs.regex.validate == nil {
				return nil, fmt.Errorf("signature %s: unknown validate %q", sig.ID, sig.Regex.Validate)
			}
		}
	case signatures.KindBlock:
		prog, err := compileBlock(sig.Block)
		if err != nil {
			return nil, fmt.Errorf("signature %s: %w", sig.ID, err)
		}
		cs.block = prog
	case signatures.KindRun:
		cs.run = compileRun(sig.Run)
	default:
		if err := compileMeasure(cs, sig); err != nil {
			return nil, err
		}
	}
	return cs, nil
}

// everyLineRegex reports whether re matches every line: the empty
// regex, which the simple form of a block uses for "every line joins".
// The walk then skips running it.
func everyLineRegex(re *regexp.Regexp) bool {
	return re == nil || re.String() == ""
}

// workerCount is the number of walking goroutines for opts.
func workerCount(opts Options) int {
	if opts.Workers > 0 {
		return opts.Workers
	}
	return runtime.NumCPU()
}

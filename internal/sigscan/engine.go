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
}

// redactor is a redacting signature as an excerpt is masked by it: its
// anchor, and which group of a match is the value.
type redactor struct {
	re      *regexp.Regexp
	capture int
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
		switch cs.kind {
		case signatures.KindBlock:
			e.blocks = append(e.blocks, cs.idx)
		case signatures.KindRun:
			e.runs = append(e.runs, cs.idx)
			e.hasSame = e.hasSame || cs.run.mode == runSame
		}
		if sig.Anchor() != nil && sig.LinePass() {
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

// compileRedactors keeps every redacting signature with an anchor, the
// selected ones first, each id once, and builds their keyword gate.
func (e *engine) compileRedactors(selected, given []*signatures.Signature) error {
	seen := map[string]bool{}
	var sigs []*signatures.Signature
	for _, sig := range slices.Concat(selected, given) {
		if !sig.Redact || sig.Anchor() == nil || seen[sig.ID] {
			continue
		}
		seen[sig.ID] = true
		sigs = append(sigs, sig)
		capture := 0
		if sig.Regex != nil {
			capture = sig.Regex.Capture
		}
		e.redactors = append(e.redactors, redactor{re: sig.Anchor(), capture: capture})
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
			x.values = append(x.values, redactMatches(r.re, r.capture, view)...)
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
		return nil, fmt.Errorf("signature %s: the measure %s cannot be scanned by this build", sig.ID, measureName(sig))
	}
	return cs, nil
}

// measureName is the name of a measure signature's measure.
func measureName(sig *signatures.Signature) string {
	if sig.Measure == nil {
		return "(none)"
	}
	return sig.Measure.Name
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

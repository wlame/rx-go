package sigscan

import (
	"fmt"
	"regexp"
	"runtime"
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
	return e, nil
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

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
	// sigs are the selected signatures, then the body recorders
	// (bodyRecorders): sigs[:selected] are the ones the result answers.
	sigs     []*compiledSig
	selected int
	gate     *gate
	// lineBytes is the line view; cap is the findings cap of a selected
	// signature, bodyCap that of a body recorder (capOf).
	lineBytes int
	cap       int
	bodyCap   int
	// lineAnchored are the signatures with an anchor but no keyword
	// clause: their anchor is tested on every line.
	lineAnchored []int32
	blocks, runs []int32
	// valueSigs are the value measures (entropy, base64-run): tested on
	// every line, one finding per line.
	valueSigs []int32
	// longLines are the long-line measures.
	longLines []int32
	// backs, gaps and densities are the time-backwards, time-gap and
	// density measures: they look at the lines' timestamps only.
	backs, gaps, densities []int32
	// hasSame is true when a `same` run is selected: the walk then
	// keeps each line's view to compare the next line with.
	hasSame bool
	// full[i] is set by the merge once signature i has Cap findings
	// stored: a worker then only counts that signature's findings.
	full []atomic.Bool
	// redact holds the redacting set of the scan (Options.Redactors) and
	// its keyword gate: every excerpt is masked by all of them. It is
	// empty when the scan masks nothing.
	redact redactorSet
	// gapLimit is how many candidates a time-gap keeper holds after it
	// compacts: max(Cap, maxGapCandidates), which compile sets, or a
	// test's tuning.
	gapLimit int
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
	idx  int32
	sig  *signatures.Signature
	kind signatures.Kind
	// redact is the signature's `redact: true`: the anchor's first
	// match of a redacting block or run is then the finding's value,
	// whether or not the scan masks.
	redact bool
	// masks is true when the signature redacts and is in the scan's
	// redacting set (Options.Redactors): each of its findings is then
	// marked masked.
	masks bool
	// bodyOnly marks a body recorder: a copy of a redacting block or run
	// that the walk runs only for where its bodies lie. It builds no
	// excerpt, and the result leaves it out; its findings give
	// Result.MaskedRanges (merger.bodyRanges).
	bodyOnly bool
	regex    *regexMatcher
	block    *blockProgram
	run      *runProgram
	// value is the matcher of a value measure (entropy, base64-run).
	value valueMatcher
	// minShare is the unprefixed measure's min_share: its runs count
	// only when stamped lines are at least this share of the file.
	minShare float64
	// long is a long-line measure's parameters.
	long *longLineSpec
	// back, gap and density are the time measures' parameters.
	back    *backSpec
	gap     *gapSpec
	density *densitySpec
}

// compile builds the engine of a selection. A signature id given twice
// is scanned once. Each measure is compiled to the machinery it runs on
// (measures.go). After the selected signatures come the body recorders
// of every redacting block and run of opts.Redactors (bodyRecorders);
// an empty selection has none, since it reads nothing.
//
// opts.Redactors is the whole redacting set: a selected signature with
// `redact: true` masks only when it is among them, so a scan given no
// redactors masks nothing.
func compile(sigs []*signatures.Signature, opts Options) (*engine, error) {
	e := &engine{lineBytes: opts.LineBytes, cap: opts.Cap, bodyCap: MaxBodiesPerSignature}
	if e.lineBytes <= 0 {
		e.lineBytes = DefaultLineBytes
	}
	if e.cap <= 0 {
		e.cap = DefaultCap
	}
	// A keeper with a limit of 0 would drop every candidate it is
	// given, so the limit always starts at its default; a scan's tuning
	// may set another one afterwards.
	e.gapLimit = max(e.cap, maxGapCandidates)
	unique := uniqueByID(sigs)
	e.selected = len(unique)
	all := unique
	if len(unique) > 0 {
		all = slices.Concat(unique, bodyRecorders(opts.Redactors))
	}
	masking := redactingIDs(opts.Redactors)
	for i, sig := range all {
		cs, err := compileSig(int32(i), sig) // #nosec G115 -- at most the loaded signatures, twice
		if err != nil {
			return nil, err
		}
		cs.masks = cs.redact && masking[sig.ID]
		if i >= e.selected {
			cs.recordBodies()
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
		case cs.back != nil:
			e.backs = append(e.backs, cs.idx)
		case cs.gap != nil:
			e.gaps = append(e.gaps, cs.idx)
		case cs.density != nil:
			e.densities = append(e.densities, cs.idx)
		}
		// A json-block has no anchor and no keywords: its opener is
		// tried on every line, as an anchor without keywords is.
		if (sig.Anchor() != nil && sig.LinePass()) || (cs.block != nil && cs.block.json) {
			e.lineAnchored = append(e.lineAnchored, cs.idx)
		}
	}
	// The body recorders are gated by their keyword clauses as any
	// signature is, in the one automaton of the scan.
	g, err := newGate(all)
	if err != nil {
		return nil, err
	}
	e.gate = g
	e.full = make([]atomic.Bool, len(e.sigs))
	redact, err := compileRedactors(opts.Redactors)
	if err != nil {
		return nil, err
	}
	e.redact = redact
	return e, nil
}

// uniqueByID returns sigs with each id once, in first-seen order.
func uniqueByID(sigs []*signatures.Signature) []*signatures.Signature {
	seen := map[string]bool{}
	var unique []*signatures.Signature
	for _, sig := range sigs {
		if !seen[sig.ID] {
			seen[sig.ID] = true
			unique = append(unique, sig)
		}
	}
	return unique
}

// recordsBodies reports whether sig is a redacting block or `match` run:
// a signature whose body lines (after its anchor line) every output
// replaces by their length. A redacting run is always a `match` run
// (the loader refuses redact on blank and same runs).
func recordsBodies(sig *signatures.Signature) bool {
	return sig.Redact && (sig.Kind == signatures.KindBlock || sig.Kind == signatures.KindRun) && sig.Anchor() != nil
}

// redactingIDs is the set of ids of the signatures of redactors that
// redact: the signatures whose own findings a scan marks masked.
func redactingIDs(redactors []*signatures.Signature) map[string]bool {
	ids := make(map[string]bool, len(redactors))
	for _, sig := range redactors {
		if sig.Redact {
			ids[sig.ID] = true
		}
	}
	return ids
}

// bodyRecorders returns the redacting blocks and runs of the given
// redactors, each id once: the signatures the scan walks a second time,
// as body recorders, for where their bodies lie. One that is selected
// too is walked twice because its findings keep the findings cap and
// its own min_lines, while its bodies are recorded far past the cap and
// for every block it opened.
func bodyRecorders(given []*signatures.Signature) []*signatures.Signature {
	var out []*signatures.Signature
	for _, sig := range uniqueByID(given) {
		if recordsBodies(sig) {
			out = append(out, sig)
		}
	}
	return out
}

// recordBodies turns a compiled redacting block or run into its body
// recorder. A block's copy has min_lines 1: min_lines decides only
// which closed blocks are findings, never what a block joins, so the
// copy walks exactly as the signature does and reports every block it
// opened, one shorter than min_lines too: such a block still holds the
// lines it joined, key material included. A run keeps its min: every
// line of a run matches the anchor, so a run too short to be a finding
// already has its values masked.
func (cs *compiledSig) recordBodies() {
	cs.bodyOnly = true
	if cs.block != nil {
		prog := *cs.block
		prog.minLines = 1
		cs.block = &prog
	}
}

// capOf is the most findings signature si stores: the findings cap, or
// for a body recorder the bound on its bodies.
func (e *engine) capOf(si int32) int {
	if e.sigs[si].bodyOnly {
		return e.bodyCap
	}
	return e.cap
}

// redactorSet is a set of redacting signatures as text is masked by
// them: each one's redactor, and their keyword gate (nil when the set
// is empty). It is built once and only read afterwards; the per-line
// gate counters live in a gateScratch of each goroutine.
type redactorSet struct {
	redactors []redactor
	gate      *gate
}

// compileRedactors keeps every redacting signature of given with
// something to mask, an anchor or a value measure's values, each id
// once, and builds their keyword gate (a value measure has no keywords,
// so it masks every line it finds a value on). No signature given is an
// empty set, which masks nothing.
func compileRedactors(given []*signatures.Signature) (redactorSet, error) {
	var set redactorSet
	seen := map[string]bool{}
	var sigs []*signatures.Signature
	for _, sig := range given {
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
		set.redactors = append(set.redactors, r)
	}
	if len(sigs) == 0 {
		return set, nil
	}
	g, err := newGate(sigs)
	if err != nil {
		return redactorSet{}, err
	}
	set.gate = g
	return set, nil
}

// newScratch returns gate counters for one goroutine, nil for an empty
// set.
func (s redactorSet) newScratch() *gateScratch {
	if s.gate == nil {
		return nil
	}
	return s.gate.newScratch()
}

// appendLineValues appends to dst the values every redactor masks on
// view, in the order the redactors come: a redactor runs only on a line
// that passes its keyword gate. It stops adding once dst holds more
// than maxRedactMatches values, since the line is then masked whole.
// gs is the caller's gate counters (newScratch).
func (s redactorSet) appendLineValues(dst []span, view []byte, gs *gateScratch) []span {
	if s.gate == nil {
		return dst
	}
	s.gate.evaluate(view, gs)
	for i, r := range s.redactors {
		if len(dst) > maxRedactMatches {
			break // already more than can be followed: the line is masked whole
		}
		if s.gate.passes(int32(i), gs) { // #nosec G115 -- at most the loaded signatures
			dst = r.appendValues(dst, view)
		}
	}
	return dst
}

// excerpter builds the excerpts of one goroutine, which owns its gate
// counters for the redactors.
type excerpter struct {
	e  *engine
	gs *gateScratch
	// values and masked are reused from excerpt to excerpt: the line's
	// values to mask, and the masked text before it is cut.
	values []span
	masked []byte
}

func newExcerpter(e *engine) *excerpter {
	return &excerpter{e: e, gs: e.redact.newScratch()}
}

// build appends the excerpt of a line view to dst with every value a
// redactor matches on the line masked (redaction), and reports whether
// a mask is in the excerpt. A redactor runs only on a line that passes
// its keyword gate; the values of all of them are masked together, so
// the bounds and the fail-closed rules apply to their union.
func (x *excerpter) build(dst, view []byte) ([]byte, bool) {
	x.values = x.e.redact.appendLineValues(x.values[:0], view, x.gs)
	if len(x.values) == 0 {
		return appendExcerpt(dst, view), false
	}
	slices.SortStableFunc(x.values, func(a, b span) int { return cmp.Compare(a[0], b[0]) })
	var masked bool
	dst, x.masked, masked = appendRedactedExcerpt(dst, x.masked, view, x.values)
	return dst, masked
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

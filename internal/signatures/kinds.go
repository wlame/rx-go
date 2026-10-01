package signatures

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// validators are the values `validate` accepts.
var validators = []string{ValidateLuhn, ValidateIBAN, ValidateIPv4, ValidateIPv6}

// everyLine is the rule regex of a simple block with `end` and no
// `continue`: every line joins until the end line. The empty regex
// matches every line.
var everyLine = regexp.MustCompile(``)

// stateName is the form of a state name in a block's states form.
var stateName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// simpleBlockForm keeps a block written in the simple form as it was
// written, for DefinitionYAML: the compiled states say the same thing
// less readably.
type simpleBlockForm struct {
	start, cont, end *regexp.Regexp
}

// readRegexKind reads a `regex` signature: the anchor, then the
// regex-kind fields written at the top of the signature.
func (p *sigParser) readRegexKind(f pair) {
	spec := &RegexSpec{}
	p.hasAnchor = true
	spec.Regex = p.compileRegex(f)
	if spec.Regex != nil {
		p.anchor = spec.Regex.String()
	}
	for _, extra := range p.regexKindFields {
		switch extra.key {
		case "not":
			spec.Not = p.compileRegex(extra)
		case "capture":
			v, err := intValue(extra.value, "capture", 0, math.MaxInt32)
			p.fail(err)
			if err == nil && spec.Regex != nil && v > spec.Regex.NumSubexp() {
				p.fail(newFieldError(extra.value, "capture: the regex has %d groups, so there is no group %d", spec.Regex.NumSubexp(), v))
			}
			spec.Capture = v
		case "entropy":
			v, err := floatValue(extra.value, "entropy", 0, 8)
			p.fail(err)
			spec.Entropy = &v
		case "validate":
			v := p.text(extra, nonEmptyText)
			if v != "" && !contains(validators, v) {
				p.fail(newFieldError(extra.value, "validate %q is not one of %s", v, strings.Join(validators, ", ")))
			}
			spec.Validate = v
		}
	}
	p.sig.Regex = spec
}

// blockFields are the fields of a `block:` mapping.
var blockFields = []string{"start", "continue", "end", "lines", "states", "min_lines", "max_lines", "not"}

// readBlock reads a `block` signature in the simple form (start with
// continue and/or end, or with lines) or the states form.
func (p *sigParser) readBlock(f pair) {
	fields, ok := p.subFields(f, blockFields)
	if !ok {
		return
	}
	spec := &BlockSpec{MinLines: 1, MaxLines: 200}
	p.sig.Block = spec
	p.hasAnchor = true
	if v, given := fields["max_lines"]; given {
		spec.MaxLines = p.maxLines(v)
	}
	if v, given := fields["min_lines"]; given {
		n, err := intValue(v.value, "min_lines", 1, maxBlockLines)
		p.fail(err)
		if err == nil && n > spec.MaxLines {
			p.fail(newFieldError(v.value, "min_lines: %d is more than max_lines (%d)", n, spec.MaxLines))
		}
		spec.MinLines = n
	}
	if v, given := fields["not"]; given {
		spec.Not = p.compileRegex(v)
	}
	states, hasStates := fields["states"]
	simple := hasAny(fields, "start", "continue", "end", "lines")
	switch {
	case hasStates && simple:
		p.fail(newFieldError(states.keyAt, "states and the simple form (start, continue, end, lines) do not mix: give one"))
	case hasStates:
		p.readStates(states, spec)
	default:
		p.readSimpleBlock(f, fields, spec)
	}
}

// maxLines reads `max_lines`: 1 to 10,000. A larger value is a limit,
// which refuses the whole file.
func (p *sigParser) maxLines(v pair) int {
	n, err := intValue(v.value, "max_lines", 1, math.MaxInt32)
	if err != nil {
		p.fail(err)
		return 200
	}
	if n > maxBlockLines {
		p.fail(&fieldError{line: v.value.Line, wholeFile: true,
			msg: fmt.Sprintf("max_lines: %d is over the limit of %d", n, maxBlockLines)})
	}
	return n
}

// readSimpleBlock reads the simple form and compiles it to states (see
// StateStart for the rules it makes).
func (p *sigParser) readSimpleBlock(f pair, fields map[string]pair, spec *BlockSpec) {
	startField, hasStart := fields["start"]
	contField, hasCont := fields["continue"]
	endField, hasEnd := fields["end"]
	linesField, hasLines := fields["lines"]
	if !hasStart {
		p.fail(newFieldError(f.value, "block needs start (with continue, end or lines) or states"))
		return
	}
	start := p.compileRegex(startField)
	if start != nil {
		p.anchor = start.String()
	}
	spec.Start = start
	spec.States = []StateRule{}
	switch {
	case hasLines && (hasCont || hasEnd):
		p.fail(newFieldError(linesField.keyAt, "lines takes no continue or end: the block is the start line and the next lines lines"))
	case hasLines:
		n, err := intValue(linesField.value, "lines", 1, maxBlockLines)
		p.fail(err)
		if err == nil && n+1 > spec.MaxLines {
			p.fail(newFieldError(linesField.value, "lines: %d needs max_lines of at least %d (the start line counts)", n, n+1))
		}
		spec.Lines = n
		return
	case !hasCont && !hasEnd:
		p.fail(newFieldError(f.value, "a block with start alone never ends: add continue, end or lines"))
		return
	}
	form := &simpleBlockForm{start: start}
	spec.States = append(spec.States, StateRule{State: StateStart, Regex: start, Next: StateBody})
	if hasEnd {
		form.end = p.compileRegex(endField)
		spec.States = append(spec.States, StateRule{State: StateBody, Regex: form.end, End: true})
	}
	if hasCont {
		form.cont = p.compileRegex(contField)
		spec.States = append(spec.States, StateRule{State: StateBody, Regex: form.cont, Next: StateBody})
	} else {
		spec.States = append(spec.States, StateRule{State: StateBody, Regex: everyLine, Next: StateBody})
	}
	p.sig.simpleBlock = form
}

// stateRuleFields are the fields of one rule of `states:`.
var stateRuleFields = []string{"state", "regex", "next", "end"}

// readStates reads the states form: rules {state, regex, next, end}.
// The first rule's state is "start"; a rule without next and without
// end stays in its state; every next names a state that has rules.
func (p *sigParser) readStates(f pair, spec *BlockSpec) {
	items, err := sequenceItems(f.value, "states")
	if err == nil && len(items) == 0 {
		err = newFieldError(f.value, "states is an empty list")
	}
	if err != nil {
		p.fail(err)
		return
	}
	rules := make([]StateRule, 0, len(items))
	lines := make([]*yaml.Node, 0, len(items))
	var startSources []string
	for i, item := range items {
		rule, ok := p.readStateRule(item)
		if !ok {
			return
		}
		if i == 0 && rule.State != StateStart {
			p.fail(newFieldError(item, "the first rule's state must be start, not %q", rule.State))
			return
		}
		if rule.State == StateStart && rule.Regex != nil {
			startSources = append(startSources, rule.Regex.String())
		}
		rules = append(rules, rule)
		lines = append(lines, item)
	}
	known := map[string]bool{}
	for _, r := range rules {
		known[r.State] = true
	}
	for i, r := range rules {
		if !r.End && !known[r.Next] {
			p.fail(newFieldError(lines[i], "next: no rule has the state %q", r.Next))
		}
	}
	spec.States = rules
	spec.Start = p.joinStarts(f, rules, startSources)
	if spec.Start != nil {
		p.anchor = spec.Start.String()
	}
}

// readStateRule reads one rule of `states:`.
func (p *sigParser) readStateRule(item *yaml.Node) (StateRule, bool) {
	var rule StateRule
	fields, ok := p.subFields(pair{key: "a state rule", keyAt: item, value: item}, stateRuleFields)
	if !ok {
		return rule, false
	}
	state, hasState := fields["state"]
	regexField, hasRegex := fields["regex"]
	if !hasState || !hasRegex {
		p.fail(newFieldError(item, "a state rule needs state and regex"))
		return rule, false
	}
	rule.State = p.text(state, nonEmptyText)
	if rule.State != "" && !stateName.MatchString(rule.State) {
		p.fail(newFieldError(state.value, "state %q is not a name (a lower-case letter, then letters, digits, - or _)", rule.State))
	}
	rule.Regex = p.compileRegex(regexField)
	if v, given := fields["end"]; given {
		rule.End = p.flag(v)
	}
	next, hasNext := fields["next"]
	switch {
	case hasNext && rule.End:
		p.fail(newFieldError(next.keyAt, "a rule with end: true closes the block, so it has no next"))
	case hasNext:
		rule.Next = p.text(next, nonEmptyText)
	case !rule.End:
		rule.Next = rule.State
	}
	return rule, true
}

// joinStarts returns the anchor of a states block: the one start rule's
// regex, or the start rules' regexes joined as (?:a)|(?:b).
func (p *sigParser) joinStarts(f pair, rules []StateRule, sources []string) *regexp.Regexp {
	if len(sources) == 1 {
		for _, r := range rules {
			if r.State == StateStart {
				return r.Regex
			}
		}
	}
	if len(sources) == 0 {
		return nil
	}
	parts := make([]string, len(sources))
	for i, s := range sources {
		parts[i] = "(?:" + s + ")"
	}
	re, err := regexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		p.fail(newFieldError(f.value, "states: the start rules do not join into one regex: %s", err.Error()))
		return nil
	}
	return re
}

// runFields are the fields of a `run:` mapping.
var runFields = []string{"match", "blank", "same", "min", "not"}

// readRun reads a `run` signature: exactly one of match, blank: true or
// same: true, and min.
func (p *sigParser) readRun(f pair) {
	fields, ok := p.subFields(f, runFields)
	if !ok {
		return
	}
	spec := &RunSpec{}
	p.sig.Run = spec
	if v, given := fields["blank"]; given {
		spec.Blank = p.flag(v)
	}
	if v, given := fields["same"]; given {
		spec.Same = p.flag(v)
	}
	match, hasMatch := fields["match"]
	if count(hasMatch, spec.Blank, spec.Same) != 1 {
		p.fail(newFieldError(f.value, "a run needs exactly one of match, blank: true or same: true"))
	}
	if hasMatch {
		p.hasAnchor = true
		spec.Match = p.compileRegex(match)
		if spec.Match != nil {
			p.anchor = spec.Match.String()
		}
	}
	if v, given := fields["not"]; given {
		if !hasMatch {
			p.fail(newFieldError(v.keyAt, "not needs match: a blank or same run has no line to test it on"))
		}
		spec.Not = p.compileRegex(v)
	}
	minField, hasMin := fields["min"]
	if !hasMin {
		p.fail(newFieldError(f.value, "min is missing: a run is at least min lines long (2 to 1000000)"))
		return
	}
	n, err := intValue(minField.value, "min", 2, 1000000)
	p.fail(err)
	spec.Min = n
}

// readMeasure reads a `measure` signature: a known measure name and its
// parameters, checked against the measure table.
func (p *sigParser) readMeasure(f pair) {
	pairs, err := mappingPairs(f.value, "measure")
	if err != nil {
		p.fail(err)
		return
	}
	var name string
	for _, mp := range pairs {
		if mp.key == "name" {
			name = p.text(mp, nonEmptyText)
		}
	}
	if name == "" {
		p.fail(newFieldError(f.value, "measure needs a name (one of %s)", strings.Join(MeasureNames(), ", ")))
		return
	}
	def, known := measureDefs[name]
	if !known {
		p.fail(newFieldError(f.value, "unknown measure %q (one of %s)", name, strings.Join(MeasureNames(), ", ")))
		return
	}
	values := paramValues{}
	for _, spec := range def.params {
		values[spec.name] = spec.def
	}
	for _, mp := range pairs {
		if mp.key == "name" {
			continue
		}
		spec, found := findParam(def.params, mp.key)
		if !found {
			p.fail(newFieldError(mp.keyAt, "unknown parameter %q of %s (it takes %s)", mp.key, name, paramNames(def.params)))
			continue
		}
		if v, ok := p.paramValue(spec, mp.value); ok {
			values[spec.name] = v
		}
	}
	p.sig.Measure = &MeasureSpec{Name: name, Params: def.build(values)}
}

// paramValue reads one measure parameter as the table declares it: a
// whole number, a number, a duration with a unit, or a bucket (a
// duration or "auto"). Durations are returned in seconds.
func (p *sigParser) paramValue(spec paramSpec, n *yaml.Node) (float64, bool) {
	switch spec.kind {
	case paramInt:
		v, err := intValue(n, spec.name, int(spec.min), int(spec.max))
		p.fail(err)
		return float64(v), err == nil
	case paramFloat:
		v, err := floatValue(n, spec.name, spec.min, spec.max)
		p.fail(err)
		return v, err == nil
	}
	text, err := textValue(n, spec.name)
	if r := resolve(n); err == nil && r.ShortTag() != "!!str" {
		err = newFieldError(n, "%s: %s needs a unit, such as 60s", spec.name, r.Value)
	}
	if err != nil {
		p.fail(err)
		return 0, false
	}
	if spec.kind == paramBucket && text == "auto" {
		return 0, true
	}
	d, perr := parseDuration(text)
	if perr != nil {
		p.fail(newFieldError(n, "%s: %s", spec.name, perr.Error()))
		return 0, false
	}
	if d%time.Millisecond != 0 {
		// Timestamps are read to the millisecond, so a finer duration
		// would be cut silently.
		p.fail(newFieldError(n, "%s: %s is not a whole number of milliseconds", spec.name, text))
		return 0, false
	}
	seconds := d.Seconds()
	if seconds < spec.min || seconds > spec.max {
		p.fail(newFieldError(n, "%s: %s is out of range %s to %s", spec.name, text,
			formatDuration(secondsDuration(spec.min)), formatDuration(secondsDuration(spec.max))))
		return 0, false
	}
	return seconds, true
}

// secondsDuration turns a number of seconds from the measure table into
// a duration.
func secondsDuration(s float64) time.Duration {
	return time.Duration(math.Round(s * float64(time.Second)))
}

// findParam returns the parameter of a measure with the given name.
func findParam(params []paramSpec, name string) (paramSpec, bool) {
	for _, p := range params {
		if p.name == name {
			return p, true
		}
	}
	return paramSpec{}, false
}

// paramNames lists a measure's parameter names for an error message.
func paramNames(params []paramSpec) string {
	if len(params) == 0 {
		return "no parameters"
	}
	names := make([]string, len(params))
	for i, p := range params {
		names[i] = p.name
	}
	return strings.Join(names, ", ")
}

// subFields reads a mapping whose keys must come from allowed, keyed by
// name. An unknown key is recorded and the rest still read.
func (p *sigParser) subFields(f pair, allowed []string) (map[string]pair, bool) {
	pairs, err := mappingPairs(f.value, f.key)
	if err != nil {
		p.fail(err)
		return nil, false
	}
	out := make(map[string]pair, len(pairs))
	for _, sub := range pairs {
		if !contains(allowed, sub.key) {
			p.fail(newFieldError(sub.keyAt, "unknown field %q of %s (it has %s)", sub.key, f.key, strings.Join(allowed, ", ")))
			continue
		}
		out[sub.key] = sub
	}
	return out, true
}

// hasAny reports whether any of the keys is in fields.
func hasAny(fields map[string]pair, keys ...string) bool {
	for _, k := range keys {
		if _, ok := fields[k]; ok {
			return true
		}
	}
	return false
}

// count returns how many of the flags are true.
func count(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

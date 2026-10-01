package signatures

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The limits of a signature file. Each refuses the whole file it is
// found in (the per-file limits) or the file that passes it (the user
// signature count).
const (
	maxFileBytes      = 1 << 20
	maxUserSignatures = 1000
	maxRegexBytes     = 4096
	maxAliasNodes     = 10000
	maxBlockLines     = 10000
	maxIDBytes        = 64
)

// kebabCase is the form of an id, a group name and a tag.
var kebabCase = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// reservedWords are the selection words that cannot be an id, a group
// or a tag: a selection reads them as every signature and the default
// set.
var reservedWords = []string{"all", "default"}

// fileDef is what one signature file defines, before the catalogue is
// assembled from every file.
type fileDef struct {
	name    string
	builtin bool
	groups  []groupDecl
	disable []idRef
	entries []entry
	// errs are problems of a group or disable entry: that entry is
	// skipped and the rest of the file loads.
	errs []LoadError
}

// groupDecl is one entry of a file's `groups:` list.
type groupDecl struct {
	name, description string
	line              int
}

// idRef is one id of a file's `disable:` list and its line.
type idRef struct {
	id   string
	line int
}

// entry is one item of a file's `signatures:` list: the signature it
// defines, or the errors that refused it.
type entry struct {
	sig       *Signature // nil when refused
	id        string
	line      int
	groupLine int
	errs      []LoadError
}

// topLevelKeys are the keys a signature file may hold, each with the
// function that reads its value. Any other key refuses the file.
var topLevelKeys = map[string]func(fd *fileDef, value *yaml.Node) *fieldError{
	"signatures": parseSignatures,
	"disable":    parseDisable,
	"groups":     parseGroups,
}

// wholeFileSuffix ends the message of every problem that refuses a
// whole file.
const wholeFileSuffix = "; the whole file is refused"

// parseFile reads one signature file. It returns the file's
// definitions, or the one error that refuses the whole file: a YAML
// syntax error, a document of several parts, too much alias expansion,
// a top level that is not a mapping, an unknown top-level key, or a
// limit.
//
// Error flow: a problem of one signature becomes LoadErrors on its
// entry, and the walk goes on with the next signature; a fieldError
// marked wholeFile travels up to here and replaces the whole result.
func parseFile(name string, data []byte, builtin bool) (*fileDef, *LoadError) {
	fd := &fileDef{name: name, builtin: builtin}
	refuse := func(line int, msg string) (*fileDef, *LoadError) {
		return nil, &LoadError{File: name, Line: line, Message: msg + wholeFileSuffix}
	}
	refuseAt := func(err *fieldError) (*fileDef, *LoadError) {
		return nil, &LoadError{File: name, Line: err.line, ID: err.id, Message: err.msg + wholeFileSuffix}
	}

	// Go note: a yaml.Decoder reads one document per Decode call and
	// returns io.EOF when there is none left, so the second call tells
	// whether the file holds more than one.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return fd, nil
		}
		line, msg := yamlSyntaxError(err)
		return refuse(line, "YAML syntax error: "+msg)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			line, msg := yamlSyntaxError(err)
			return refuse(line, "YAML syntax error: "+msg)
		}
		return refuse(extra.Line, "the file holds more than one YAML document; a signature file is one YAML document")
	}
	if !aliasExpansionFits(&doc) {
		return refuse(0, fmt.Sprintf("alias expansion passes the limit of %d nodes", maxAliasNodes))
	}
	if len(doc.Content) == 0 || isNull(doc.Content[0]) {
		return fd, nil
	}
	top := doc.Content[0]
	if resolve(top).Kind != yaml.MappingNode {
		return refuse(top.Line, "the top level must be a mapping of signatures, disable and groups")
	}
	pairs, ferr := mappingPairs(top, "the top level")
	if ferr != nil {
		return refuse(ferr.line, ferr.msg)
	}
	for _, p := range pairs {
		read, ok := topLevelKeys[p.key]
		if !ok {
			return refuse(p.keyAt.Line, fmt.Sprintf("unknown top-level key %q: a signature file holds signatures, disable and groups", p.key))
		}
		if err := read(fd, p.value); err != nil {
			return refuseAt(err)
		}
	}
	if builtin && len(fd.disable) > 0 {
		return refuse(fd.disable[0].line, "a built-in file cannot disable signatures")
	}
	return fd, nil
}

// yamlLineText matches the line yaml.v3 puts in a syntax error.
var yamlLineText = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

// yamlSyntaxError splits a yaml.v3 error into its line and message.
// yaml.v3 gives the line only inside the text ("yaml: line 3: …").
func yamlSyntaxError(err error) (int, string) {
	text := err.Error()
	if m := yamlLineText.FindStringSubmatch(text); m != nil {
		line, _ := strconv.Atoi(m[1])
		return line, m[2]
	}
	return 0, strings.TrimPrefix(text, "yaml: ")
}

// parseSignatures reads the `signatures:` list. A problem of one item
// refuses that item only; a limit refuses the file.
func parseSignatures(fd *fileDef, value *yaml.Node) *fieldError {
	if isNull(value) {
		return nil
	}
	items, err := sequenceItems(value, "signatures")
	if err != nil {
		return err
	}
	for _, item := range items {
		e, fatal := parseSignature(fd.name, item, fd.builtin)
		if fatal != nil {
			return fatal
		}
		fd.entries = append(fd.entries, e)
	}
	return nil
}

// parseDisable reads the `disable:` list of built-in ids.
func parseDisable(fd *fileDef, value *yaml.Node) *fieldError {
	if isNull(value) {
		return nil
	}
	items, err := sequenceItems(value, "disable")
	if err != nil {
		return err
	}
	for _, item := range items {
		id, err := textValue(item, "disable")
		if err != nil {
			fd.errs = append(fd.errs, LoadError{File: fd.name, Line: err.line, Message: err.msg})
			continue
		}
		fd.disable = append(fd.disable, idRef{id: id, line: item.Line})
	}
	return nil
}

// parseGroups reads the `groups:` list of group declarations. A bad
// entry is skipped with an error; the rest of the file loads.
func parseGroups(fd *fileDef, value *yaml.Node) *fieldError {
	if isNull(value) {
		return nil
	}
	items, err := sequenceItems(value, "groups")
	if err != nil {
		return err
	}
	for _, item := range items {
		decl, err := parseGroup(item)
		if err != nil {
			fd.errs = append(fd.errs, LoadError{File: fd.name, Line: err.line, Message: err.msg})
			continue
		}
		fd.groups = append(fd.groups, decl)
	}
	return nil
}

// parseGroup reads one group declaration: a kebab-case name and a
// description.
func parseGroup(n *yaml.Node) (groupDecl, *fieldError) {
	decl := groupDecl{line: n.Line}
	pairs, err := mappingPairs(n, "a group")
	if err != nil {
		return decl, err
	}
	for _, p := range pairs {
		switch p.key {
		case "name":
			decl.name, err = nonEmptyText(p.value, "group name")
		case "description":
			decl.description, err = nonEmptyText(p.value, "group description")
		default:
			err = newFieldError(p.keyAt, "unknown field %q of a group: a group has name and description", p.key)
		}
		if err != nil {
			return decl, err
		}
	}
	switch {
	case decl.name == "":
		return decl, newFieldError(n, "a group needs a name")
	case decl.description == "":
		return decl, newFieldError(n, "group %s needs a description", decl.name)
	}
	if problem := nameProblem(decl.name); problem != "" {
		return decl, newFieldError(n, "group name %q %s", decl.name, problem)
	}
	return decl, nil
}

// nameProblem says what is wrong with an id, a group name or a tag, or
// returns "" when it is fine.
func nameProblem(name string) string {
	switch {
	case len(name) > maxIDBytes:
		return fmt.Sprintf("is longer than %d bytes", maxIDBytes)
	case !kebabCase.MatchString(name):
		return "is not kebab-case (lower-case letters and digits joined by single dashes)"
	case slices.Contains(reservedWords, name):
		return "is reserved: all and default are selection words"
	}
	return ""
}

// sigParser gathers one signature while its fields are read. Every
// problem is kept, so one run of `rx signatures check` reports all of a
// signature's mistakes.
type sigParser struct {
	file    string
	builtin bool
	node    *yaml.Node
	sig     *Signature
	errs    []*fieldError
	fatal   *fieldError
	// kinds are the kind keys present (regex, block, run, measure).
	kinds []pair
	// regexKindFields are the fields of the regex kind given at the top
	// of the signature (not, capture, entropy, validate).
	regexKindFields []pair
	keywords        *pair
	examples        *pair
	// redactAt is where `redact: true` was given, for a refusal that
	// names its line; nil when the signature does not redact.
	redactAt  *yaml.Node
	groupLine int
	given     map[string]bool
	// anchor is the source of the compiled anchor regex, for the
	// keyword walk; hasAnchor is true for a kind that has one (regex,
	// block, run with match), even when it failed to compile.
	anchor    string
	hasAnchor bool
}

// fail records a problem of the signature; a wholeFile one is kept
// apart, since it refuses the file.
func (p *sigParser) fail(err *fieldError) {
	if err == nil {
		return
	}
	if err.wholeFile {
		if p.fatal == nil {
			p.fatal = err
		}
		return
	}
	p.errs = append(p.errs, err)
}

// signatureFields are the keys a signature mapping may hold, each with
// the function that reads its value. A key not in the table refuses the
// signature.
var signatureFields = map[string]func(p *sigParser, field pair){
	"id":          func(*sigParser, pair) {}, // read first, by parseSignature
	"name":        func(p *sigParser, f pair) { p.sig.Name = p.text(f, nonEmptyText) },
	"group":       readGroupField,
	"description": func(p *sigParser, f pair) { p.sig.Description = p.text(f, nonEmptyText) },
	"tags":        readTags,
	"level":       readLevel,
	"links":       readLinks,
	"metadata":    readMetadata,
	"default":     func(p *sigParser, f pair) { p.sig.Default = p.flag(f) },
	"redact":      readRedact,
	"keywords":    func(p *sigParser, f pair) { p.keywords = &f },
	"examples":    func(p *sigParser, f pair) { p.examples = &f },
	"regex":       addKind,
	"block":       addKind,
	"run":         addKind,
	"measure":     addKind,
	"not":         addRegexKindField,
	"capture":     addRegexKindField,
	"entropy":     addRegexKindField,
	"validate":    addRegexKindField,
}

// text reads a field with one of the text readers.
func (p *sigParser) text(f pair, read func(*yaml.Node, string) (string, *fieldError)) string {
	v, err := read(f.value, f.key)
	p.fail(err)
	return v
}

// flag reads a true/false field.
func (p *sigParser) flag(f pair) bool {
	v, err := boolValue(f.value, f.key)
	p.fail(err)
	return v
}

func addKind(p *sigParser, f pair)           { p.kinds = append(p.kinds, f) }
func addRegexKindField(p *sigParser, f pair) { p.regexKindFields = append(p.regexKindFields, f) }

// readGroupField reads the group name; whether the group exists is
// checked once every file's groups are known.
func readGroupField(p *sigParser, f pair) {
	p.sig.Group = p.text(f, nonEmptyText)
	p.groupLine = f.value.Line
}

// readTags reads the kebab-case tags.
func readTags(p *sigParser, f pair) {
	tags, err := textList(f.value, "tags")
	if err != nil {
		p.fail(err)
		return
	}
	for _, tag := range tags {
		if problem := nameProblem(tag); problem != "" {
			p.fail(newFieldError(f.value, "tag %q %s", tag, problem))
			return
		}
	}
	p.sig.Tags = tags
}

// readLevel reads one of the five levels.
func readLevel(p *sigParser, f pair) {
	text := p.text(f, nonEmptyText)
	if text == "" {
		return
	}
	if !slices.Contains(Levels(), Level(text)) {
		p.fail(newFieldError(f.value, "level %q is not one of info, low, medium, high, critical", text))
		return
	}
	p.sig.Level = Level(text)
}

// readLinks reads the links: http and https URLs only, since a viewer
// shows them as links.
func readLinks(p *sigParser, f pair) {
	links, err := textList(f.value, "links")
	if err != nil {
		p.fail(err)
		return
	}
	for _, link := range links {
		u, err := url.Parse(link)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.fail(newFieldError(f.value, "link %q is not an http or https URL", link))
			return
		}
	}
	p.sig.Links = links
}

// readMetadata reads the text-to-text metadata map.
func readMetadata(p *sigParser, f pair) {
	m, err := textMap(f.value, "metadata")
	p.fail(err)
	if err == nil {
		p.sig.Metadata = m
	}
}

// parseSignature reads one item of `signatures:`. It returns the entry
// (the signature, or the errors that refused it) and, separately, a
// problem that refuses the whole file (a limit).
func parseSignature(file string, n *yaml.Node, builtin bool) (entry, *fieldError) {
	e := entry{line: n.Line}
	pairs, err := mappingPairs(n, "a signature")
	if err != nil {
		e.id = findID(n)
		e.errs = []LoadError{{File: file, Line: err.line, ID: e.id, Message: err.msg}}
		return e, nil
	}
	p := &sigParser{
		file: file, builtin: builtin, node: n, given: map[string]bool{},
		sig: &Signature{
			Level: LevelMedium, Default: true,
			Tags: []string{}, Links: []string{}, Metadata: map[string]string{},
			Examples: Examples{Match: []string{}, NoMatch: []string{}},
			Source:   Source{Builtin: builtin, File: file, Line: n.Line},
		},
	}
	// The id is read first, so every later error can name it.
	for _, f := range pairs {
		if f.key == "id" {
			p.sig.ID = p.text(f, nonEmptyText)
			if problem := nameProblem(p.sig.ID); p.sig.ID != "" && problem != "" {
				p.fail(newFieldError(f.value, "id %q %s", p.sig.ID, problem))
			}
		}
	}
	for _, f := range pairs {
		p.given[f.key] = true
		read, ok := signatureFields[f.key]
		if !ok {
			p.fail(newFieldError(f.keyAt, "unknown field %q", f.key))
			continue
		}
		read(p, f)
	}
	p.finish()
	if p.fatal != nil {
		p.fatal.id = p.sig.ID
		return e, p.fatal
	}
	e.id, e.groupLine = p.sig.ID, p.groupLine
	if len(p.errs) > 0 {
		for _, fe := range p.errs {
			e.errs = append(e.errs, LoadError{File: file, Line: fe.line, ID: p.sig.ID, Message: fe.msg})
		}
		return e, nil
	}
	e.sig = p.sig
	return e, nil
}

// findID returns the id of a signature mapping that could not be read
// as a whole, when its id can still be found.
func findID(n *yaml.Node) string {
	m := resolve(n)
	if m.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if resolve(m.Content[i]).Value == "id" {
			if v := resolve(m.Content[i+1]); v.Kind == yaml.ScalarNode {
				return v.Value
			}
		}
	}
	return ""
}

// requiredFields are the fields every signature must give.
var requiredFields = []string{"id", "name", "group", "description"}

// finish checks what the fields say together: required fields, exactly
// one kind, the kind's own fields, keywords and examples.
func (p *sigParser) finish() {
	for _, field := range requiredFields {
		if !p.given[field] {
			p.fail(newFieldError(p.node, "%q is missing", field))
		}
	}
	switch len(p.kinds) {
	case 0:
		p.fail(newFieldError(p.node, "no kind: give one of regex, block, run or measure"))
	case 1:
		p.readKind(p.kinds[0])
	default:
		p.fail(newFieldError(p.kinds[1].keyAt, "two kinds (%s and %s): give exactly one of regex, block, run or measure",
			p.kinds[0].key, p.kinds[1].key))
	}
	p.checkRedact()
	p.readKeywords()
	p.readExamples()
}

// readRedact reads `redact` and keeps where it was given.
func readRedact(p *sigParser, f pair) {
	p.sig.Redact = p.flag(f)
	if p.sig.Redact {
		p.redactAt = f.keyAt
	}
}

// redactableMeasures are the measures that find a value on a line, a
// span masked like a regex value: the only measures `redact` applies to.
var redactableMeasures = map[string]bool{"entropy": true, "base64-run": true}

// checkRedact refuses `redact: true` where nothing is a value to mask: a
// blank or same run, which has no anchor, and a measure other than the
// value measures (entropy, base64-run). A finding marked masked with
// nothing masked would mislead.
func (p *sigParser) checkRedact() {
	if p.redactAt == nil {
		return
	}
	switch {
	case p.sig.Run != nil && (p.sig.Run.Blank || p.sig.Run.Same):
		p.fail(newFieldError(p.redactAt, "redact: a blank or same run has no anchor match to mask; remove redact"))
	case p.sig.Measure != nil && !redactableMeasures[p.sig.Measure.Name]:
		p.fail(newFieldError(p.redactAt, "redact: the measure %s finds no value to mask (only entropy and base64-run do); remove redact",
			p.sig.Measure.Name))
	}
}

// readKind reads the definition of the signature's one kind.
func (p *sigParser) readKind(f pair) {
	p.sig.Kind = Kind(f.key)
	if p.sig.Kind != KindRegex {
		for _, extra := range p.regexKindFields {
			p.fail(newFieldError(extra.keyAt, "%s is a field of the regex kind, not of %s", extra.key, f.key))
		}
	}
	switch p.sig.Kind {
	case KindRegex:
		p.readRegexKind(f)
	case KindBlock:
		p.readBlock(f)
	case KindRun:
		p.readRun(f)
	case KindMeasure:
		p.readMeasure(f)
	}
}

// readKeywords sets the keyword clauses: the declared `keywords` as one
// clause, or the clauses derived from the anchor. A signature without
// an anchor (a measure, a blank or same run) has none and declares
// none.
func (p *sigParser) readKeywords() {
	if p.keywords == nil {
		if p.anchor == "" {
			return
		}
		clauses, err := deriveClauses(p.anchor)
		if err == nil {
			p.sig.Clauses = clauses
		}
		return
	}
	f := *p.keywords
	if !p.hasAnchor && p.sig.Kind != "" {
		p.fail(newFieldError(f.keyAt, "keywords: this signature has no anchor regex, so it has no keywords"))
		return
	}
	words, err := textList(f.value, "keywords")
	if err != nil {
		p.fail(err)
		return
	}
	clause := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < minKeywordBytes {
			p.fail(newFieldError(f.value, "keyword %q is shorter than %d bytes", w, minKeywordBytes))
			return
		}
		clause = append(clause, asciiLower(w))
	}
	p.sig.KeywordsDeclared = true
	if len(clause) > 0 {
		p.sig.Clauses = [][]string{dedupe(clause)}
	}
}

// readExamples reads `examples: {match: [...], no_match: [...]}`. A
// built-in must give at least one of each.
func (p *sigParser) readExamples() {
	p.sig.examplesLine = p.node.Line
	if p.examples != nil {
		p.sig.examplesLine = p.examples.keyAt.Line
		pairs, err := mappingPairs(p.examples.value, "examples")
		if err != nil {
			p.fail(err)
			return
		}
		for _, f := range pairs {
			var texts []string
			texts, err = textList(f.value, "examples."+f.key)
			switch {
			case err != nil:
				p.fail(err)
			case f.key == "match":
				p.sig.Examples.Match = texts
			case f.key == "no_match":
				p.sig.Examples.NoMatch = texts
			default:
				p.fail(newFieldError(f.keyAt, "unknown field %q of examples: examples has match and no_match", f.key))
			}
		}
	}
	if p.builtin && (len(p.sig.Examples.Match) == 0 || len(p.sig.Examples.NoMatch) == 0) {
		p.fail(newFieldError(p.node, "a built-in signature needs examples: at least one match and one no_match"))
	}
}

// regexSource reads a regex field: one regex, or a list of them that
// means "any of these" and is joined as (?:a)|(?:b)|…. It returns the
// source to compile, or "" after recording the problem.
func (p *sigParser) regexSource(f pair) string {
	r := resolve(f.value)
	var parts []string
	switch r.Kind {
	case yaml.ScalarNode:
		text, err := nonEmptyText(f.value, f.key)
		if err != nil {
			p.fail(err)
			return ""
		}
		parts = []string{text}
	case yaml.SequenceNode:
		items, err := textList(f.value, f.key)
		if err == nil && len(items) == 0 {
			err = newFieldError(f.value, "%s is an empty list", f.key)
		}
		if err != nil {
			p.fail(err)
			return ""
		}
		for _, item := range items {
			if item == "" {
				p.fail(newFieldError(f.value, "%s holds an empty regex", f.key))
				return ""
			}
			parts = append(parts, "(?:"+item+")")
		}
	default:
		p.fail(newFieldError(f.value, "%s must be a regex or a list of regexes", f.key))
		return ""
	}
	source := strings.Join(parts, "|")
	if len(source) > maxRegexBytes {
		p.fail(&fieldError{line: f.value.Line, wholeFile: true,
			msg: fmt.Sprintf("%s: the regex source is %d bytes, over the limit of %d bytes", f.key, len(source), maxRegexBytes)})
		return ""
	}
	return source
}

// compileRegex reads and compiles a regex field with Go's regexp (RE2
// syntax: no lookaround, no backreferences, linear time on any line).
func (p *sigParser) compileRegex(f pair) *regexp.Regexp {
	source := p.regexSource(f)
	if source == "" {
		return nil
	}
	re, err := regexp.Compile(source)
	if err != nil {
		p.fail(newFieldError(f.value, "%s: %s", f.key, err.Error()))
		return nil
	}
	return re
}

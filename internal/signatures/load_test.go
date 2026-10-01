package signatures

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeSignatureFile writes body as the file name in dir and returns
// its path.
func writeSignatureFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadFiles loads the built-ins plus the given user files and
// directories.
func loadFiles(paths ...string) *Catalogue {
	return Load(LoadOptions{Paths: paths})
}

// requireNoErrors fails the test when the load found any problem.
func requireNoErrors(t *testing.T, cat *Catalogue) {
	t.Helper()
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
}

// mustLookup returns the active signature id or fails the test.
func mustLookup(t *testing.T, cat *Catalogue, id string) *Signature {
	t.Helper()
	sig, ok := cat.Lookup(id)
	if !ok {
		t.Fatalf("%s did not load; errors: %v", id, cat.Errors())
	}
	return sig
}

// eachKindFile defines one signature of each kind and a group.
const eachKindFile = `groups:
  - name: billing
    description: Events of the billing service.

signatures:
  - id: payment-declined
    name: Payment declined
    group: billing
    description: A payment the provider declined.
    regex: ['\bpayment declined\b', '\bcard refused\b']
    not: 'test-card'
  - id: worker-dump
    name: Worker dump
    group: billing
    description: |
      The dump a worker prints when it stops.
    level: high
    tags: [worker, dump]
    block:
      start: '^=== worker dump ==='
      continue: '^\s+at '
      end: '^=== end of dump ==='
  - id: retry-storm
    name: Retry storm
    group: billing
    description: Retries in a row.
    default: false
    run: {match: '\bretrying\b', min: 3}
  - id: billing-burst
    name: Burst of billing lines
    group: billing
    description: A minute with many more lines than usual.
    measure: {name: density, bucket: 1m}
`

// A valid file with one signature of each kind loads, with the
// defaults applied and its source recorded.
func TestLoad_EachKind(t *testing.T) {
	dir := t.TempDir()
	path := writeSignatureFile(t, dir, "billing.yml", eachKindFile)
	cat := loadFiles(path)
	requireNoErrors(t, cat)

	declined := mustLookup(t, cat, "payment-declined")
	if declined.Kind != KindRegex || declined.Regex == nil || declined.Block != nil || declined.Run != nil || declined.Measure != nil {
		t.Fatalf("payment-declined: kind %q with specs %v %v %v %v", declined.Kind, declined.Regex, declined.Block, declined.Run, declined.Measure)
	}
	if got := declined.Regex.Regex.String(); got != `(?:\bpayment declined\b)|(?:\bcard refused\b)` {
		t.Errorf("a regex list compiles to %q", got)
	}
	if declined.Regex.Not == nil || declined.Regex.Not.String() != "test-card" {
		t.Errorf("not: %v", declined.Regex.Not)
	}
	if declined.Level != LevelMedium || !declined.Default || declined.Redact || declined.Regex.Capture != 0 {
		t.Errorf("defaults: level %q default %v redact %v capture %d", declined.Level, declined.Default, declined.Redact, declined.Regex.Capture)
	}
	if want := (Source{File: path, Line: 6}); declined.Source != want {
		t.Errorf("source %+v, want %+v", declined.Source, want)
	}
	if want := [][]string{{"payment declined", "card refused"}}; !reflect.DeepEqual(declined.Clauses, want) {
		t.Errorf("clauses %q, want %q", declined.Clauses, want)
	}
	if declined.Tags == nil || len(declined.Tags) != 0 || declined.Links == nil || declined.Metadata == nil {
		t.Errorf("empty lists and maps are not nil: tags %v links %v metadata %v", declined.Tags, declined.Links, declined.Metadata)
	}

	dump := mustLookup(t, cat, "worker-dump")
	if dump.Kind != KindBlock || dump.Block.MinLines != 1 || dump.Block.MaxLines != 200 || dump.Block.Lines != 0 {
		t.Errorf("worker-dump: %q, block %+v", dump.Kind, dump.Block)
	}
	if dump.Level != LevelHigh || !slices.Equal(dump.Tags, []string{"worker", "dump"}) {
		t.Errorf("worker-dump: level %q tags %v", dump.Level, dump.Tags)
	}
	if dump.Description != "The dump a worker prints when it stops.\n" {
		t.Errorf("description %q", dump.Description)
	}

	storm := mustLookup(t, cat, "retry-storm")
	if storm.Kind != KindRun || storm.Run.Min != 3 || storm.Run.Match.String() != `\bretrying\b` || storm.Default {
		t.Errorf("retry-storm: %q %+v default %v", storm.Kind, storm.Run, storm.Default)
	}

	burst := mustLookup(t, cat, "billing-burst")
	want := DensityParams{Bucket: time.Minute, Factor: 5, Z: 3.5, MinCount: 100}
	if burst.Kind != KindMeasure || burst.Measure.Name != "density" || burst.Measure.Params != want {
		t.Errorf("billing-burst: %q %+v", burst.Kind, burst.Measure)
	}
	if !burst.LinePass() || burst.Anchor() != nil {
		t.Errorf("a measure has no gate and no anchor")
	}

	groups := cat.Groups()
	last := groups[len(groups)-1]
	if last.Name != "billing" || last.Builtin || last.Order != len(groups) || last.Description != "Events of the billing service." {
		t.Errorf("declared group %+v", last)
	}
}

// ruleText is a state rule as text, for comparing compiled states.
type ruleText struct {
	State, Regex, Next string
	End                bool
}

// statesText lists a block's rules as text.
func statesText(b *BlockSpec) []ruleText {
	out := make([]ruleText, len(b.States))
	for i, r := range b.States {
		out[i] = ruleText{State: r.State, Regex: r.Regex.String(), Next: r.Next, End: r.End}
	}
	return out
}

// The simple block form compiles to the states form: end is tried
// before continue, a block with only end takes every line until it,
// and a lines block has no states.
func TestLoad_SimpleBlockCompilesToStates(t *testing.T) {
	const start = `^=== worker dump ===`
	cases := []struct {
		label string
		block string
		want  []ruleText
		lines int
	}{
		{"continue and end", "start: '" + start + "'\n      continue: '^\\s'\n      end: '^=== end'", []ruleText{
			{StateStart, start, StateBody, false},
			{StateBody, `^=== end`, "", true},
			{StateBody, `^\s`, StateBody, false},
		}, 0},
		{"continue only", "start: '" + start + "'\n      continue: '^\\s'", []ruleText{
			{StateStart, start, StateBody, false},
			{StateBody, `^\s`, StateBody, false},
		}, 0},
		{"end only", "start: '" + start + "'\n      end: '^=== end'", []ruleText{
			{StateStart, start, StateBody, false},
			{StateBody, `^=== end`, "", true},
			{StateBody, ``, StateBody, false},
		}, 0},
		{"lines", "start: '" + start + "'\n      lines: 4", []ruleText{}, 4},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		body := "signatures:\n  - id: dump\n    name: Dump\n    group: structure\n    description: A dump.\n    block:\n      " + tc.block + "\n"
		cat := loadFiles(writeSignatureFile(t, dir, "dump.yml", body))
		requireNoErrors(t, cat)
		block := mustLookup(t, cat, "dump").Block
		if got := statesText(block); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: states %+v, want %+v", tc.label, got, tc.want)
		}
		if block.Lines != tc.lines || block.Start.String() != start {
			t.Errorf("%s: lines %d start %q", tc.label, block.Lines, block.Start)
		}
	}
}

// The states form keeps its rules; the start rules joined are the
// anchor.
func TestLoad_StatesForm(t *testing.T) {
	body := `signatures:
  - id: chained
    name: Chained exception
    group: traceback
    description: A traceback and the ones it caused.
    block:
      states:
        - {state: start, regex: '^Traceback', next: frames}
        - {state: start, regex: '^Exception group', next: frames}
        - {state: frames, regex: '^\s', next: frames}
        - {state: frames, regex: '^\w+Error', end: true}
      max_lines: 500
`
	cat := loadFiles(writeSignatureFile(t, t.TempDir(), "chained.yml", body))
	requireNoErrors(t, cat)
	block := mustLookup(t, cat, "chained").Block
	want := []ruleText{
		{StateStart, `^Traceback`, "frames", false},
		{StateStart, `^Exception group`, "frames", false},
		{"frames", `^\s`, "frames", false},
		{"frames", `^\w+Error`, "", true},
	}
	if got := statesText(block); !reflect.DeepEqual(got, want) {
		t.Errorf("states %+v", got)
	}
	if block.Start.String() != `(?:^Traceback)|(?:^Exception group)` || block.MaxLines != 500 {
		t.Errorf("start %q max_lines %d", block.Start, block.MaxLines)
	}
}

// Declared keywords form one clause and replace the derived ones.
func TestLoad_DeclaredKeywordsReplaceDerived(t *testing.T) {
	body := `signatures:
  - id: declined
    name: Payment declined
    group: severity
    description: A payment the provider declined.
    regex: '\bpayment declined\b'
    keywords: [Payment, DECLINED]
  - id: derived
    name: Derived
    group: severity
    description: Keywords from the regex.
    regex: '\bpayment declined\b'
  - id: no-gate
    name: No gate
    group: severity
    description: An empty list declares no keywords.
    regex: '\bpayment declined\b'
    keywords: []
`
	cat := loadFiles(writeSignatureFile(t, t.TempDir(), "kw.yml", body))
	requireNoErrors(t, cat)
	declared := mustLookup(t, cat, "declined")
	if want := [][]string{{"payment", "declined"}}; !reflect.DeepEqual(declared.Clauses, want) || !declared.KeywordsDeclared {
		t.Errorf("declared: %q declared=%v", declared.Clauses, declared.KeywordsDeclared)
	}
	derived := mustLookup(t, cat, "derived")
	if want := [][]string{{"payment declined"}}; !reflect.DeepEqual(derived.Clauses, want) || derived.KeywordsDeclared {
		t.Errorf("derived: %q declared=%v", derived.Clauses, derived.KeywordsDeclared)
	}
	noGate := mustLookup(t, cat, "no-gate")
	if noGate.Clauses != nil || !noGate.KeywordsDeclared || !noGate.LinePass() {
		t.Errorf("no-gate: %q declared=%v", noGate.Clauses, noGate.KeywordsDeclared)
	}
}

// validSignature is a signature that loads, written into the files of
// the refusal tests: a refusal of the whole file leaves it out, a
// refusal of one signature keeps it.
const validSignature = `  - id: good-one
    name: Good one
    group: severity
    description: Loads.
    regex: 'good'
`

// errorsOf returns the load errors of one file.
func errorsOf(cat *Catalogue, file string) []LoadError {
	var out []LoadError
	for _, e := range cat.Errors() {
		if e.File == file {
			out = append(out, e)
		}
	}
	return out
}

// A YAML syntax error, a file over 1 MiB, an unknown top-level key, a
// document of several parts, a top level that is not a mapping, too
// much alias expansion, a regex over 4 KiB and a max_lines over 10,000
// each refuse the whole file: the valid signature in it does not load.
func TestLoad_WholeFileRefusals(t *testing.T) {
	bomb := "x0: &a0 [y, y, y, y, y, y, y, y, y, y]\n" +
		"x1: &a1 [*a0, *a0, *a0, *a0, *a0, *a0, *a0, *a0, *a0, *a0]\n" +
		"x2: &a2 [*a1, *a1, *a1, *a1, *a1, *a1, *a1, *a1, *a1, *a1]\n" +
		"x3: &a3 [*a2, *a2, *a2, *a2, *a2, *a2, *a2, *a2, *a2, *a2]\n" +
		"signatures:\n" + validSignature
	cases := []struct {
		label   string
		body    string
		line    int
		id      string
		message string
	}{
		{"syntax error", "signatures:\n" + validSignature + "\t- id: tabbed\n", 7, "", "YAML syntax"},
		{"file over 1 MiB", "signatures:\n" + validSignature + "#" + strings.Repeat("x", 1<<20) + "\n", 0, "", "over the limit of 1048576 bytes"},
		{"unknown top-level key", "signatures:\n" + validSignature + "disabled: [blank-run]\n", 7, "", `unknown top-level key "disabled"`},
		{"two documents", "signatures:\n" + validSignature + "---\nsignatures: []\n", 0, "", "one YAML document"},
		{"top level not a mapping", "- id: good-one\n", 1, "", "a mapping"},
		{"alias bomb", bomb, 0, "", "alias expansion"},
		{"alias cycle", "x: &x [1, *x]\nsignatures:\n" + validSignature, 0, "", "alias expansion"},
		{"regex over 4 KiB", "signatures:\n" + validSignature + "  - id: long-regex\n    name: L\n    group: severity\n    description: L.\n    regex: '" +
			strings.Repeat("a", 4097) + "'\n", 11, "long-regex", "over the limit of 4096 bytes"},
		{"max_lines over 10,000", "signatures:\n" + validSignature + "  - id: huge-block\n    name: H\n    group: severity\n    description: H.\n" +
			"    block: {start: '^x', continue: '^y', max_lines: 10001}\n", 11, "huge-block", "over the limit of 10000"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		path := writeSignatureFile(t, dir, "refused.yml", tc.body)
		cat := loadFiles(path)
		if _, ok := cat.Lookup("good-one"); ok {
			t.Errorf("%s: a signature of the refused file loaded", tc.label)
		}
		errs := errorsOf(cat, path)
		if len(errs) != 1 {
			t.Errorf("%s: errors %v, want one", tc.label, errs)
			continue
		}
		e := errs[0]
		if !strings.Contains(e.Message, tc.message) || e.ID != tc.id || (tc.line != 0 && e.Line != tc.line) {
			t.Errorf("%s: error %+v, want line %d id %q and a message with %q", tc.label, e, tc.line, tc.id, tc.message)
		}
		if !strings.Contains(e.Message, "file") {
			t.Errorf("%s: the message %q does not say the whole file is refused", tc.label, e.Message)
		}
	}
}

// An empty file, or one of comments only, defines nothing and is no
// error.
func TestLoad_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"empty.yml": "", "comments.yml": "# nothing yet\n", "null.yml": "~\n"} {
		cat := loadFiles(writeSignatureFile(t, dir, name, body))
		requireNoErrors(t, cat)
	}
}

// A bad regex, an unknown field, no kind, two kinds, a block start
// alone, an unknown group, a duplicate id and other bad values each
// refuse that signature only: the file's other signature loads, and the
// error names the file, the line and the id.
func TestLoad_SignatureRefusals(t *testing.T) {
	header := "    name: Bad\n    group: severity\n    description: Bad.\n"
	cases := []struct {
		label   string
		entry   string // a signature entry after `- id: bad-one`
		line    int    // the line of the error in the file
		message string
	}{
		{"bad regex", header + "    regex: '(foo'\n", 11, "regex: "},
		{"unknown field", header + "    shade: red\n    regex: 'x'\n", 11, `unknown field "shade"`},
		{"no kind", header, 7, "no kind"},
		{"two kinds", header + "    regex: 'x'\n    run: {blank: true, min: 3}\n", 12, "two kinds"},
		{"block start alone", header + "    block: {start: '^x'}\n", 11, "start alone"},
		{"lines with continue", header + "    block: {start: '^x', lines: 3, continue: '^y'}\n", 11, "lines"},
		{"unknown group", "    name: Bad\n    group: nowhere\n    description: Bad.\n    regex: 'x'\n", 9, `unknown group "nowhere"`},
		{"bad level", header + "    level: severe\n    regex: 'x'\n", 11, "level"},
		{"missing name", "    group: severity\n    description: Bad.\n    regex: 'x'\n", 7, `"name" is missing`},
		{"id not kebab-case", "", 7, "kebab-case"},
		{"reserved id", "", 7, "reserved"},
		{"capture past the groups", header + "    regex: '(a)b'\n    capture: 2\n", 12, "capture"},
		{"unknown validate", header + "    regex: 'x'\n    validate: crc\n", 12, "validate"},
		{"regex field on a run", header + "    run: {blank: true, min: 3}\n    capture: 1\n", 12, "capture"},
		{"keyword under 3 bytes", header + "    regex: 'xyz'\n    keywords: [xy]\n", 12, "keyword"},
		{"keywords on a measure", header + "    measure: {name: binary}\n    keywords: [abc]\n", 12, "keywords"},
		{"unknown measure", header + "    measure: {name: sparkle}\n", 11, `unknown measure "sparkle"`},
		{"unknown measure parameter", header + "    measure: {name: long-line, widow: 3}\n", 11, `"widow"`},
		{"measure parameter out of range", header + "    measure: {name: long-line, window: 0}\n", 11, "window"},
		{"duration without a unit", header + "    measure: {name: time-gap, min_gap: 60}\n", 11, "min_gap"},
		{"days past the duration limit", header + "    measure: {name: time-gap, min_gap: 213504d}\n", 11, "min_gap"},
		{"hours past the duration limit", header + "    measure: {name: time-gap, min_gap: 99999999999h}\n", 11, "min_gap"},
		{"a float that is not a number", header + "    regex: 'x'\n    entropy: .nan\n", 12, "entropy"},
		{"a measure float that is not a number", header + "    measure: {name: density, z: .nan}\n", 11, "z"},
		{"run without min", header + "    run: {blank: true}\n", 11, "min"},
		{"run with two qualifiers", header + "    run: {blank: true, same: true, min: 3}\n", 11, "exactly one"},
		{"redact on a blank run", header + "    run: {blank: true, min: 3}\n    redact: true\n", 12, "redact"},
		{"redact on a same run", header + "    redact: true\n    run: {same: true, min: 3}\n", 11, "redact"},
		{"states without a start rule", header + "    block:\n      states:\n        - {state: body, regex: 'x', next: body}\n", 13, "start"},
		{"states naming a missing state", header + "    block:\n      states:\n        - {state: start, regex: 'x', next: nowhere}\n", 13, `"nowhere"`},
		{"a link that is not http", header + "    regex: 'x'\n    links: ['javascript:alert(1)']\n", 12, "link"},
		{"duplicate key", header + "    regex: 'x'\n    regex: 'y'\n", 12, "twice"},
		{"merge key", header + "    <<: {level: high}\n    regex: 'x'\n", 11, "<<"},
	}
	for _, tc := range cases {
		id := "bad-one"
		switch tc.label {
		case "id not kebab-case":
			id = "Bad_One"
		case "reserved id":
			id = "all"
		}
		entry := tc.entry
		if entry == "" {
			entry = header + "    regex: 'x'\n"
		}
		body := "signatures:\n" + validSignature + "  - id: " + id + "\n" + entry
		dir := t.TempDir()
		path := writeSignatureFile(t, dir, "bad.yml", body)
		cat := loadFiles(path)
		mustLookup(t, cat, "good-one")
		if _, ok := cat.Lookup(id); ok {
			t.Errorf("%s: the bad signature loaded", tc.label)
		}
		errs := errorsOf(cat, path)
		if len(errs) == 0 {
			t.Errorf("%s: no error", tc.label)
			continue
		}
		e := errs[0]
		if e.ID != id || e.Line != tc.line || !strings.Contains(e.Message, tc.message) {
			t.Errorf("%s: error %+v, want id %q line %d and a message with %q", tc.label, e, id, tc.line, tc.message)
		}
	}
}

// The same id twice is an error of the later definition, in one file
// and across files; the first one loads.
func TestLoad_DuplicateID(t *testing.T) {
	dir := t.TempDir()
	first := writeSignatureFile(t, dir, "a.yml", "signatures:\n"+validSignature+validSignature)
	second := writeSignatureFile(t, dir, "b.yml", "signatures:\n"+validSignature)
	cat := loadFiles(dir)
	sig := mustLookup(t, cat, "good-one")
	if sig.Source.File != first || sig.Source.Line != 2 {
		t.Errorf("the loaded definition is %v, want the first", sig.Source)
	}
	errs := cat.Errors()
	if len(errs) != 2 {
		t.Fatalf("errors %v, want two", errs)
	}
	if errs[0].File != first || errs[0].Line != 7 || errs[1].File != second || errs[1].Line != 2 {
		t.Errorf("errors %v", errs)
	}
	for _, e := range errs {
		if e.ID != "good-one" || !strings.Contains(e.Message, "duplicate") || !strings.Contains(e.Message, first+":2") {
			t.Errorf("error %+v", e)
		}
	}
}

// A user signature with a built-in's id replaces it; `disable` switches
// a built-in off; a disable of an id no built-in has is an error of the
// file, whose other entries still load; a declared group can be used.
func TestLoad_OverrideDisableAndGroups(t *testing.T) {
	dir := t.TempDir()
	path := writeSignatureFile(t, dir, "team.yml", `disable: [aws-access-key, no-such-signature]
groups:
  - name: billing
    description: Billing.
signatures:
  - id: blank-run
    name: Many blank lines
    group: structure
    description: Five blank lines in a row.
    run: {blank: true, min: 5}
  - id: invoice-failed
    name: Invoice failed
    group: billing
    description: An invoice that failed.
    regex: 'invoice failed'
`)
	cat := loadFiles(path)

	blank := mustLookup(t, cat, "blank-run")
	if !blank.Overrides || blank.Source.Builtin || blank.Run.Min != 5 {
		t.Errorf("the override: overrides=%v source %v min %d", blank.Overrides, blank.Source, blank.Run.Min)
	}
	count := 0
	for _, s := range cat.Signatures() {
		if s.ID == "blank-run" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("blank-run is listed %d times", count)
	}

	if _, ok := cat.Lookup("aws-access-key"); ok {
		t.Error("a disabled built-in is active")
	}
	disabled, ok := cat.LookupDisabled("aws-access-key")
	if !ok || disabled.DisabledBy == nil || *disabled.DisabledBy != (Source{File: path, Line: 1}) {
		t.Errorf("disabled: %v %+v", ok, disabled)
	}
	for _, s := range append(cat.DefaultSet(), cat.Signatures()...) {
		if s.ID == "aws-access-key" {
			t.Error("a disabled built-in is in the default set or the signatures")
		}
	}

	errs := cat.Errors()
	if len(errs) != 1 || errs[0].File != path || errs[0].Line != 1 || !strings.Contains(errs[0].Message, "no-such-signature") {
		t.Errorf("errors %v, want the unknown disable", errs)
	}
	invoice := mustLookup(t, cat, "invoice-failed")
	if invoice.Group != "billing" {
		t.Errorf("group %q", invoice.Group)
	}
}

// A user signature that replaces a built-in and is refused leaves
// neither: the user meant to replace the built-in, and a selection that
// names the id repeats the error.
func TestLoad_RefusedOverrideDropsTheBuiltin(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "team.yml", `signatures:
  - id: blank-run
    name: Many blank lines
    group: structure
    description: Five blank lines in a row.
    run: {blank: true, min: 1}
`)
	cat := loadFiles(path)
	if _, ok := cat.Lookup("blank-run"); ok {
		t.Fatal("the built-in still runs after its override was refused")
	}
	if refused := cat.Refused("blank-run"); len(refused) != 1 || refused[0].File != path {
		t.Errorf("refused %v", refused)
	}
}

// Disabling a user signature is an error: disable names built-ins.
func TestLoad_DisableNamesBuiltinsOnly(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "team.yml", "disable: [good-one]\nsignatures:\n"+validSignature)
	cat := loadFiles(path)
	mustLookup(t, cat, "good-one")
	errs := cat.Errors()
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "built-in") {
		t.Errorf("errors %v", errs)
	}
}

// RX_SIGNATURES_PATH lists entries in order, files of a directory in
// name order; XDG_CONFIG_HOME is used when it is unset, then
// ~/.config; a missing directory is no error.
func TestSearchPath(t *testing.T) {
	t.Setenv("RX_SIGNATURES_PATH", "/a:/b/team.yml")
	if got := SearchPath(); !slices.Equal(got, []string{"/a", "/b/team.yml"}) {
		t.Errorf("RX_SIGNATURES_PATH: %v", got)
	}

	xdg := t.TempDir()
	t.Setenv("RX_SIGNATURES_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "rx", "signatures"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := SearchPath(); !slices.Equal(got, []string{filepath.Join(xdg, "rx", "signatures")}) {
		t.Errorf("XDG_CONFIG_HOME: %v", got)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := SearchPath(); !slices.Equal(got, []string{filepath.Join(home, ".config", "rx", "signatures")}) {
		t.Errorf("home: %v", got)
	}

	// An XDG directory that does not exist gives way to ~/.config when
	// that one exists: the first that exists is used.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "missing"))
	if err := os.MkdirAll(filepath.Join(home, ".config", "rx", "signatures"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := SearchPath(); !slices.Equal(got, []string{filepath.Join(home, ".config", "rx", "signatures")}) {
		t.Errorf("missing XDG directory: %v", got)
	}
}

// Files are loaded in the order of the search path, a directory's
// files in name order, without recursion and without dot-files; a
// missing directory is no error.
func TestLoad_PathOrder(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	sig := func(id string) string {
		return "signatures:\n  - id: " + id + "\n    name: N\n    group: severity\n    description: D.\n    regex: 'x'\n"
	}
	az := writeSignatureFile(t, a, "z.yml", sig("from-z"))
	aa := writeSignatureFile(t, a, "a.yaml", sig("from-a"))
	writeSignatureFile(t, a, "notes.txt", "not a signature file")
	writeSignatureFile(t, a, ".#a.yml", "a lock file, not YAML: [")
	if err := os.Mkdir(filepath.Join(a, "sub.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSignatureFile(t, filepath.Join(a, "sub.yml"), "deep.yml", sig("from-deep"))
	bx := writeSignatureFile(t, b, "x.yml", sig("from-b"))
	t.Setenv("RX_SIGNATURES_PATH", a+":"+filepath.Join(a, "missing")+":"+b)

	cat := Load(LoadOptions{})
	requireNoErrors(t, cat)
	if got := cat.Files(); !slices.Equal(got, []string{aa, az, bx}) {
		t.Errorf("files %v", got)
	}
	var ids []string
	for _, s := range cat.Signatures() {
		if !s.Source.Builtin {
			ids = append(ids, s.ID)
		}
	}
	if !slices.Equal(ids, []string{"from-a", "from-z", "from-b"}) {
		t.Errorf("user signatures in order %v", ids)
	}
	if got := cat.Paths(); !slices.Equal(got, []string{a, filepath.Join(a, "missing"), b}) {
		t.Errorf("paths %v", got)
	}
}

// More than 1,000 user signatures: the file that passes the limit is
// refused.
func TestLoad_TooManyUserSignatures(t *testing.T) {
	var body strings.Builder
	body.WriteString("signatures:\n")
	for i := range 1001 {
		body.WriteString("  - {id: s" + itoa(i) + ", name: N, group: severity, description: D., regex: x}\n")
	}
	path := writeSignatureFile(t, t.TempDir(), "many.yml", body.String())
	cat := loadFiles(path)
	errs := errorsOf(cat, path)
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "1000 user signatures") {
		t.Fatalf("errors %v", errs)
	}
	if _, ok := cat.Lookup("s0"); ok {
		t.Error("a signature of the refused file loaded")
	}
}

// itoa writes a small number without strconv, for test data.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// The examples check runs on every signature that has examples when the
// hook is set, and a failing example refuses the signature; without the
// hook nothing runs.
func TestLoad_CheckExamplesHook(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "ex.yml", `signatures:
  - id: checked
    name: Checked
    group: severity
    description: Has examples.
    regex: 'boom'
    examples: {match: ['a boom'], no_match: ['calm']}
  - id: failing
    name: Failing
    group: severity
    description: Has examples the check refuses.
    regex: 'bang'
    examples: {match: ['a bang']}
  - id: unchecked
    name: Unchecked
    group: severity
    description: No examples.
    regex: 'quiet'
`)
	var seen []string
	cat := Load(LoadOptions{Paths: []string{path}, CheckExamples: func(s *Signature) error {
		seen = append(seen, s.ID)
		if s.ID == "failing" {
			return errors.New(`example "a bang" gave no finding`)
		}
		return nil
	}})
	if !cat.ExamplesChecked() {
		t.Error("ExamplesChecked is false with a hook")
	}
	if !slices.Contains(seen, "checked") || !slices.Contains(seen, "python-traceback") || slices.Contains(seen, "unchecked") {
		t.Errorf("the hook saw %v", seen)
	}
	if _, ok := cat.Lookup("failing"); ok {
		t.Error("a signature whose example failed loaded")
	}
	errs := cat.Errors()
	if len(errs) != 1 || errs[0].ID != "failing" || errs[0].Line != 13 || !strings.Contains(errs[0].Message, "a bang") {
		t.Errorf("errors %v", errs)
	}

	plain := Load(LoadOptions{Paths: []string{path}})
	if plain.ExamplesChecked() {
		t.Error("ExamplesChecked is true without a hook")
	}
	mustLookup(t, plain, "failing")
}

// The embedded catalogue loads with no error, every built-in has a
// match and a no_match example, and the groups are the built-in ones in
// order.
func TestBuiltins(t *testing.T) {
	cat := Load(LoadOptions{Paths: []string{}})
	requireNoErrors(t, cat)
	for _, id := range []string{"python-traceback", "aws-access-key", "blank-run"} {
		sig := mustLookup(t, cat, id)
		if !sig.Source.Builtin || len(sig.Examples.Match) == 0 || len(sig.Examples.NoMatch) == 0 {
			t.Errorf("%s: source %v examples %+v", id, sig.Source, sig.Examples)
		}
	}
	var names []string
	for _, g := range cat.Groups() {
		if !g.Builtin || g.Description == "" {
			t.Errorf("group %+v", g)
		}
		names = append(names, g.Name)
	}
	want := []string{"traceback", "crash", "secret", "pii", "sql", "http", "severity", "system", "lifecycle", "structure", "time"}
	if !slices.Equal(names, want) {
		t.Errorf("groups %v", names)
	}
	if !mustLookup(t, cat, "aws-access-key").Redact {
		t.Error("aws-access-key is not redacted")
	}
	if got := mustLookup(t, cat, "aws-access-key").Clauses; !reflect.DeepEqual(got, [][]string{{"a3t", "akia", "asia", "abia", "acca"}}) {
		t.Errorf("aws-access-key clauses %q", got)
	}
	if len(cat.Paths()) != 0 || len(cat.Files()) != 0 {
		t.Errorf("paths %v files %v", cat.Paths(), cat.Files())
	}
}

// A built-in without examples is an error: a definition in the binary
// is checked like the others.
func TestParse_BuiltinNeedsExamples(t *testing.T) {
	file, fileErr := parseFile("builtin/x.yml", []byte("signatures:\n"+validSignature), true)
	if fileErr != nil {
		t.Fatal(fileErr)
	}
	if len(file.entries) != 1 || file.entries[0].sig != nil || len(file.entries[0].errs) != 1 ||
		!strings.Contains(file.entries[0].errs[0].Message, "examples") {
		t.Errorf("entries %+v", file.entries)
	}
}

// Measure parameters take their defaults, and each measure's Fields
// lists the parameters of the measure table in its order.
func TestMeasureParams(t *testing.T) {
	want := map[string]MeasureParams{
		"long-line":      LongLineParams{Window: 100, Factor: 10, MinBytes: 1024},
		"binary":         BinaryParams{},
		"entropy":        EntropyParams{MinLength: 20, Base64: 4.5, Hex: 3},
		"json-block":     JSONBlockParams{MaxLines: 200},
		"unprefixed":     UnprefixedParams{Min: 5, MinShare: 0.5},
		"base64-run":     Base64RunParams{MinLength: 100},
		"density":        DensityParams{Bucket: 0, Factor: 5, Z: 3.5, MinCount: 100},
		"time-gap":       TimeGapParams{MinGap: time.Minute, Factor: 50},
		"time-backwards": TimeBackwardsParams{Tolerance: time.Second},
	}
	if names := MeasureNames(); len(names) != len(want) {
		t.Fatalf("measures %v", names)
	}
	for name, params := range want {
		got, ok := defaultMeasureParams(name)
		if !ok || got != params {
			t.Errorf("%s: defaults %+v, want %+v", name, got, params)
			continue
		}
		var tableNames, fieldNames []string
		for _, p := range measureDefs[name].params {
			tableNames = append(tableNames, p.name)
		}
		for _, f := range got.Fields() {
			fieldNames = append(fieldNames, f.Name)
		}
		if !slices.Equal(tableNames, fieldNames) {
			t.Errorf("%s: fields %v, table %v", name, fieldNames, tableNames)
		}
	}
}

// Canonical bytes name the measure and every parameter in a fixed form.
func TestMeasureParams_Canonical(t *testing.T) {
	cases := map[string]MeasureParams{
		"long-line;window=100;factor=10;min_bytes=1024":    LongLineParams{Window: 100, Factor: 10, MinBytes: 1024},
		"density;bucket=auto;factor=5;z=3.5;min_count=100": DensityParams{Factor: 5, Z: 3.5, MinCount: 100},
		"density;bucket=90s;factor=5;z=3.5;min_count=100":  DensityParams{Bucket: 90 * time.Second, Factor: 5, Z: 3.5, MinCount: 100},
		"time-gap;min_gap=1d;factor=50":                    TimeGapParams{MinGap: 24 * time.Hour, Factor: 50},
		"time-backwards;tolerance=500ms":                   TimeBackwardsParams{Tolerance: 500 * time.Millisecond},
		"binary":                                           BinaryParams{},
	}
	for want, params := range cases {
		if got := string(params.Canonical()); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// Measure parameters given in a file: durations with units, "auto",
// numbers within range.
func TestLoad_MeasureParameters(t *testing.T) {
	body := `signatures:
  - id: gaps
    name: Gaps
    group: time
    description: Gaps.
    measure: {name: time-gap, min_gap: 1d, factor: 2.5}
  - id: bursts
    name: Bursts
    group: time
    description: Bursts.
    measure: {name: density, bucket: auto, z: 4}
  - id: long
    name: Long
    group: structure
    description: Long.
    measure: {name: long-line, window: 50}
`
	cat := loadFiles(writeSignatureFile(t, t.TempDir(), "m.yml", body))
	requireNoErrors(t, cat)
	if got := mustLookup(t, cat, "gaps").Measure.Params; got != (TimeGapParams{MinGap: 24 * time.Hour, Factor: 2.5}) {
		t.Errorf("gaps %+v", got)
	}
	if got := mustLookup(t, cat, "bursts").Measure.Params; got != (DensityParams{Factor: 5, Z: 4, MinCount: 100}) {
		t.Errorf("bursts %+v", got)
	}
	if got := mustLookup(t, cat, "long").Measure.Params; got != (LongLineParams{Window: 50, Factor: 10, MinBytes: 1024}) {
		t.Errorf("long %+v", got)
	}
}

// Signatures are ordered by group, then load order.
func TestLoad_SignatureOrder(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "o.yml", `signatures:
  - {id: z-traceback, name: Z, group: traceback, description: D., regex: 'zzz'}
  - {id: a-severity, name: A, group: severity, description: D., regex: 'aaa'}
`)
	cat := loadFiles(path)
	requireNoErrors(t, cat)
	var ids []string
	for _, s := range cat.Signatures() {
		ids = append(ids, s.ID)
	}
	want := []string{"python-traceback", "z-traceback", "aws-access-key", "a-severity", "blank-run"}
	if !slices.Equal(ids, want) {
		t.Errorf("order %v, want %v", ids, want)
	}
}

// A duration is a whole number of days or Go's syntax, at most
// maxDurationDays days: a longer one, or one that does not fit a
// time.Duration, is an error rather than a wrapped-around value.
func TestParseDuration(t *testing.T) {
	cases := []struct {
		text string
		want time.Duration
		ok   bool
	}{
		{"60s", time.Minute, true},
		{"500ms", 500 * time.Millisecond, true},
		{"1d", 24 * time.Hour, true},
		{"3650d", 3650 * 24 * time.Hour, true},
		{"3651d", 0, false},
		{"213504d", 0, false},
		{"9223372036854775807d", 0, false},
		{"87601h", 0, false},
		{"99999999999h", 0, false},
		{"-1d", 0, false},
		{"-5s", 0, false},
		{"d", 0, false},
	}
	for _, tc := range cases {
		got, err := parseDuration(tc.text)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("%q: %v, %v; want %v, ok=%v", tc.text, got, err, tc.want, tc.ok)
		}
	}
}

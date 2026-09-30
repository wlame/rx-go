package signatures

import (
	"reflect"
	"strings"
	"testing"
)

// The definition of a built-in is shown as YAML with every default
// filled in, and the simple block form as written.
func TestDefinitionYAML_Builtin(t *testing.T) {
	cat := Load(LoadOptions{Paths: []string{}})
	got := string(mustLookup(t, cat, "python-traceback").DefinitionYAML())
	for _, want := range []string{
		"id: python-traceback\n",
		"level: high\n",
		"default: true\n",
		"redact: false\n",
		"tags: [python]\n",
		"links: ['https://docs.python.org/3/library/traceback.html']\n",
		"metadata: {}\n",
		"block:\n  start: '^Traceback \\(most recent call last\\):'\n  continue: '^\\s'\n",
		"  min_lines: 1\n  max_lines: 200\n",
		"examples:\n  match:\n    - |\n      Traceback (most recent call last):\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the definition lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "states") {
		t.Errorf("a simple block is shown in the states form:\n%s", got)
	}
}

// Every signature's definition loads back into the same signature: the
// YAML shown is a definition a user can copy.
func TestDefinitionYAML_LoadsBack(t *testing.T) {
	first := loadFiles(writeSignatureFile(t, t.TempDir(), "kinds.yml", eachKindFile+`  - id: chained
    name: Chained
    group: billing
    description: States.
    redact: true
    keywords: [traceback]
    block:
      states:
        - {state: start, regex: '^Traceback', next: frames}
        - {state: frames, regex: '^\s', next: frames}
        - {state: frames, regex: '^\w+Error', end: true}
      not: 'ignored'
  - id: dump-lines
    name: Dump lines
    group: billing
    description: Lines.
    block: {start: '^dump', lines: 3}
  - id: card
    name: Card
    group: billing
    description: Regex fields.
    regex: '(\d{16})'
    capture: 1
    entropy: 2.5
    validate: luhn
    metadata: {owner: payments}
    links: [https://runbooks.example.com/card]
`))
	requireNoErrors(t, first)
	var body strings.Builder
	body.WriteString("groups:\n  - name: billing\n    description: Billing.\nsignatures:\n")
	for _, s := range first.Signatures() {
		if s.Source.Builtin {
			continue
		}
		for i, line := range strings.Split(strings.TrimRight(string(s.DefinitionYAML()), "\n"), "\n") {
			prefix := "    "
			if i == 0 {
				prefix = "  - "
			}
			body.WriteString(prefix + line + "\n")
		}
	}
	second := loadFiles(writeSignatureFile(t, t.TempDir(), "again.yml", body.String()))
	requireNoErrors(t, second)
	for _, a := range first.Signatures() {
		if a.Source.Builtin {
			continue
		}
		b := mustLookup(t, second, a.ID)
		if a.Kind != b.Kind || !reflect.DeepEqual(a.Clauses, b.Clauses) || a.KeywordsDeclared != b.KeywordsDeclared ||
			a.Level != b.Level || a.Default != b.Default || a.Redact != b.Redact || a.Description != b.Description ||
			!reflect.DeepEqual(a.Tags, b.Tags) || !reflect.DeepEqual(a.Links, b.Links) || !reflect.DeepEqual(a.Metadata, b.Metadata) ||
			!reflect.DeepEqual(a.Examples, b.Examples) {
			t.Errorf("%s differs after a round trip:\n%s", a.ID, a.DefinitionYAML())
		}
		if a.Measure != nil && a.Measure.Params != b.Measure.Params {
			t.Errorf("%s: params %+v, then %+v", a.ID, a.Measure.Params, b.Measure.Params)
		}
		if a.Block != nil && (!reflect.DeepEqual(statesText(a.Block), statesText(b.Block)) || a.Block.Lines != b.Block.Lines ||
			a.Block.MaxLines != b.Block.MaxLines || a.Block.MinLines != b.Block.MinLines) {
			t.Errorf("%s: block %+v, then %+v", a.ID, a.Block, b.Block)
		}
		if a.Regex != nil && (a.Regex.Regex.String() != b.Regex.Regex.String() || a.Regex.Capture != b.Regex.Capture ||
			a.Regex.Validate != b.Regex.Validate || !reflect.DeepEqual(a.Regex.Entropy, b.Regex.Entropy)) {
			t.Errorf("%s: regex %+v, then %+v", a.ID, a.Regex, b.Regex)
		}
	}
}

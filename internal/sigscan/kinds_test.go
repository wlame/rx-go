package sigscan

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// regexSig is a regex signature definition with the given extra
// fields, one per line, already indented.
func regexSig(id, regex string, extra ...string) string {
	return fmt.Sprintf("  - id: %s\n    name: %s\n    group: severity\n    description: Test.\n    regex: '%s'\n%s",
		id, id, regex, indentFields(extra))
}

// indentFields indents signature fields under a list item.
func indentFields(fields []string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString("    " + f + "\n")
	}
	return b.String()
}

// kindSig is a block or run signature: kind is "block" or "run", body
// its fields (YAML flow or indented block form).
func kindSig(id, kind string, fields ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  - id: %s\n    name: %s\n    group: structure\n    description: Test.\n    %s:\n", id, id, kind)
	for _, f := range fields {
		b.WriteString("      " + f + "\n")
	}
	return b.String()
}

// fixture is one case of the scan's rules: signature definitions, a
// text, and the findings each signature must give in it.
type fixture struct {
	name string
	def  string
	ids  []string
	text string
	opts Options
	want map[string][]lineSpan
	// value is the value span of the first finding of ids[0];
	// the zero value is not checked.
	value [2]int
	// timed checks every finding's time against oracleTime.
	timed bool
}

// one is a fixture of a single signature, whose id is read from def.
func one(name, def, text string, want ...lineSpan) fixture {
	id := idOf(def)
	return fixture{name: name, def: def, ids: []string{id}, text: text, want: map[string][]lineSpan{id: want}}
}

// withOpts returns the fixture with opts.
func (fx fixture) withOpts(opts Options) fixture {
	fx.opts = opts
	return fx
}

// withValue returns the fixture expecting a value span.
func (fx fixture) withValue(start, end int) fixture {
	fx.value = [2]int{start, end}
	return fx
}

// check fails when a result is not what the fixture expects.
func (fx fixture) check(t *testing.T, res *Result) {
	t.Helper()
	for id, want := range fx.want {
		if got := spans(res, id); !slices.Equal(got, want) {
			t.Errorf("%s: findings %v, want %v", id, got, want)
		}
	}
	if fx.value != [2]int{} {
		f := res.BySignature[fx.ids[0]].Findings[0]
		if f.ValueStart == nil || *f.ValueStart != fx.value[0] || *f.ValueEnd != fx.value[1] {
			t.Errorf("value span %v-%v, want %v", deref(f.ValueStart), deref(f.ValueEnd), fx.value)
		}
	}
	if fx.timed {
		for _, id := range fx.ids {
			for _, f := range res.BySignature[id].Findings {
				want := oracleTime(t, fx.text, f.StartOffset, int64(fx.opts.LookbackBytes))
				if got := derefTime(f.TimeMS); got != want {
					t.Errorf("%s at line %d: time %d, want %d", id, f.StartLine, got, want)
				}
			}
		}
	}
}

// run scans the fixture's text as one plain file and checks it.
func (fx fixture) run(t *testing.T) {
	t.Helper()
	fx.check(t, scanOne(t, fx.text, loadSigs(t, sigFile(fx.def), fx.ids...), fx.opts))
}

// idOf returns the id of a signature definition.
func idOf(def string) string {
	rest := def[strings.Index(def, "id: ")+4:]
	return rest[:strings.Index(rest, "\n")]
}

// deref shows a value span bound, -1 for none.
func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// regexFixtures: not, capture, entropy and each validate decide the
// line; a valid value's line is a finding, an invalid one's is not.
var regexFixtures = []fixture{
	one("not", regexSig("del", `(?i)\bdelete\s+from\s+\S+`, `not: '(?i)\bwhere\b'`),
		lines("DELETE FROM users WHERE id = 1", "delete from users"), lineSpan{2, 2}),
	one("capture", regexSig("tok", `token=([a-z0-9]+)`, "capture: 1"),
		lines("x token=abc123 y"), lineSpan{1, 1}).withValue(8, 14),
	one("entropy", regexSig("key", `key=(\S+)`, "capture: 1", "entropy: 3.0"),
		lines("key=aaaaaaaaaaaaaaaa", "key=Zx8Qp2Lm9Rt4Wv7Y"), lineSpan{2, 2}).withValue(4, 20),
	one("luhn", regexSig("card", `card (\d{16})`, "capture: 1", "validate: luhn"),
		lines("card 4111111111111112", "card 4111111111111111"), lineSpan{2, 2}).withValue(5, 21),
	one("iban", regexSig("iban", `iban ([A-Z]{2}\d{2}[A-Z0-9]{11,30})`, "capture: 1", "validate: iban"),
		lines("iban GB82WEST12345698765433", "iban GB82WEST12345698765432"), lineSpan{2, 2}).withValue(5, 27),
	one("ipv4", regexSig("ip4", `from (\S+)`, "capture: 1", "validate: ipv4"),
		lines("from 999.1.1.1", "from 192.168.1.10", "from 010.1.1.1"), lineSpan{2, 2}).withValue(5, 17),
	one("ipv6", regexSig("ip6", `addr (\S+)`, "capture: 1", "validate: ipv6"),
		lines("addr fe80:::1", "addr fe80::1"), lineSpan{2, 2}).withValue(5, 12),
	{name: "two signatures on one line", def: regexSig("err-word", `ERROR`) + regexSig("code-word", `code=\d+`),
		ids: []string{"err-word", "code-word"}, text: lines("ERROR code=1 ERROR code=2"),
		want: map[string][]lineSpan{"err-word": {{1, 1}}, "code-word": {{1, 1}}}},
}

// A regex signature with not, capture, entropy and each validate finds
// the valid value's line and not the invalid one.
func TestScan_RegexFieldsDecideTheLine(t *testing.T) {
	for _, fx := range regexFixtures {
		t.Run(fx.name, fx.run)
	}
}

// Two signatures on one line give two findings; one signature with two
// matches on a line gives one.
func TestScan_RegexOneFindingPerLinePerSignature(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("err-word", `ERROR`), regexSig("code-word", `code=\d+`)), "err-word", "code-word")
	res := scanOne(t, lines("ERROR code=1 ERROR code=2"), sigs, Options{})
	for _, id := range []string{"err-word", "code-word"} {
		if got := spans(res, id); !slices.Equal(got, []lineSpan{{1, 1}}) || res.BySignature[id].Count != 1 {
			t.Errorf("%s: findings %v count %d, want one on line 1", id, got, res.BySignature[id].Count)
		}
	}
}

// block is a fixture of one block signature with fields.
func block(name string, fields []string, text string, want ...lineSpan) fixture {
	return one(name, kindSig("blk", "block", fields...), text, want...)
}

// blockSimpleFixtures: the simple form of a block, each rule and bound.
var blockSimpleFixtures = []fixture{
	block("start continue end", []string{`start: '^Traceback'`, `continue: '^\s'`, `end: '^\w+Error:'`},
		lines("before", "Traceback (most recent call last):", "  File a.py", "ValueError: bad", "after"), lineSpan{2, 4}),
	block("end only", []string{`start: '^BEGIN'`, `end: '^END'`},
		lines("x", "BEGIN", "anything", "", "END", "y"), lineSpan{2, 5}),
	block("continue only", []string{`start: '^Traceback'`, `continue: '^\s'`},
		lines("Traceback", "  one", "  two", "next"), lineSpan{1, 3}),
	block("lines 3", []string{`start: '^HEAD'`, "lines: 3"},
		lines("HEAD", "a", "b", "c", "d"), lineSpan{1, 4}),
	block("max_lines cut", []string{`start: '^S'`, `continue: '^ '`, "max_lines: 3"},
		lines("S", " 1", " 2", " 3", " 4"), lineSpan{1, 3}),
	block("min_lines drop", []string{`start: '^S'`, `continue: '^ '`, "min_lines: 3"},
		lines("S", " 1", "x", "S", " 1", " 2", "y"), lineSpan{4, 6}),
	block("trailing empty lines dropped", []string{`start: '^S'`, `continue: '^(\s.*)?$'`},
		lines("S", " 1", "", "\t", "x"), lineSpan{1, 2}),
	block("end of file closes", []string{`start: '^S'`, `continue: '^ '`},
		lines("x", "S", " 1", " 2"), lineSpan{2, 4}),
}

// The simple form of a block, each rule and bound.
func TestScan_BlockSimpleForm(t *testing.T) {
	for _, fx := range blockSimpleFixtures {
		t.Run(fx.name, fx.run)
	}
}

// chainedExceptionSig is a Python traceback in the states form that
// joins a chained exception into one block.
var chainedExceptionSig = kindSig("py-chain", "block",
	"states:",
	`  - {state: start, regex: '^Traceback \(most recent call last\):', next: frames}`,
	`  - {state: frames, regex: '^\s', next: frames}`,
	`  - {state: frames, regex: '^[A-Za-z_][\w.]*(?::|$)', next: after}`,
	`  - {state: after, regex: '^$', next: after}`,
	`  - {state: after, regex: '^(During handling of the above exception|The above exception was the direct cause)', next: chained}`,
	`  - {state: chained, regex: '^$', next: chained}`,
	`  - {state: chained, regex: '^Traceback \(most recent call last\):', next: frames}`,
	"max_lines: 400")

// chainedExceptionFixture: the Python chained-exception shape
// (exception line, empty line, "During handling…", new traceback).
var chainedExceptionFixture = one("chained exception", chainedExceptionSig, lines(
	"INFO start",
	"Traceback (most recent call last):",
	`  File "app.py", line 3, in <module>`,
	"ValueError: bad input",
	"",
	"During handling of the above exception, another exception occurred:",
	"",
	"Traceback (most recent call last):",
	`  File "app.py", line 5, in <module>`,
	"KeyError: 'x'",
	"",
	"INFO next"), lineSpan{2, 10})

// The states form joins the Python chained-exception shape into one
// finding.
func TestScan_BlockStatesChainedException(t *testing.T) {
	chainedExceptionFixture.run(t)
}

// blockOverlapFixtures: blocks of one signature never overlap.
var blockOverlapFixtures = []fixture{
	block("a start inside an open block is not a new finding", []string{`start: '^BEGIN'`, `end: '^END'`},
		lines("BEGIN", "x", "BEGIN", "y", "END"), lineSpan{1, 5}),
	block("a start inside a block dropped by min_lines is not retried", []string{`start: '^S'`, "lines: 2", "min_lines: 3"},
		lines("S1", "S2", "", "a", "b")),
	block("a start on the line the block refused is a new finding", []string{`start: '^BEGIN'`, `continue: '^ '`},
		lines("BEGIN", " a", "BEGIN", " b", "x"), lineSpan{1, 2}, lineSpan{3, 4}),
	block("the line that ends a block is not a new start", []string{`start: '^S'`, `end: '^S'`},
		lines("S", "S", "S", "S"), lineSpan{1, 2}, lineSpan{3, 4}),
}

// Blocks of one signature never overlap.
func TestScan_BlockOverlap(t *testing.T) {
	for _, fx := range blockOverlapFixtures {
		t.Run(fx.name, fx.run)
	}
}

// run is a fixture of one run signature with fields.
func run(name string, fields []string, text string, want ...lineSpan) fixture {
	return one(name, kindSig("rn", "run", fields...), text, want...)
}

// runFixtures: runs of blank lines, of equal lines (one longer than the
// view), and of matching lines behind the gate.
var runFixtures = func() []fixture {
	long := strings.Repeat("a", 2000)
	return []fixture{
		run("two blank lines are no run of 3", []string{"blank: true", "min: 3"}, lines("a", "", "", "b")),
		run("four blank lines are one run", []string{"blank: true", "min: 3"},
			lines("a", "", "  ", "\t", "", "b"), lineSpan{2, 5}),
		run("same compares the view and the full length", []string{"same: true", "min: 2"},
			lines(long, long, long+"b"), lineSpan{1, 2}).withOpts(Options{LineBytes: 1024}),
		run("same lines longer than the view equal by view and length", []string{"same: true", "min: 3"},
			lines(long+"x", long+"y", long+"z", "end"), lineSpan{1, 3}).withOpts(Options{LineBytes: 1024}),
		one("match lines pass the gate", kindSig("rn", "run", "match: 'ERROR'", "min: 2")+"    keywords: [boom]\n",
			lines("ERROR boom", "ERROR boom", "ERROR x", "ERROR boom"), lineSpan{1, 2}),
	}
}()

// Runs of blank lines, of equal lines (one longer than the view), and
// of matching lines behind the gate.
func TestScan_Runs(t *testing.T) {
	for _, fx := range runFixtures {
		t.Run(fx.name, fx.run)
	}
}

// gateFixture: a line that matches the regex but holds no keyword gives
// no finding; keywords are compared ASCII case-insensitively.
var gateFixture = fixture{
	name: "gate",
	def:  regexSig("declared", `timeout`, "keywords: [deadline]") + regexSig("derived", `(?i)connection reset`),
	ids:  []string{"declared", "derived"},
	text: lines("timeout reached", "DEADLINE: timeout", "CONNECTION RESET by peer", "Connection Reset"),
	want: map[string][]lineSpan{"declared": {{2, 2}}, "derived": {{3, 3}, {4, 4}}},
}

// A line that matches the regex but holds no keyword gives no finding;
// keywords are compared ASCII case-insensitively.
func TestScan_GateIsPartOfTheMeaning(t *testing.T) {
	gateFixture.run(t)
}

// foldFixture: under (?i) Go's regexp matches k to the Kelvin sign
// (U+212A) and s to the long s (U+017F), which no keyword holds. A line
// whose view holds either letter skips the gate, so every anchor is
// tested on it: regex, block start and match run, declared keywords
// included. A line of ASCII only still needs its keywords ("timeout
// reached"), and a fold letter alone makes no finding ("nothing ſ here").
var foldFixture = fixture{
	name: "fold letters skip the gate",
	def: regexSig("sel", `(?i)select`) + regexSig("kern", `(?i)kernel panic`) +
		regexSig("declared", `timeout`, "keywords: [deadline]") +
		kindSig("tb", "block", `start: '(?i)^traceback'`, `continue: '^\s'`) +
		kindSig("df", "run", `match: '(?i)disk full'`, "min: 2"),
	ids: []string{"sel", "kern", "declared", "tb", "df"},
	text: lines(
		"ſelect 1",
		"Kernel panic",
		"select 2",
		"timeout reached",
		"timeout reached K",
		"Traceback (most recent call last):",
		`  File "a.py"`,
		"TracebacK (most recent call last):",
		`  File "b.py"`,
		"diſk full on /var",
		"disK full on /var",
		"nothing ſ here",
	),
	want: map[string][]lineSpan{
		"sel":      {{1, 1}, {3, 3}},
		"kern":     {{2, 2}},
		"declared": {{5, 5}},
		"tb":       {{6, 7}, {8, 9}},
		"df":       {{10, 11}},
	},
}

// A match made through the Kelvin sign or the long s is found: the
// line skips the gate. A line of ASCII only still goes through it.
func TestScan_FoldLettersSkipTheGate(t *testing.T) {
	foldFixture.run(t)
}

// scenarioFixtures are every fixture of the scan's rules, for the
// same-answer test.
func scenarioFixtures() []fixture {
	all := slices.Concat(regexFixtures, blockSimpleFixtures, blockOverlapFixtures, runFixtures, excerptFixtures)
	all = append(all, chainedExceptionFixture, gateFixture, foldFixture, timeFixture, replayTimeFixture)
	all = append(all, binaryFixture, entropyFixture, base64Fixture, unprefixedFixture, longLineFixture, jsonBlockMoreFixture)
	return append(all, jsonBlockFixtures...)
}

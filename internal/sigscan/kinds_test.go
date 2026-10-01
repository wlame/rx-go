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

// A regex signature with not, capture, entropy and each
// validate finds the valid value's line and not the invalid one.
func TestScan_RegexFieldsDecideTheLine(t *testing.T) {
	cases := []struct {
		name  string
		def   string
		text  string
		want  []lineSpan
		value [2]int // value span of the first finding; {0,0}: not checked
	}{
		{"not", regexSig("del", `(?i)\bdelete\s+from\s+\S+`, `not: '(?i)\bwhere\b'`),
			lines("DELETE FROM users WHERE id = 1", "delete from users"), []lineSpan{{2, 2}}, [2]int{}},
		{"capture", regexSig("tok", `token=([a-z0-9]+)`, "capture: 1"),
			lines("x token=abc123 y"), []lineSpan{{1, 1}}, [2]int{8, 14}},
		{"entropy", regexSig("key", `key=(\S+)`, "capture: 1", "entropy: 3.0"),
			lines("key=aaaaaaaaaaaaaaaa", "key=Zx8Qp2Lm9Rt4Wv7Y"), []lineSpan{{2, 2}}, [2]int{4, 20}},
		{"luhn", regexSig("card", `card (\d{16})`, "capture: 1", "validate: luhn"),
			lines("card 4111111111111112", "card 4111111111111111"), []lineSpan{{2, 2}}, [2]int{5, 21}},
		{"iban", regexSig("iban", `iban ([A-Z]{2}\d{2}[A-Z0-9]{11,30})`, "capture: 1", "validate: iban"),
			lines("iban GB82WEST12345698765433", "iban GB82WEST12345698765432"), []lineSpan{{2, 2}}, [2]int{5, 27}},
		{"ipv4", regexSig("ip4", `from (\S+)`, "capture: 1", "validate: ipv4"),
			lines("from 999.1.1.1", "from 192.168.1.10", "from 010.1.1.1"), []lineSpan{{2, 2}}, [2]int{5, 17}},
		{"ipv6", regexSig("ip6", `addr (\S+)`, "capture: 1", "validate: ipv6"),
			lines("addr fe80:::1", "addr fe80::1"), []lineSpan{{2, 2}}, [2]int{5, 12}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := loadSigs(t, sigFile(tc.def), idOf(tc.def))
			res := scanOne(t, tc.text, sigs, Options{})
			id := sigs[0].ID
			if got := spans(res, id); !slices.Equal(got, tc.want) {
				t.Fatalf("findings %v, want %v", got, tc.want)
			}
			if tc.value != [2]int{} {
				f := res.BySignature[id].Findings[0]
				if f.ValueStart == nil || *f.ValueStart != tc.value[0] || *f.ValueEnd != tc.value[1] {
					t.Errorf("value span %v-%v, want %v", deref(f.ValueStart), deref(f.ValueEnd), tc.value)
				}
			}
		})
	}
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

// Two signatures on one line give two findings; one signature
// with two matches on a line gives one.
func TestScan_RegexOneFindingPerLinePerSignature(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("err-word", `ERROR`), regexSig("code-word", `code=\d+`)), "err-word", "code-word")
	res := scanOne(t, lines("ERROR code=1 ERROR code=2"), sigs, Options{})
	for _, id := range []string{"err-word", "code-word"} {
		if got := spans(res, id); !slices.Equal(got, []lineSpan{{1, 1}}) || res.BySignature[id].Count != 1 {
			t.Errorf("%s: findings %v count %d, want one on line 1", id, got, res.BySignature[id].Count)
		}
	}
}

// The simple form of a block, each rule and bound.
func TestScan_BlockSimpleForm(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		text   string
		want   []lineSpan
	}{
		{"start continue end", []string{`start: '^Traceback'`, `continue: '^\s'`, `end: '^\w+Error:'`},
			lines("before", "Traceback (most recent call last):", "  File a.py", "ValueError: bad", "after"), []lineSpan{{2, 4}}},
		{"end only", []string{`start: '^BEGIN'`, `end: '^END'`},
			lines("x", "BEGIN", "anything", "", "END", "y"), []lineSpan{{2, 5}}},
		{"continue only", []string{`start: '^Traceback'`, `continue: '^\s'`},
			lines("Traceback", "  one", "  two", "next"), []lineSpan{{1, 3}}},
		{"lines 3", []string{`start: '^HEAD'`, "lines: 3"},
			lines("HEAD", "a", "b", "c", "d"), []lineSpan{{1, 4}}},
		{"max_lines cut", []string{`start: '^S'`, `continue: '^ '`, "max_lines: 3"},
			lines("S", " 1", " 2", " 3", " 4"), []lineSpan{{1, 3}}},
		{"min_lines drop", []string{`start: '^S'`, `continue: '^ '`, "min_lines: 3"},
			lines("S", " 1", "x", "S", " 1", " 2", "y"), []lineSpan{{4, 6}}},
		{"trailing empty lines dropped", []string{`start: '^S'`, `continue: '^(\s.*)?$'`},
			lines("S", " 1", "", "\t", "x"), []lineSpan{{1, 2}}},
		{"end of file closes", []string{`start: '^S'`, `continue: '^ '`},
			lines("x", "S", " 1", " 2"), []lineSpan{{2, 4}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := loadSigs(t, sigFile(kindSig("blk", "block", tc.fields...)), "blk")
			res := scanOne(t, tc.text, sigs, Options{})
			if got := spans(res, "blk"); !slices.Equal(got, tc.want) {
				t.Fatalf("findings %v, want %v", got, tc.want)
			}
		})
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

// The states form joins the Python chained-exception shape
// (exception line, empty line, "During handling…", new traceback) into
// one finding.
func TestScan_BlockStatesChainedException(t *testing.T) {
	sigs := loadSigs(t, sigFile(chainedExceptionSig), "py-chain")
	text := lines(
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
		"INFO next")
	res := scanOne(t, text, sigs, Options{})
	if got := spans(res, "py-chain"); !slices.Equal(got, []lineSpan{{2, 10}}) {
		t.Fatalf("findings %v, want one block of lines 2-10", got)
	}
}

// Blocks of one signature never overlap.
func TestScan_BlockOverlap(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		text   string
		want   []lineSpan
	}{
		{"a start inside an open block is not a new finding", []string{`start: '^BEGIN'`, `end: '^END'`},
			lines("BEGIN", "x", "BEGIN", "y", "END"), []lineSpan{{1, 5}}},
		{"a start inside a block dropped by min_lines is not retried", []string{`start: '^S'`, "lines: 2", "min_lines: 3"},
			lines("S1", "S2", "", "a", "b"), nil},
		{"a start on the line the block refused is a new finding", []string{`start: '^BEGIN'`, `continue: '^ '`},
			lines("BEGIN", " a", "BEGIN", " b", "x"), []lineSpan{{1, 2}, {3, 4}}},
		{"the line that ends a block is not a new start", []string{`start: '^S'`, `end: '^S'`},
			lines("S", "S", "S", "S"), []lineSpan{{1, 2}, {3, 4}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := loadSigs(t, sigFile(kindSig("blk", "block", tc.fields...)), "blk")
			res := scanOne(t, tc.text, sigs, Options{})
			if got := spans(res, "blk"); !slices.Equal(got, tc.want) {
				t.Fatalf("findings %v, want %v", got, tc.want)
			}
		})
	}
}

// Runs of blank lines, of equal lines (one longer than the
// view), and of matching lines behind the gate.
func TestScan_Runs(t *testing.T) {
	long := strings.Repeat("a", 2000)
	cases := []struct {
		name   string
		fields []string
		text   string
		opts   Options
		want   []lineSpan
	}{
		{"two blank lines are no run of 3", []string{"blank: true", "min: 3"},
			lines("a", "", "", "b"), Options{}, nil},
		{"four blank lines are one run", []string{"blank: true", "min: 3"},
			lines("a", "", "  ", "\t", "", "b"), Options{}, []lineSpan{{2, 5}}},
		{"same compares the view and the full length", []string{"same: true", "min: 2"},
			lines(long, long, long+"b"), Options{LineBytes: 1024}, []lineSpan{{1, 2}}},
		{"same lines longer than the view equal by view and length", []string{"same: true", "min: 3"},
			lines(long+"x", long+"y", long+"z", "end"), Options{LineBytes: 1024}, []lineSpan{{1, 3}}},
		{"match lines pass the gate", []string{"match: 'ERROR'", "min: 2"},
			lines("ERROR boom", "ERROR boom", "ERROR x", "ERROR boom"), Options{}, []lineSpan{{1, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := kindSig("rn", "run", tc.fields...)
			if strings.Contains(tc.name, "gate") {
				def += "    keywords: [boom]\n"
			}
			sigs := loadSigs(t, sigFile(def), "rn")
			res := scanOne(t, tc.text, sigs, tc.opts)
			if got := spans(res, "rn"); !slices.Equal(got, tc.want) {
				t.Fatalf("findings %v, want %v", got, tc.want)
			}
		})
	}
}

// A line that matches the regex but holds no keyword gives no
// finding; keywords are compared ASCII case-insensitively.
func TestScan_GateIsPartOfTheMeaning(t *testing.T) {
	sigs := loadSigs(t, sigFile(
		regexSig("declared", `timeout`, "keywords: [deadline]"),
		regexSig("derived", `(?i)connection reset`),
	), "declared", "derived")
	res := scanOne(t, lines("timeout reached", "DEADLINE: timeout", "CONNECTION RESET by peer", "Connection Reset"), sigs, Options{})
	if got := spans(res, "declared"); !slices.Equal(got, []lineSpan{{2, 2}}) {
		t.Errorf("declared keywords: findings %v, want line 2 only", got)
	}
	if got := spans(res, "derived"); !slices.Equal(got, []lineSpan{{3, 3}, {4, 4}}) {
		t.Errorf("derived keywords: findings %v, want lines 3 and 4", got)
	}
}

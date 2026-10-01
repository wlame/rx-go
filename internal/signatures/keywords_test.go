package signatures

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The clauses derived from an anchor regex: each clause is a set of
// lowered literals of at least 3 bytes, and every line the regex matches
// holds a member of every clause.
func TestDeriveClauses(t *testing.T) {
	cases := []struct {
		label string
		regex string
		want  [][]string
	}{
		{"one literal", `^Traceback \(most recent call last\):`, [][]string{{"traceback (most recent call last):"}}},
		{"two literals around a class", `(?i)\bdelete\s+from`, [][]string{{"delete"}, {"from"}}},
		{"case-sensitive literals are lowered", `\bUPDATE\s+\S+\s+SET\b`, [][]string{{"update"}, {"set"}}},
		{"a member under 3 bytes drops the clause", `foo|ba`, nil},
		{"a factored alternation is combined", `(AKIA|ASIA)[A-Z2-7]{16}`, [][]string{{"akia", "asia"}}},
		{"a factored alternation with a class in a branch", `(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}`,
			[][]string{{"a3t", "akia", "asia", "abia", "acca"}}},
		{"classes and repeats only", `\d+\.\d+`, nil},
		// A branch that starts with a class has no prefix to join the
		// literal before it: "foobar" would miss "foo7bar".
		{"only a prefix set joins a literal on its left", `foo(?:[0-9]bar|baz)`, [][]string{{"foo"}, {"bar", "baz"}}},
		{"only a suffix set joins a literal on its right", `(?:bar[0-9]|baz)foo`, [][]string{{"bar", "baz"}, {"foo"}}},
		{"a fold literal and a plain one join", `[Ee]rror`, [][]string{{"error"}}},
		{"an empty-width assertion keeps literals adjacent", `\bERROR\b:`, [][]string{{"error:"}}},
		{"a list compiles to an alternation", `(?:panic: )|(?:fatal error: )`, [][]string{{"panic: ", "fatal error: "}}},
		{"an alternation needs a clause in every branch", `(?:timeout|\d+)`, nil},
		{"a plus keeps its clauses and its suffix", `(?:abc)+x`, [][]string{{"abcx"}}},
		{"a star gives nothing", `(?:abc)*x`, nil},
		{"a question mark gives nothing", `(?:abc)?x`, nil},
		{"a repeat with a minimum keeps its clauses", `(?:abc){2,}`, [][]string{{"abc"}}},
		{"a repeat without a minimum gives nothing", `(?:abc){0,3}`, nil},
		{"the best clause of a branch is chosen", `(?:ab\s+longword|other)`, [][]string{{"longword", "other"}}},
		{"an implied clause is dropped", `(?:xyz)(?:qwe|rty)\s(?:qwe|rty)`, [][]string{{"xyzqwe", "xyzrty"}}},
		{"a non-ASCII fold rune splits the literal", `(?i)caféteria`, [][]string{{"caf"}, {"teria"}}},
		{"a non-ASCII rune without case stays", `ошибка`, [][]string{{"ошибка"}}},
		{"the replacement rune splits the literal", "abc�def", [][]string{{"abc"}, {"def"}}},
		{"a long literal stays whole", `0123456789012345678901234567890123456789`, [][]string{{"0123456789012345678901234567890123456789"}}},
		{"a product over 32 bytes is not combined", `(?:ab|cd)0123456789012345678901234567890`,
			[][]string{{"0123456789012345678901234567890"}}},
		{"a product over 64 literals is not combined",
			`(?:aa|bb|cc|dd|ee|ff|gg|hh)(?:ii|jj|kk|ll|mm|nn|oo|pp|qq)xyz`,
			[][]string{{"iixyz", "jjxyz", "kkxyz", "llxyz", "mmxyz", "nnxyz", "ooxyz", "ppxyz", "qqxyz"}}},
		{"no anchor text at all", `.*`, nil},
		// The keyword of (?i)select is select, though the regex also
		// matches ſelect: the scan tests every anchor on a line that
		// holds the long s or the Kelvin sign, so no clause needs them.
		{"a case-insensitive s stays an ASCII keyword", `(?i)select`, [][]string{{"select"}}},
	}
	for _, tc := range cases {
		got, err := deriveClauses(tc.regex)
		if err != nil {
			t.Errorf("%s: %v", tc.label, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %q gives %q, want %q", tc.label, tc.regex, got, tc.want)
		}
	}
}

// A regex the parser refuses is an error, not a missing gate.
func TestDeriveClauses_BadRegex(t *testing.T) {
	if _, err := deriveClauses(`(foo`); err == nil {
		t.Fatal("an unbalanced group gave no error")
	}
}

// FuzzDeriveClauses_Sound checks the one property the gate rests on:
// a line the regex matches holds a member of every derived clause,
// compared ASCII case-insensitively.
//
// A case-insensitive k or s also matches the Kelvin sign (U+212A) and
// the long s (U+017F), which ASCII folding never equates with them. The
// clauses hold the ASCII letters only, and the scan tests every anchor
// on a line that holds either rune without looking at the clauses, so
// such lines are not checked here.
func FuzzDeriveClauses_Sound(f *testing.F) {
	seeds := []struct{ regex, line string }{
		{`(?i)\bdelete\s+from`, "x DELETE   FROM t"},
		{`(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}`, "key A3TXIOSFODNN7EXAMPLE end"},
		{`foo(?:[0-9]bar|baz)`, "foo7bar"},
		{`[Ee]rror`, "ERROR"},
		{`(?i)caféteria`, "CAFÉTERIA"},
		{`(?:a|b)(?:c|d)xyz`, "adxyz"},
		{`\bERROR\b:`, "ERROR: x"},
		{"abc�def", "abc\xffdef"},
		{`(?:xyz)(?:qwe|rty)\s(?:qwe|rty)`, "xyzrty qwe"},
	}
	for _, s := range seeds {
		f.Add(s.regex, s.line)
	}
	f.Fuzz(func(t *testing.T, regex, line string) {
		re, err := regexp.Compile(regex)
		if err != nil || !re.MatchString(line) {
			return
		}
		if strings.ContainsRune(line, 'K') || strings.ContainsRune(line, 'ſ') {
			return
		}
		clauses, err := deriveClauses(regex)
		if err != nil {
			t.Fatalf("%q compiles but the walk refused it: %v", regex, err)
		}
		lowered := asciiLower(line)
		for _, clause := range clauses {
			if !clauseHasMember(clause, lowered) {
				t.Fatalf("%q matches %q, which holds no member of the clause %q", regex, line, clause)
			}
		}
	})
}

// clauseHasMember reports whether the lowered line holds a member of
// the clause.
func clauseHasMember(clause []string, loweredLine string) bool {
	for _, member := range clause {
		if strings.Contains(loweredLine, member) {
			return true
		}
	}
	return false
}

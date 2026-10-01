package sigscan

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
)

// The gate agrees with a naive check on random keywords and lines: a
// signature passes when every clause has a member in the line, compared
// ASCII case-insensitively, overlapping and suffix keywords included.
func TestGate_AgreesWithANaiveSearch(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	alphabet := []string{"a", "b", "A", "B", "c", "\xc3\xa9", " "}
	word := func(minLen, maxLen int) string {
		var b strings.Builder
		for range minLen + r.IntN(maxLen-minLen+1) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		return b.String()
	}
	for round := range 200 {
		var sigs []*signatures.Signature
		for range 1 + r.IntN(6) {
			sig := &signatures.Signature{}
			for range 1 + r.IntN(3) {
				var clause []string
				for range 1 + r.IntN(3) {
					clause = append(clause, string(bytes.ToLower([]byte(word(3, 5)))))
				}
				sig.Clauses = append(sig.Clauses, clause)
			}
			sigs = append(sigs, sig)
		}
		g, err := newGate(sigs)
		if err != nil {
			t.Fatal(err)
		}
		s := g.newScratch()
		for range 50 {
			line := word(0, 40)
			g.evaluate([]byte(line), s)
			lowered := strings.Map(func(c rune) rune {
				if 'A' <= c && c <= 'Z' {
					return c + 'a' - 'A'
				}
				return c
			}, line)
			for si, sig := range sigs {
				want := true
				for _, clause := range sig.Clauses {
					want = want && slices.ContainsFunc(clause, func(m string) bool { return strings.Contains(lowered, m) })
				}
				if got := g.passes(int32(si), s); got != want {
					t.Fatalf("round %d: line %q, clauses %q: gate %v, want %v", round, line, sig.Clauses, got, want)
				}
			}
		}
	}
}

// A signature without clauses always passes; the gate of a selection
// without keywords builds no automaton.
func TestGate_NoClausesPassEveryLine(t *testing.T) {
	g, err := newGate([]*signatures.Signature{{}, {Clauses: [][]string{{"error"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s := g.newScratch()
	g.evaluate([]byte("nothing here"), s)
	if !g.passes(0, s) || g.passes(1, s) {
		t.Errorf("passes: %v %v, want true false", g.passes(0, s), g.passes(1, s))
	}
	g.evaluate([]byte("an ERROR"), s)
	if !g.passes(1, s) {
		t.Error("an upper-case keyword does not pass")
	}
}

// splitLines cuts text into lines with the splitter, pushing it in
// pieces of size bytes, and returns each line as "view|length|start|next".
func splitLines(text string, size, lineBytes int, limit int64) []string {
	var out []string
	sp := newSplitter(lineBytes, 0, limit, func(l *line) bool {
		out = append(out, fmt.Sprintf("%s|%d|%d|%d", l.view, l.length, l.start, l.next))
		return true
	})
	for i := 0; i < len(text); i += size {
		if !sp.push([]byte(text[i:min(len(text), i+size)])) {
			return out
		}
	}
	sp.finish()
	return out
}

// The splitter gives the same lines however the text is pushed: the
// "\r" of a CRLF line dropped, a byte-order mark dropped from line 1,
// views cut, a last line without a newline kept, lines after the limit
// not given.
func TestSplitter_LinesWhateverThePieces(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		limit int64
		want  []string
	}{
		{"crlf and no final newline", "ab\r\n\r\ncd", noLimit, []string{"ab|2|0|4", "|0|4|6", "cd|2|6|8"}},
		{"a final \\r without a newline stays", "ab\r", noLimit, []string{"ab\r|3|0|3"}},
		{"byte-order mark", "\xEF\xBB\xBFab\nc\n", noLimit, []string{"ab|2|0|6", "c|1|6|8"}},
		{"long line cut", "abcdefghij\nk\n", noLimit, []string{"abcd|10|0|11", "k|1|11|13"}},
		{"long CRLF line keeps its length without the \\r", "abcdefghij\r\nk\n", noLimit, []string{"abcd|10|0|12", "k|1|12|14"}},
		{"limit", "a\nb\nc\n", 2, []string{"a|1|0|2", "b|1|2|4"}},
	}
	for _, tc := range cases {
		for _, size := range []int{1, 2, 3, 100} {
			if got := splitLines(tc.text, size, 4, tc.limit); !slices.Equal(got, tc.want) {
				t.Errorf("%s, pieces of %d: %q, want %q", tc.name, size, got, tc.want)
			}
		}
	}
}

// Each validator, with a valid and an invalid value.
func TestValidators(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"luhn", "4111 1111 1111 1111", true},
		{"luhn", "4111-1111-1111-1112", false},
		{"luhn", "41x1", false},
		{"iban", "GB82 WEST 1234 5698 7654 32", true},
		{"iban", "de89370400440532013000", true},
		{"iban", "GB82WEST12345698765433", false},
		{"iban", "XX82WEST12345698765432", false},
		{"iban", "GB82WEST1234569876543", false},
		{"ipv4", "10.0.0.255", true},
		{"ipv4", "10.0.0.256", false},
		{"ipv4", "::1", false},
		{"ipv6", "2001:db8::1", true},
		{"ipv6", "::ffff:10.0.0.1", true},
		{"ipv6", "10.0.0.1", false},
		{"ipv6", "2001:db8:::1", false},
	}
	for _, tc := range cases {
		if got := validators[tc.name]([]byte(tc.value)); got != tc.want {
			t.Errorf("%s(%q) = %v, want %v", tc.name, tc.value, got, tc.want)
		}
	}
}

// Shannon entropy in bits per byte.
func TestShannonEntropy(t *testing.T) {
	for value, want := range map[string]float64{"": 0, "aaaa": 0, "abab": 1, "abcd": 2} {
		if got := shannonEntropy([]byte(value)); got != want {
			t.Errorf("entropy(%q) = %v, want %v", value, got, want)
		}
	}
}

// The mask keeps 4 characters and replaces the rest with at most 12
// '*'.
func TestMaskValue(t *testing.T) {
	for value, want := range map[string]string{
		"abc":                  "abc",
		"abcdef":               "abcd**",
		"AKIAIOSFODNN7EXAMPLE": "AKIA************",
		"ééééé":                "éééé*",
	} {
		if got := string(maskValue([]byte(value))); got != want {
			t.Errorf("mask(%q) = %q, want %q", value, got, want)
		}
	}
}

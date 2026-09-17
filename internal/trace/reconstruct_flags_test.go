package trace

import (
	"slices"
	"testing"
)

// A cache hit rebuilds submatches by re-running the pattern, so the
// rebuilt text has to be the text rg matched under the same flags.
func TestSubmatchesFromPattern_HonorsRipgrepMatchingFlags(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		line    string
		flags   []string
		want    []string
	}{
		{"fixed string", "foo(", "call foo( now", []string{"-F"}, []string{"foo("}},
		{"case-insensitive", "error", "ERROR and Error", []string{"-i"}, []string{"ERROR", "Error"}},
		{"whole line", "a|ab", "ab", []string{"-x"}, []string{"ab"}},
		{"whole word", "err", "error err", []string{"-w"}, []string{"err"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs, _ := submatchesFromPattern(tc.pattern, tc.line, matchFlagsFrom(tc.flags), 100)

			got := make([]string, 0, len(subs))
			for _, s := range subs {
				got = append(got, s.Text)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("submatch texts = %v, want %v", got, tc.want)
			}
		})
	}
}

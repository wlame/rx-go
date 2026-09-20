package trace

import (
	"slices"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ripgrep reports that a line matched, never which -e pattern matched it,
// so identification re-runs every pattern in Go. These cases are lines rg
// does report under a matching flag; identification has to credit at
// least the pattern that produced each one, or the match is dropped.
func TestIdentifyMatchingPatterns_HonorsRipgrepMatchingFlags(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		submatch  string
		patterns  map[string]string
		flags     []string
		wantFound []string
	}{
		{
			name:      "fixed string that is not a valid regex",
			line:      "call foo( now",
			submatch:  "foo(",
			patterns:  map[string]string{"p1": "foo("},
			flags:     []string{"-F"},
			wantFound: []string{"p1"},
		},
		{
			name:      "fixed string whose dot is literal",
			line:      "a.b and axb",
			submatch:  "a.b",
			patterns:  map[string]string{"p1": "a.b", "p2": "axb"},
			flags:     []string{"--fixed-strings"},
			wantFound: []string{"p1"},
		},
		{
			name:      "whole line chooses the longer alternative",
			line:      "ab",
			submatch:  "ab",
			patterns:  map[string]string{"p1": "a|ab"},
			flags:     []string{"-x"},
			wantFound: []string{"p1"},
		},
		{
			name:      "whole word chooses the longer alternative",
			line:      "ab cd",
			submatch:  "ab",
			patterns:  map[string]string{"p1": "a|ab"},
			flags:     []string{"-w"},
			wantFound: []string{"p1"},
		},
		{
			name:      "case-insensitive fixed string",
			line:      "CALL FOO( NOW",
			submatch:  "FOO(",
			patterns:  map[string]string{"p1": "foo("},
			flags:     []string{"-i", "-F"},
			wantFound: []string{"p1"},
		},
		{
			name:      "a pattern Go cannot compile is credited when nothing else explains the line",
			line:      "error two",
			submatch:  "error",
			patterns:  map[string]string{"p1": "error(?= two)"},
			flags:     []string{"-P"},
			wantFound: []string{"p1"},
		},
		{
			name:      "only the pattern Go cannot compile can explain the line",
			line:      "an error",
			submatch:  "error",
			patterns:  map[string]string{"p1": "zzz", "p2": "(?<=an )error"},
			flags:     []string{"-P"},
			wantFound: []string{"p2"},
		},
		{
			name:      "a pattern Go reproduces is enough on its own",
			line:      "the error",
			submatch:  "error",
			patterns:  map[string]string{"p1": "error", "p2": "(?<=an )error"},
			flags:     []string{"-P"},
			wantFound: []string{"p1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order := make([]string, 0, len(tc.patterns))
			for id := range tc.patterns {
				order = append(order, id)
			}
			slices.Sort(order)
			subs := []rxtypes.Submatch{{Text: tc.submatch}}

			got := IdentifyMatchingPatterns(tc.line, subs, tc.patterns, order, tc.flags)

			if !slices.Equal(got, tc.wantFound) {
				t.Errorf("IdentifyMatchingPatterns = %v, want %v", got, tc.wantFound)
			}
		})
	}
}

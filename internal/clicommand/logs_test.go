package clicommand

import "testing"

// The MISSING cell shows the first three names and how many more are
// missing in all: the count, not the names the answer carries, which
// stop at a limit.
func TestMissingCell(t *testing.T) {
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = "x.log." + string(rune('a'+i%26))
	}
	cases := []struct {
		label string
		names []string
		count int
		want  string
	}{
		{"none", []string{}, 0, "-"},
		{"two", []string{"x.2", "x.3"}, 2, "x.2, x.3"},
		{"four", []string{"x.2", "x.3", "x.4", "x.5"}, 4, "x.2, x.3, x.4 and 1 more"},
		{"more than the answer names", hundred, 150, "x.log.a, x.log.b, x.log.c and 147 more"},
	}
	for _, tc := range cases {
		if got := missingCell(tc.names, tc.count); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.label, got, tc.want)
		}
	}
}

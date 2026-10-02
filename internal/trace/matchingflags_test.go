package trace

import (
	"slices"
	"testing"
)

// The CLI and the HTTP API both turn a set of flag names into ripgrep
// arguments through RipgrepArgs, so the two surfaces send ripgrep the same
// thing for the same request, and the trace-cache key does not depend on
// the order the flags were given in.
func TestRipgrepArgs_FollowsTableOrderWhateverTheSelection(t *testing.T) {
	selected := map[string]bool{"pcre2": true, "ignore-case": true, "line-regexp": true}

	got := RipgrepArgs(selected)

	want := []string{"-i", "-x", "-P"}
	if !slices.Equal(got, want) {
		t.Errorf("RipgrepArgs = %q, want %q", got, want)
	}
}

func TestRipgrepArgs_IgnoresFlagsThatAreOff(t *testing.T) {
	got := RipgrepArgs(map[string]bool{"ignore-case": false, "word-regexp": true})

	if !slices.Equal(got, []string{"-w"}) {
		t.Errorf("RipgrepArgs = %q, want [-w]", got)
	}
}

func TestRipgrepArgs_ReturnsAnEmptyListWhenNothingIsSet(t *testing.T) {
	got := RipgrepArgs(nil)

	if got == nil || len(got) != 0 {
		t.Errorf("RipgrepArgs(nil) = %#v, want an empty, non-nil list", got)
	}
}

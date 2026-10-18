package trace

import "testing"

// The trace-cache key knows the matching flags by the short spellings
// the searches emit, and only those: a long spelling no rx input holds
// is ignored like any unknown flag, and the keys of the five flags are
// what they were when the set was a second literal table.
func TestComputePatternsHash_MatchingFlagsComeFromTheOneTable(t *testing.T) {
	plain := ComputePatternsHash([]string{"alpha"}, nil)
	for _, flag := range []string{"-i", "-w", "-x", "-F", "-P"} {
		if ComputePatternsHash([]string{"alpha"}, []string{flag}) == plain {
			t.Errorf("%s does not change the key", flag)
		}
	}
	for _, flag := range []string{"--ignore-case", "--case-sensitive", "--json"} {
		if got := ComputePatternsHash([]string{"alpha"}, []string{flag}); got != plain {
			t.Errorf("%s changes the key: %s vs %s", flag, got, plain)
		}
	}
	if got := ComputePatternsHash([]string{"alpha"}, []string{"-i"}); got != "c99b10e8f303f1b9" {
		t.Errorf("key with -i = %s, want the pinned value", got)
	}
	if got := ComputePatternsHash([]string{"alpha"}, []string{"-P", "-w"}); got != "3e7b21a05e64fd1f" {
		t.Errorf("key with -P -w = %s, want the pinned value", got)
	}
}

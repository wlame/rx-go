package clicommand

import (
	"bytes"
	"testing"
)

// --before and --after override --context when they are given, and a
// value of 0 is given: it asks for no lines on that side. The HTTP route
// reads before_context=0 the same way, so a command and a request that
// say the same thing get the same answer.
func TestSamples_ExplicitZeroBeforeOrAfterOverridesContext(t *testing.T) {
	path := multiValueFixture(t)

	cases := []struct {
		name       string
		args       []string
		wantBefore float64
		wantAfter  float64
	}{
		{"before zero", []string{"--context=2", "--before=0"}, 0, 2},
		{"after zero", []string{"--context=2", "--after=0"}, 2, 0},
		{"both given", []string{"--context=2", "--before=1", "--after=0"}, 1, 0},
		{"neither given", []string{"--context=2"}, 2, 2},
		{"no flags", nil, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := NewSamplesCommand(&out)
			cmd.SetArgs(append([]string{path, "--lines=3", "--json"}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			answer := decodeSamples(t, out.Bytes())
			if answer["before_context"] != tc.wantBefore || answer["after_context"] != tc.wantAfter {
				t.Errorf("before_context=%v after_context=%v, want %v and %v",
					answer["before_context"], answer["after_context"], tc.wantBefore, tc.wantAfter)
			}
		})
	}
}

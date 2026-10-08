package clicommand

import (
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

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

// The PARTS cell counts the parts the answer lists, with how many of
// them cannot be read, and says ">10000"
// for a chain of more parts than are read as one text, whose answer
// lists none.
func TestPartsCell(t *testing.T) {
	if got := partsCell(rxtypes.ChainEntry{Parts: []string{"x.log.1", "x.log"}}); got != "2" {
		t.Errorf("two parts: %q", got)
	}
	unreadable := rxtypes.ChainEntry{Parts: []string{"x.log.2", "x.log.1", "x.log"}, Unreadable: []string{"x.log.2"}}
	if got := partsCell(unreadable); got != "3 (1 unreadable)" {
		t.Errorf("an unreadable part: %q", got)
	}
	if got := partsCell(rxtypes.ChainEntry{Parts: []string{}, TooManyParts: true}); got != ">10000" {
		t.Errorf("too many parts: %q", got)
	}
}

// A chain of more parts than are read as one text lists none, and its
// first line says how many its files' names give instead of "0 parts".
func TestWriteChainDescription_TooManyParts(t *testing.T) {
	resp := &rxtypes.ChainResponse{
		Path: "/var/log/x.log", State: rxtypes.ChainStateInvalid, Fingerprint: "0123456789abcdef",
		Parts: []rxtypes.ChainPart{},
		Reasons: []rxtypes.ChainReason{{Code: rxtypes.ChainReasonTooManyParts, Parts: []string{},
			Message: "the names of the chain's files give 20001 parts; at most 10000 are read as one text"}},
	}
	var out strings.Builder
	writeChainDescription(&out, shownChain{resp: resp, namedParts: 20001}, time.UTC)
	first := strings.SplitN(out.String(), "\n", 2)[0]
	if first != "/var/log/x.log: invalid, too many parts (20001), fingerprint 0123456789abcdef, times in UTC" {
		t.Fatalf("first line %q", first)
	}
}

// rx logs index counts what it built in parts, the word the chain
// commands use, where rx index counts files: the summary, the line for
// nothing built and the skipped list.
func TestWriteLogsIndex_CountsParts(t *testing.T) {
	cases := []struct {
		label  string
		result indexBuildResult
		want   string
	}{
		{
			label:  "built",
			result: indexBuildResult{Indexed: []map[string]any{{}, {}}, TotalTime: 0.04},
			want:   "Indexed 2 parts in 0.0s\n",
		},
		{
			label: "nothing built",
			result: indexBuildResult{SkipReasons: []rxtypes.SkippedFile{
				{Path: "/var/log/app.log.1", Reason: "not text"},
			}},
			want: "No parts indexed.\nSkipped 1 parts:\n  /var/log/app.log.1: not text\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			var out strings.Builder
			if err := writeLogsIndex(&out, []logsIndexAnswer{{indexBuildResult: tc.result}}, true, false); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Errorf("output\n%q\nwant\n%q", out.String(), tc.want)
			}
		})
	}
}

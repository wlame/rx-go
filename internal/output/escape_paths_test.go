package output

import (
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A file name can hold terminal control sequences. The human output
// prints every path and reason with them written out, so listing such a
// file cannot recolor, clear or rewrite the user's terminal; only rx's
// own colors are raw escapes.
func TestHumanOutputEscapesControlsInPathsAndReasons(t *testing.T) {
	evil := "/tmp/\x1b[31mevil\x1b[0m.log"
	trace := &rxtypes.TraceResponse{
		RequestID:    "fixed",
		Path:         []string{evil},
		Patterns:     map[string]string{"p1": "error"},
		Files:        map[string]string{"f1": evil},
		Matches:      []rxtypes.Match{{Pattern: "p1", File: "f1", Offset: 0, AbsoluteLineNumber: 1, LineText: strPtr("error")}},
		SkippedFiles: []string{evil},
		SkipReasons:  []rxtypes.SkippedFile{{Path: evil, Reason: "cannot resolve symlink: \x1b]0;title\x07"}},
		ContextLines: map[string][]rxtypes.ContextLine{
			"p1:f1:0": {{RelativeLineNumber: 1, AbsoluteLineNumber: 1, LineText: "error", AbsoluteOffset: 0}},
		},
	}
	samples := &rxtypes.SamplesResponse{
		Path:    evil,
		Lines:   map[string]int64{"1": 0},
		Offsets: map[string]int64{},
		Samples: map[string][]string{"1": {"error"}},
	}
	for name, out := range map[string]string{
		"trace":   FormatTraceCLI(trace, TraceFormatOptions{ShowContext: true}),
		"samples": FormatSamplesCLI(samples, false, ""),
	} {
		if strings.ContainsAny(out, "\x1b\x07") {
			t.Errorf("%s output holds a raw control byte:\n%q", name, out)
		}
		if !strings.Contains(out, `/tmp/\x1b[31mevil\x1b[0m.log`) {
			t.Errorf("%s output does not show the escaped name:\n%q", name, out)
		}
	}
}

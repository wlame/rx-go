package samplesanswer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/isolatedcache"
)

// TestMain gives the package its own cache directory: ColdAndIndexed
// stores an index.
func TestMain(m *testing.M) {
	isolatedcache.Main(m)
}

// answer is a small samples-shaped document for the comparison tests.
type answer struct {
	Path       string              `json:"path"`
	Lines      map[string]int64    `json:"lines"`
	Samples    map[string][]string `json:"samples"`
	CLICommand *string             `json:"cli_command"`
}

func TestDifference_EqualAnswersAgree(t *testing.T) {
	a := answer{Path: "a.log", Lines: map[string]int64{"5": 40}, Samples: map[string][]string{"5": {"x"}}}
	b := answer{Path: "a.log", Lines: map[string]int64{"5": 40}, Samples: map[string][]string{"5": {"x"}}}
	if diff := Difference(a, b); diff != "" {
		t.Fatalf("equal answers differ: %s", diff)
	}
}

func TestDifference_IgnoresCLICommand(t *testing.T) {
	one, other := "rx samples a.log --lines=5", "rx samples a.log --lines=5 --context=3"
	a := answer{Path: "a.log", CLICommand: &one}
	b := answer{Path: "a.log", CLICommand: &other}
	if diff := Difference(a, b); diff != "" {
		t.Fatalf("cli_command was compared: %s", diff)
	}
}

func TestDifference_NamesTheFirstDifferentField(t *testing.T) {
	cases := []struct {
		name      string
		got, want answer
		where     string
	}{
		{
			"a number",
			answer{Lines: map[string]int64{"5": 41}},
			answer{Lines: map[string]int64{"5": 40}},
			"lines.5",
		},
		{
			"a sample line",
			answer{Samples: map[string][]string{"5": {"x", "y"}}},
			answer{Samples: map[string][]string{"5": {"x", "z"}}},
			"samples.5[1]",
		},
		{
			"a missing key",
			answer{Lines: map[string]int64{}},
			answer{Lines: map[string]int64{"5": 40}},
			"lines",
		},
		{
			"null against an empty list",
			answer{Samples: map[string][]string{"5": nil}},
			answer{Samples: map[string][]string{"5": {}}},
			"samples.5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := Difference(tc.got, tc.want)
			if !strings.HasPrefix(diff, tc.where+":") {
				t.Fatalf("difference = %q, want one at %s", diff, tc.where)
			}
		})
	}
}

func TestColdAndIndexed_RunsOnceWithoutAndOnceWithTheIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("LINE some text\n", 200)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stored []bool
	got := ColdAndIndexed(t, path, 256, func(t testing.TB) any {
		idx, _ := index.LoadForSource(path)
		stored = append(stored, idx != nil)
		return answer{Path: path}
	})
	if len(stored) != 2 || stored[0] || !stored[1] {
		t.Fatalf("index stored at each run = %v, want [false true]", stored)
	}
	if got.(answer).Path != path {
		t.Fatalf("returned answer = %+v, want the cold one", got)
	}
}

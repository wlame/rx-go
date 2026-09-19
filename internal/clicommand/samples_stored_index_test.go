package clicommand

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The line a samples lookup asks for in the stored-index tests, and the
// text that line holds in samplesIndexFixture.
const (
	storedIndexTargetLine = "50000"
	storedIndexTargetText = "log line 50000 with some padding here"
)

// saveShiftedIndex builds and stores a valid index for path, then moves
// every checkpoint's line number up by 1000. The index still describes
// the file as far as its identity goes, and its checkpoints stay in
// order, so a lookup that reads it answers with a line 1000 too early:
// a canary that shows whether the index was read.
func saveShiftedIndex(t *testing.T, path string) {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	for i := range idx.LineIndex {
		idx.LineIndex[i].LineNumber += 1000
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
}

// samplesTargetLine runs `rx samples` on path for the target line with
// no context and returns the text it answers, failing the test when the
// command fails.
func samplesTargetLine(t *testing.T, path string, extraArgs ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := NewSamplesCommand(&out)
	cmd.SetArgs(append([]string{path, "--lines=" + storedIndexTargetLine, "--context=0", "--json"}, extraArgs...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rx samples %v: %v", extraArgs, err)
	}
	var resp rxtypes.SamplesResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	sample := resp.Samples[storedIndexTargetLine]
	if len(sample) != 1 {
		t.Fatalf("sample for line %s: %q, want one line", storedIndexTargetLine, sample)
	}
	return sample[0]
}

// `--no-index` and RX_NO_INDEX neither build nor read a line index: the
// lookup streams the file, so an index that says something else about
// the file cannot change the answer.
func TestSamples_NoIndexNeverReadsTheStoredIndex(t *testing.T) {
	ways := []struct {
		name string
		env  string
		args []string
	}{
		{"flag", "", []string{"--no-index"}},
		{"environment", "1", nil},
	}
	for _, way := range ways {
		t.Run(way.name, func(t *testing.T) {
			path, _ := samplesIndexFixture(t, 60000)
			saveShiftedIndex(t, path)
			// The canary is live: a lookup that reads the index answers
			// from its shifted checkpoints.
			if got := samplesTargetLine(t, path); got == storedIndexTargetText {
				t.Fatalf("the default lookup did not read the shifted index (answered %q)", got)
			}

			t.Setenv("RX_NO_INDEX", way.env)
			if got := samplesTargetLine(t, path, way.args...); got != storedIndexTargetText {
				t.Errorf("line %s = %q, want %q: the index was read", storedIndexTargetLine, got, storedIndexTargetText)
			}
		})
	}
}

// storedIndexDamage is one way a stored index becomes unusable: cut
// short (a power loss or a full disk during a write), or unreadable.
type storedIndexDamage struct {
	name   string
	damage func(t *testing.T, cachePath string)
}

var storedIndexDamages = []storedIndexDamage{
	{"truncated", func(t *testing.T, cachePath string) {
		body, err := os.ReadFile(cachePath)
		if err != nil {
			t.Fatalf("read index: %v", err)
		}
		if err := os.WriteFile(cachePath, body[:300], 0o600); err != nil {
			t.Fatalf("truncate index: %v", err)
		}
	}},
	{"unreadable", func(t *testing.T, cachePath string) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its permissions")
		}
		if err := os.Chmod(cachePath, 0); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(cachePath, 0o600) })
	}},
}

// A damaged index is treated as absent, never as a reason to fail the
// lookup: with --no-index it is not read at all; by default it is
// rebuilt over for a file worth an index, and passed over for a file
// below the threshold, where no build runs.
func TestSamples_DamagedIndexIsTreatedAsAbsent(t *testing.T) {
	modes := []struct {
		name        string
		largeFileMB string
		args        []string
		wantRebuilt bool
	}{
		{name: "no index", largeFileMB: "1", args: []string{"--no-index"}},
		{name: "default, large file", largeFileMB: "1", wantRebuilt: true},
		{name: "default, below the threshold", largeFileMB: "100"},
	}
	for _, damage := range storedIndexDamages {
		for _, mode := range modes {
			t.Run(damage.name+"/"+mode.name, func(t *testing.T) {
				path, _ := samplesIndexFixture(t, 60000)
				built, err := index.Build(path, index.BuildOptions{})
				if err != nil {
					t.Fatalf("build index: %v", err)
				}
				cachePath, err := index.Save(built)
				if err != nil {
					t.Fatalf("save index: %v", err)
				}
				damage.damage(t, cachePath)
				t.Setenv("RX_LARGE_FILE_MB", mode.largeFileMB)

				if got := samplesTargetLine(t, path, mode.args...); got != storedIndexTargetText {
					t.Errorf("line %s = %q, want %q", storedIndexTargetLine, got, storedIndexTargetText)
				}
				rebuilt, err := index.LoadForSource(path)
				if mode.wantRebuilt && (err != nil || rebuilt == nil) {
					t.Errorf("the damaged index was not rebuilt: %v", err)
				}
			})
		}
	}
}

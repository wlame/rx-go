package clicommand

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A second lookup in a multi-gigabyte file is the case an index exists
// for, so `rx samples` builds one when the file is worth it and none is
// cached. rx-python has always done this; rx-go did not, so the same
// command left different state on disk and the first call cost very
// different amounts of time.
//
// What it must not do is change the answer. An index only makes an
// answer faster.

func samplesIndexFixture(t *testing.T, lines int) (path string, cache string) {
	t.Helper()
	dir := t.TempDir()
	cache = filepath.Join(dir, "cache")
	t.Setenv("RX_CACHE_DIR", cache)
	// A 1 MB threshold makes a modest fixture count as large, so the
	// test does not have to write a 50 MB file to exercise the rule.
	t.Setenv("RX_LARGE_FILE_MB", "1")
	// No early answer from the head of the file: every lookup takes the
	// path that builds the index first. The tests of the head set it.
	t.Setenv("RX_SAMPLES_HEAD_MB", "0")

	path = filepath.Join(dir, "big.log")
	var body bytes.Buffer
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&body, "log line %d with some padding here\n", n)
	}
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path, cache
}

func runSamplesLine(t *testing.T, path string, noIndex bool) string {
	t.Helper()
	var buf bytes.Buffer
	err := runSamples(&buf, samplesParams{
		path:       path,
		lines:      []string{"1000"},
		ctxLines:   1,
		jsonOutput: true,
		noIndex:    noIndex,
	})
	if err != nil {
		t.Fatalf("runSamples: %v", err)
	}
	return buf.String()
}

func TestSamples_BuildsAnIndexForALargeFile(t *testing.T) {
	path, _ := samplesIndexFixture(t, 60000)

	if idx, err := index.LoadForSource(path); err == nil && idx != nil {
		t.Fatal("the fixture already has an index")
	}

	runSamplesLine(t, path, false)

	idx, err := index.LoadForSource(path)
	if err != nil || idx == nil {
		t.Fatalf("no index was built: %v", err)
	}
	// Analysis is a different feature: nothing on this path reads its
	// output, and a full anomaly pass to answer one line is work nobody
	// asked for.
	if idx.AnalysisPerformed {
		t.Error("the index carries analysis, which samples does not need")
	}
}

func TestSamples_NoIndexBuildsNothing(t *testing.T) {
	path, _ := samplesIndexFixture(t, 60000)

	runSamplesLine(t, path, true)

	if idx, err := index.LoadForSource(path); err == nil && idx != nil {
		t.Error("--no-index still wrote an index")
	}
}

// A small file is not worth indexing, and neither backend indexes one.
func TestSamples_SmallFileIsNotIndexed(t *testing.T) {
	path, _ := samplesIndexFixture(t, 10)

	runSamplesLine(t, path, false)

	if idx, err := index.LoadForSource(path); err == nil && idx != nil {
		t.Error("a small file was indexed")
	}
}

// The index only changes how fast the answer is reached.
func TestSamples_TheIndexDoesNotChangeTheAnswer(t *testing.T) {
	path, _ := samplesIndexFixture(t, 60000)

	cold := runSamplesLine(t, path, true) // nothing built, nothing read
	warm := runSamplesLine(t, path, false)
	if idx, err := index.LoadForSource(path); err != nil || idx == nil {
		t.Fatalf("the second call built no index: %v", err)
	}
	reread := runSamplesLine(t, path, false) // now reading the index

	if cold != warm || warm != reread {
		t.Errorf("the answer changed\n no index: %s\n first:    %s\n indexed:  %s", cold, warm, reread)
	}
}

// runSamplesSpec runs `rx samples --json` for one --lines value with a
// context of 1 and returns the output.
func runSamplesSpec(t *testing.T, path, lines string, noIndex bool) string {
	t.Helper()
	var buf bytes.Buffer
	err := runSamples(&buf, samplesParams{
		path: path, lines: []string{lines}, ctxLines: 1, jsonOutput: true, noIndex: noIndex,
	})
	if err != nil {
		t.Fatalf("runSamples --lines=%s: %v", lines, err)
	}
	return buf.String()
}

// filesUnder returns the regular files under dir, which may not exist.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

// A lookup whose lines lie in the head of a large file is answered from
// the head, with the answer an index would give, and builds no index: a
// command cannot finish one in the background, so it leaves the index to
// `rx index` or a later lookup that needs one.
func TestSamples_AnswersFromTheHeadWithoutBuildingAnIndex(t *testing.T) {
	path, cache := samplesIndexFixture(t, 60000)
	t.Setenv("RX_SAMPLES_HEAD_MB", "1")

	early := runSamplesSpec(t, path, "1-10", false)

	if files := filesUnder(t, cache); len(files) != 0 {
		t.Fatalf("an answer from the head wrote to the cache: %v", files)
	}
	cold := runSamplesSpec(t, path, "1-10", true)
	if early != cold {
		t.Errorf("the answer from the head differs from the lookup without an index\n head: %s\n cold: %s", early, cold)
	}
	if _, err := index.Save(buildIndexForTest(t, path)); err != nil {
		t.Fatalf("store the index: %v", err)
	}
	if indexed := runSamplesSpec(t, path, "1-10", false); early != indexed {
		t.Errorf("the answer from the head differs from the indexed one\n head:    %s\n indexed: %s", early, indexed)
	}
}

// A lookup the head cannot answer builds the index first, as before.
func TestSamples_PastTheHeadBuildsTheIndex(t *testing.T) {
	for _, lines := range []string{"-1", "59000"} {
		t.Run(lines, func(t *testing.T) {
			path, _ := samplesIndexFixture(t, 60000)
			t.Setenv("RX_SAMPLES_HEAD_MB", "1")

			runSamplesSpec(t, path, lines, false)

			if idx, err := index.LoadForSource(path); err != nil || idx == nil {
				t.Fatalf("--lines=%s past the head built no index: %v", lines, err)
			}
		})
	}
}

// buildIndexForTest builds the line index of path or fails the test.
func buildIndexForTest(t *testing.T, path string) *rxtypes.UnifiedFileIndex {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("build the index of %s: %v", path, err)
	}
	return idx
}

package clicommand

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/index"
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

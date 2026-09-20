package clicommand

// `rx index --json` used to put `index_path` on every entry and
// rx-python did not, so a consumer that read it worked against one
// backend and raised KeyError against the other. rx-python now emits it
// too, and this list is the half of the agreement that lives here.
//
// The set is written out rather than compared against a running
// rx-python, because rx-python is not installed in this repo's test
// environment. rx-python asserts the same set from its side, in
// tests/test_index_json_key_parity.py — the two lists have to be edited
// together, which is the point.

import (
	"os"
	"sort"
	"testing"
)

// expectedIndexEntryKeys is what an entry of `indexed` carries for a
// plain text file indexed without --analyze. `line_length` and
// `longest_line` are here because the line-length statistics come from
// the index build itself; the keys that really do need --analyze
// (anomalies, prefix patterns) and the compressed-source key
// (compression_format) are not.
var expectedIndexEntryKeys = []string{
	"analysis_performed",
	"build_time_seconds",
	"created_at",
	"empty_line_count",
	"file_type",
	"index_entries",
	"index_path",
	"line_count",
	"line_ending",
	"line_index",
	"line_length",
	"longest_line",
	"path",
	"size_bytes",
}

func TestIndex_JSONEntryCarriesTheAgreedKeySet(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := tinyLogFile(t)

	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})

	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 1 {
		t.Fatalf("indexed %d files, want 1: %v", len(indexed), result)
	}
	entry, ok := indexed[0].(map[string]any)
	if !ok {
		t.Fatalf("entry is %T, want an object", indexed[0])
	}

	got := make([]string, 0, len(entry))
	for key := range entry {
		got = append(got, key)
	}
	sort.Strings(got)

	if len(got) != len(expectedIndexEntryKeys) {
		t.Fatalf("key set is\n %v\nwant\n %v", got, expectedIndexEntryKeys)
	}
	for i, key := range got {
		if key != expectedIndexEntryKeys[i] {
			t.Fatalf("key set is\n %v\nwant\n %v", got, expectedIndexEntryKeys)
		}
	}
}

// The path reported must be the file that was written, not a path the
// caller has to guess at.
func TestIndex_JSONIndexPathPointsAtTheFileThatWasWritten(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := tinyLogFile(t)

	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})

	entry := result["indexed"].([]any)[0].(map[string]any)
	indexPath, _ := entry["index_path"].(string)
	if indexPath == "" {
		t.Fatal("index_path is empty")
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Errorf("index_path %q: %v", indexPath, err)
	}
}

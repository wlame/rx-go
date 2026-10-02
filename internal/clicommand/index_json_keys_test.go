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
// together, which is the point. `time_index` is the one key rx-python's
// list does not have yet: rx-go records a time section in every index,
// and rx-python, paused, has no such field.

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// expectedIndexEntryKeys is what an entry of `indexed` carries for a
// plain text file. `line_length` and `longest_line` are here because
// the line-length statistics come from the index build itself; the
// compressed-source key (compression_format) is not. The analysis keys
// (`analysis_performed`, `anomaly_count`, `anomaly_summary`,
// `anomalies`) went with contract 2.0, so rx-python's list still has
// `analysis_performed`.
var expectedIndexEntryKeys = []string{
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
	"time_index",
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

// removedAnalysisFields are the analysis fields of an index and its
// answers that contract 2.0 and index format 11 removed.
var removedAnalysisFields = []string{
	"analysis_performed",
	"analysis_window_lines",
	"analysis_detector_set",
	"anomalies",
	"anomaly_count",
	"anomaly_summary",
}

// `rx index --info --json` prints the stored index for one path and a
// summary per path for several; neither carries an analysis field.
func TestIndexInfo_JSONCarriesNoAnalysisField(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	first, second := tinyLogFile(t), tinyLogFile(t)
	zero := 0
	runIndexJSON(t, indexParams{paths: []string{first, second}, threshold: &zero})

	single := runIndexInfoJSON(t, first)
	requireNoAnalysisField(t, "info of one path", single)

	several := runIndexInfoJSON(t, first, second)
	files, _ := several["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files %v, want two entries", several["files"])
	}
	for _, file := range files {
		summary, _ := file.(map[string]any)["index"].(map[string]any)
		if summary == nil {
			t.Fatalf("no index summary in %v", file)
		}
		requireNoAnalysisField(t, "info of several paths", summary)
	}
}

// runIndexInfoJSON runs `rx index --info --json` on paths and decodes
// what it prints.
func runIndexInfoJSON(t *testing.T, paths ...string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := runIndex(&buf, indexParams{paths: paths, showInfo: true, jsonOutput: true}); err != nil {
		t.Fatalf("runIndex --info: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return decoded
}

// requireNoAnalysisField fails the test for each removed analysis
// field doc carries.
func requireNoAnalysisField(t *testing.T, label string, doc map[string]any) {
	t.Helper()
	for _, field := range removedAnalysisFields {
		if _, ok := doc[field]; ok {
			t.Errorf("%s carries %s", label, field)
		}
	}
}

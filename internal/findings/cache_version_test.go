package findings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A cache directory of the previous format version counts as absent:
// every selected signature is scanned again, and the directory then
// carries the current version.
func TestScan_APreviousCacheVersionIsAbsent(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
	opts := testOptions(t)
	dir := cacheDir(opts.Dir, file.Path())
	s, rec := newRecordedScheduler(1)
	mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line"), opts)
	rec.scanned()

	metaPath := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(metaPath) //nolint:gosec // the test's own cache
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["version"] != float64(2) {
		t.Fatalf("a fresh cache has version %v, want 2", meta["version"])
	}
	meta["version"] = 1
	data, _ = json.Marshal(meta)
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line"), opts)
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line", "warn-line"}}) {
		t.Fatalf("scanned %v, want both signatures again", scans)
	}
	requireSameFindings(t, got, coldAnswer(t, text, cat, "err-line", "warn-line"))
	readJSONFile(t, metaPath, &meta)
	if meta["version"] != float64(CacheVersion) {
		t.Errorf("version after the rescan %v, want %d", meta["version"], CacheVersion)
	}
}

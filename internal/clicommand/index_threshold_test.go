package clicommand

// `--threshold=0` meant "use the env default" here and "no threshold" in
// rx-python, so a script asking to index everything got everything from
// one backend and an empty `indexed` list with exit 0 from the other —
// a silence that reads like "there was nothing to do".
//
// rx-go's own HTTP surface already honored an explicit zero, so the
// same number meant two things inside one backend. Zero now means zero
// everywhere, and "use the default" is spelled by leaving the flag off.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// tinyLogFile writes a file far below any plausible size threshold.
func tinyLogFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiny.log")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// runIndexJSON runs the command and decodes its JSON wrapper.
func runIndexJSON(t *testing.T, p indexParams) map[string]any {
	t.Helper()
	p.jsonOutput = true
	var buf bytes.Buffer
	if err := runIndex(&buf, p); err != nil {
		t.Fatalf("runIndex: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return decoded
}

func TestIndex_ExplicitZeroThresholdIndexesEverything(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := tinyLogFile(t)

	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})

	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 1 {
		t.Fatalf("--threshold=0 indexed %d files, want 1; skipped: %v",
			len(indexed), result["skipped"])
	}
}

// Leaving the flag off is how "use the env default" is spelled, and the
// default is large enough that a three-line file is skipped.
func TestIndex_UnsetThresholdFallsBackToTheEnvironmentDefault(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := tinyLogFile(t)

	result := runIndexJSON(t, indexParams{paths: []string{path}})

	skipped, _ := result["skipped"].([]any)
	if len(skipped) != 1 {
		t.Fatalf("with no --threshold, skipped %d files, want 1; indexed: %v",
			len(skipped), result["indexed"])
	}
}

// A threshold small enough to be met still indexes, which is the case
// that would break if "0 means zero" were implemented by dropping the
// override entirely.
func TestIndex_ExplicitSmallThresholdStillApplies(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := tinyLogFile(t)

	oneMB := 1
	result := runIndexJSON(t, indexParams{paths: []string{path}, threshold: &oneMB})

	skipped, _ := result["skipped"].([]any)
	if len(skipped) != 1 {
		t.Fatalf("--threshold=1 on a 17-byte file skipped %d files, want 1", len(skipped))
	}
}

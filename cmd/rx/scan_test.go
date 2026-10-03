package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rx scan through the real binary: a bare `rx scan FILE` reaches the
// scan command (not a trace for the pattern "scan"), fills the cache,
// prints only its document with --json, and exits as rx index does: 0
// for a scanned file and for a directory, with --recursive or without
// (then its top level only), 3 for a missing file.
func TestScanCommand_ExitCodesAndJSONThroughTheBinary(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir, path := writeLog(t)

	code, stdout, stderr := runRx(t, "scan", "--json", path)
	if code != 0 {
		t.Fatalf("rx scan FILE: exit %d, stderr: %s", code, stderr)
	}
	assertOnlyJSON(t, "rx scan --json", stdout)
	var result struct {
		Files []struct {
			Path, Outcome string
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != path || result.Files[0].Outcome != "scanned" {
		t.Errorf("rx scan FILE: %s", stdout)
	}

	code, stdout, stderr = runRx(t, "scan", dir)
	if code != 0 || !strings.Contains(stdout, path+": ") {
		t.Errorf("rx scan DIR: exit %d, stdout: %s, stderr: %s", code, stdout, stderr)
	}
	if code, stdout, stderr = runRx(t, "scan", "-r", dir); code != 0 || !strings.Contains(stdout, path+": ") {
		t.Errorf("rx scan -r DIR: exit %d, stdout: %s, stderr: %s", code, stdout, stderr)
	}

	missing := filepath.Join(t.TempDir(), "nope.log")
	if code, _, stderr = runRx(t, "scan", missing); code != 3 {
		t.Errorf("rx scan MISSING: exit %d, stderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("RX_CACHE_DIR"), "rx", "findings")); err != nil {
		t.Errorf("rx scan wrote no findings cache under RX_CACHE_DIR: %v", err)
	}
}

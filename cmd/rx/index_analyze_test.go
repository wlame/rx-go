package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeAnomalousLog writes a log of about 20 KB whose ordinary lines
// read `LINE <n> ...`, with a Python traceback, a run of identical
// lines and a very long line in it, and returns its path.
func writeAnomalousLog(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filler := func(count int) {
		for i := 0; i < count; i++ {
			lines = append(lines, fmt.Sprintf("LINE %d request handled in %d ms", len(lines)+1, i%17))
		}
	}
	filler(60)
	lines = append(lines,
		"Traceback (most recent call last):",
		`  File "/srv/app/handler.py", line 42, in handle`,
		"    return payload['value'] / 0",
		"ZeroDivisionError: division by zero",
	)
	filler(40)
	for i := 0; i < 8; i++ {
		lines = append(lines, "LINE heartbeat ok")
	}
	filler(30)
	lines = append(lines, "", fmt.Sprintf("LINE %d %s", len(lines)+1, strings.Repeat("x", 3000)))
	filler(200)

	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// analysisKeys are the keys of an `rx index --json` entry that an
// analysis computes from the file's text, and so must be the same for
// a file and any compressed copy of it.
var analysisKeys = []string{
	"analysis_performed", "line_count", "empty_line_count", "line_ending",
	"line_length", "longest_line", "anomaly_count", "anomaly_summary", "anomalies",
}

// indexAnalyzeEntry runs `rx index --analyze --json path` and returns
// the one indexed entry.
func indexAnalyzeEntry(t *testing.T, env []string, path string) map[string]any {
	t.Helper()
	code, stdout, stderr := runRxEnv(t, env, "index", "--analyze", "--json", path)
	if code != 0 {
		t.Fatalf("rx index --analyze %s exited %d: %s", path, code, stderr)
	}
	var body struct {
		Indexed []map[string]any `json:"indexed"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("parse json: %v\n%s", err, stdout)
	}
	if len(body.Indexed) != 1 {
		t.Fatalf("indexed %d entries, want 1:\n%s", len(body.Indexed), stdout)
	}
	return body.Indexed[0]
}

// `rx index --analyze` on a seekable .zst made by `rx compress` gives
// the analysis of the decompressed text: the same statistics and the
// same anomalies, at the same line numbers, as the plain file. The
// answer is the same again when it comes from the cache.
func TestIndexAnalyzeOfSeekableZstdEqualsThePlainFile(t *testing.T) {
	dir := t.TempDir()
	env := []string{"RX_CACHE_DIR=" + t.TempDir()}
	plainPath := writeAnomalousLog(t, dir)
	zstPath := plainPath + ".zst"
	if code, _, stderr := runRxEnv(t, env, "compress", plainPath, "--frame-size=1K"); code != 0 {
		t.Fatalf("rx compress exited %d: %s", code, stderr)
	}

	plain := indexAnalyzeEntry(t, env, plainPath)
	cold := indexAnalyzeEntry(t, env, zstPath)
	warm := indexAnalyzeEntry(t, env, zstPath)

	if cold["file_type"] != "seekable_zstd" {
		t.Fatalf("file_type = %v, want seekable_zstd", cold["file_type"])
	}
	if count, _ := plain["anomaly_count"].(float64); count < 2 {
		t.Fatalf("the fixture should give several anomalies, got %v", plain["anomaly_summary"])
	}
	for _, key := range analysisKeys {
		if !reflect.DeepEqual(cold[key], plain[key]) {
			t.Errorf("%s: seekable %v, plain %v", key, cold[key], plain[key])
		}
		if !reflect.DeepEqual(warm[key], cold[key]) {
			t.Errorf("%s: cached %v, built %v", key, warm[key], cold[key])
		}
	}
	if warm["created_at"] != cold["created_at"] {
		t.Error("the second rx index --analyze rebuilt the analysis instead of reusing it")
	}
}

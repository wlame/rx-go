package webapi

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

// /health lists, under constants, the settings rx reads. A variable that
// nothing reads is not reported as a setting, even when it is set, so an
// operator is not told a tunable is in force when it does nothing.
func TestHealth_ConstantsListOnlySettingsRxReads(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	for _, name := range []string{"RX_MAX_FILES", "RX_MAX_LINE_SIZE_KB", "RX_DEBUG", "RX_DEBUG_DIR"} {
		t.Setenv(name, "1")
	}
	t.Setenv("NEWLINE_SYMBOL", `\n`)
	ts := newTestServer(t)

	_, _, body := get(t, ts.URL+"/health", "")

	var health struct {
		Constants   map[string]any    `json:"constants"`
		Environment map[string]string `json:"environment"`
	}
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	var keys []string
	for key := range health.Constants {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := []string{"CACHE_DIR", "LOG_LEVEL", "MAX_SUBPROCESSES", "MIN_CHUNK_SIZE_MB"}
	if !slices.Equal(keys, want) {
		t.Errorf("constants keys = %v, want %v", keys, want)
	}
	// environment echoes the RX_* variables as they are set, read or
	// not; NEWLINE_SYMBOL has no RX_ prefix and nothing reads it.
	if _, listed := health.Environment["NEWLINE_SYMBOL"]; listed {
		t.Errorf("environment lists NEWLINE_SYMBOL: %v", health.Environment)
	}
	if got := health.Environment["RX_MAX_FILES"]; got != "1" {
		t.Errorf("environment RX_MAX_FILES = %q, want the value as set", got)
	}
}

// /health reports the cache directory as an absolute path, also when
// RX_CACHE_DIR was given relative to the directory serve started in.
func TestHealth_CacheDirIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("RX_CACHE_DIR", "relcache")
	ts := newTestServer(t)

	_, _, body := get(t, ts.URL+"/health", "")

	var health struct {
		Constants map[string]any `json:"constants"`
	}
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	want, err := filepath.Abs(filepath.Join("relcache", "rx"))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if got := health.Constants["CACHE_DIR"]; got != want {
		t.Errorf("CACHE_DIR = %v, want %q", got, want)
	}
}

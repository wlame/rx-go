package clicommand

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
)

// captureCLILog sends slog's default logger, where rx's warnings go, to
// a buffer for the rest of the test.
func captureCLILog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// makeIndexDirReadOnly creates the index directory of the current
// RX_CACHE_DIR and takes away its write permission until the test ends.
func makeIndexDirReadOnly(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory whatever its permissions")
	}
	dir := config.GetIndexCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// pointCacheAtAFile sets RX_CACHE_DIR to a regular file.
func pointCacheAtAFile(t *testing.T) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", file)
}

// When the line index cannot be stored, `rx samples` answers by reading
// the file, as --no-index does, instead of building an index it would
// throw away; one warning per process says why.
func TestSamples_UnstorableIndexAnswersWithoutBuilding(t *testing.T) {
	cases := []struct {
		name   string
		breaks func(t *testing.T)
	}{
		{"read-only index directory", makeIndexDirReadOnly},
		{"cache directory is a regular file", pointCacheAtAFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := samplesIndexFixture(t, 60000)
			want := runSamplesLine(t, path, true)
			tc.breaks(t)
			log := captureCLILog(t)

			first := runSamplesLine(t, path, false)
			second := runSamplesLine(t, path, false)

			if first != want || second != want {
				t.Errorf("answers differ from --no-index\n want:   %s\n first:  %s\n second: %s", want, first, second)
			}
			logged := log.String()
			if got := strings.Count(logged, "index_not_stored"); got != 1 {
				t.Errorf("index_not_stored warnings: %d, want 1:\n%s", got, logged)
			}
			if got := strings.Count(logged, "level=WARN"); got != 1 {
				t.Errorf("warnings: %d, want 1:\n%s", got, logged)
			}
		})
	}
}

// A stored index of a file below the large-file size that cannot be read
// is replaced by the lookup that finds it, so the next lookup reads a
// good index and has nothing to warn about.
func TestSamples_DamagedIndexOfASmallFileIsReplaced(t *testing.T) {
	path, _ := samplesIndexFixture(t, 100)
	built, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	cachePath, err := index.Save(built)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte("{"), 0o600); err != nil {
		t.Fatalf("damage the index: %v", err)
	}
	want := runSamplesLine(t, path, true)
	log := captureCLILog(t)

	if got := runSamplesLine(t, path, false); got != want {
		t.Errorf("answer with a damaged index:\n got:  %s\n want: %s", got, want)
	}
	if stored, err := index.LoadForSource(path); err != nil || stored == nil {
		t.Fatalf("the damaged index was not replaced: %v", err)
	}
	log.Reset()
	if got := runSamplesLine(t, path, false); got != want {
		t.Errorf("answer with the replaced index:\n got:  %s\n want: %s", got, want)
	}
	if log.Len() != 0 {
		t.Errorf("the lookup after the rebuild still warns:\n%s", log.String())
	}
}

// `rx index` with RX_CACHE_DIR pointing at a regular file fails before
// it reads the log, with a message that names the cause.
func TestIndex_CacheDirectoryIsAFileFailsNamingTheCause(t *testing.T) {
	path, _ := samplesIndexFixture(t, 60000)
	pointCacheAtAFile(t)

	var out bytes.Buffer
	err := runIndex(&out, indexParams{paths: []string{path}, jsonOutput: true})

	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitGenericError {
		t.Fatalf("runIndex error = %v, want exit code %d", err, ExitGenericError)
	}
	var result struct {
		Errors []struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors: %+v, want one", result.Errors)
	}
	message := result.Errors[0].Error
	for _, part := range []string{"cannot store the line index", "not a directory"} {
		if !strings.Contains(message, part) {
			t.Errorf("error %q does not say %q", message, part)
		}
	}
}

// A log whose base name is too long to carry whole into a cache file
// name is indexed and its index is stored.
func TestIndex_LongBaseNameIsStored(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), strings.Repeat("n", 240)+".log")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})
	if indexed, _ := result["indexed"].([]any); len(indexed) != 1 {
		t.Fatalf("indexed: %v, want the log", result)
	}
	if stored, err := index.LoadForSource(path); err != nil || stored == nil {
		t.Fatalf("no stored index: %v", err)
	}
}

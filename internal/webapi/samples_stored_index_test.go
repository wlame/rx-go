package webapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The line the stored-index tests ask for, and the text it holds in
// storedIndexLog.
const (
	storedIndexLine = "50000"
	storedIndexText = "log line 50000 with some padding here"
)

// storedIndexLog writes a 60,000-line log into a fresh search root and
// returns its path as the server validates it, which is the path its
// index is stored under. A 1 MB threshold makes the log large enough to
// be worth an index.
func storedIndexLog(t *testing.T) string {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_NO_INDEX", "")
	root := t.TempDir()
	var text strings.Builder
	for n := 1; n <= 60000; n++ {
		fmt.Fprintf(&text, "log line %d with some padding here\n", n)
	}
	file := filepath.Join(root, "big.log")
	if err := os.WriteFile(file, []byte(text.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	validated, err := paths.ValidatePathWithinRoots(file)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return validated
}

// storeIndex builds and stores the index of path, with every checkpoint
// moved up by shift lines, and returns where it was stored. A shift
// keeps the checkpoints in order and the identity valid, so a lookup
// that reads the index answers shift lines too early.
func storeIndex(t *testing.T, path string, shift int64) string {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	for i := range idx.LineIndex {
		idx.LineIndex[i].LineNumber += shift
	}
	cachePath, err := index.Save(idx)
	if err != nil {
		t.Fatalf("save index: %v", err)
	}
	return cachePath
}

// sampleStoredIndexLine asks GET /v1/samples for the target line with
// no context and returns the status and the text answered for it.
func sampleStoredIndexLine(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	query := url.Values{"path": {path}, "lines": {storedIndexLine}, "context": {"0"}}
	resp, err := http.Get(ts.URL + "/v1/samples?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, string(body)
	}
	var answer rxtypes.SamplesResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if sample := answer.Samples[storedIndexLine]; len(sample) == 1 {
		return resp.StatusCode, sample[0]
	}
	return resp.StatusCode, fmt.Sprint(answer.Samples)
}

// Under RX_NO_INDEX, GET /v1/samples neither builds nor reads a line
// index, so an index that says something else about the file cannot
// change the answer.
func TestSamples_NoIndexEnvironmentNeverReadsTheStoredIndex(t *testing.T) {
	path := storedIndexLog(t)
	storeIndex(t, path, 1000)
	ts := newTestServer(t)

	// The canary is live: a lookup that reads the index answers from
	// its shifted checkpoints.
	if status, got := sampleStoredIndexLine(t, ts, path); status != http.StatusOK || got == storedIndexText {
		t.Fatalf("default lookup: %d %q; want 200 with the shifted index's answer", status, got)
	}

	t.Setenv("RX_NO_INDEX", "1")
	if status, got := sampleStoredIndexLine(t, ts, path); status != http.StatusOK || got != storedIndexText {
		t.Errorf("RX_NO_INDEX lookup: %d %q; want 200 %q", status, got, storedIndexText)
	}
}

// damageIndexFile makes a stored index unusable: cut short (a power
// loss or a full disk during a write), or unreadable.
var damageIndexFile = []struct {
	name   string
	damage func(t *testing.T, cachePath string)
}{
	{"truncated", func(t *testing.T, cachePath string) {
		body, err := os.ReadFile(cachePath)
		if err != nil {
			t.Fatalf("read index: %v", err)
		}
		if err := os.WriteFile(cachePath, body[:300], 0o600); err != nil {
			t.Fatalf("truncate index: %v", err)
		}
	}},
	{"unreadable", func(t *testing.T, cachePath string) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its permissions")
		}
		if err := os.Chmod(cachePath, 0); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(cachePath, 0o600) })
	}},
}

// A damaged index is treated as absent, never as a reason to answer 500:
// under RX_NO_INDEX it is not read; by default it is rebuilt over for a
// file worth an index, and passed over below the threshold, where no
// build runs.
func TestSamples_DamagedIndexIsTreatedAsAbsentOverHTTP(t *testing.T) {
	modes := []struct {
		name        string
		noIndex     string
		largeFileMB string
	}{
		{"RX_NO_INDEX", "1", "1"},
		{"default, large file", "", "1"},
		{"default, below the threshold", "", "100"},
	}
	for _, damage := range damageIndexFile {
		for _, mode := range modes {
			t.Run(damage.name+"/"+mode.name, func(t *testing.T) {
				path := storedIndexLog(t)
				damage.damage(t, storeIndex(t, path, 0))
				t.Setenv("RX_NO_INDEX", mode.noIndex)
				t.Setenv("RX_LARGE_FILE_MB", mode.largeFileMB)
				ts := newTestServer(t)

				if status, got := sampleStoredIndexLine(t, ts, path); status != http.StatusOK || got != storedIndexText {
					t.Errorf("lookup: %d %q; want 200 %q", status, got, storedIndexText)
				}
			})
		}
	}
}

// GET /v1/index answers a damaged index as it answers a missing one.
func TestIndexGet_DamagedIndexIsNotFound(t *testing.T) {
	for _, damage := range damageIndexFile {
		t.Run(damage.name, func(t *testing.T) {
			path := storedIndexLog(t)
			damage.damage(t, storeIndex(t, path, 0))
			ts := newTestServer(t)

			resp, err := http.Get(ts.URL + "/v1/index?" + url.Values{"path": {path}}.Encode())
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status %d, want 404", resp.StatusCode)
			}
		})
	}
}

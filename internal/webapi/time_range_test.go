package webapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timeRangeServer writes a timestamped log, its gzip copy, a log
// without timestamps, a binary file and a directory into a fresh
// search root and serves it. It returns the server's URL, the
// validated paths by name and the cache directory.
func timeRangeServer(t *testing.T) (string, map[string]string, string) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cache)
	t.Setenv("RX_NO_INDEX", "")
	t.Setenv("RX_LOG_TZ", "")
	root := t.TempDir()
	text := []byte("starting\n2025-12-10 07:00:04.574 INFO LINE 2\n2025-12-10 07:59:59.390 INFO LINE 3\n")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(text)
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	files := map[string][]byte{
		"app.log": text, "app.log.gz": gz.Bytes(), "notes.txt": []byte("alpha\nbeta\n"),
		"blob.bin": {0, 1, 2, 0, '\n'},
	}
	out := map[string]string{}
	for name, body := range files {
		file := filepath.Join(root, name)
		if err := os.WriteFile(file, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		out[name] = file
	}
	out["dir"] = filepath.Join(root, "dir")
	if err := os.Mkdir(out["dir"], 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	for name, file := range out {
		validated, err := paths.ValidatePathWithinRoots(file)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		out[name] = validated
	}
	ts := httptest.NewServer(NewServer(Config{AppVersion: "time-range-test", TaskManager: tasks.New(tasks.Config{})}))
	t.Cleanup(ts.Close)
	return ts.URL, out, cache
}

// getTimeRange sends GET /v1/time-range for path and returns the
// status and the body.
func getTimeRange(t *testing.T, base, path string) (int, []byte) {
	t.Helper()
	query := url.Values{}
	if path != "" {
		query.Set("path", path)
	}
	resp, err := http.Get(base + "/v1/time-range?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// rangeAnswer decodes a 200 answer.
func rangeAnswer(t *testing.T, status int, body []byte) rxtypes.TimeRangeResponse {
	t.Helper()
	var answer rxtypes.TimeRangeResponse
	if status != http.StatusOK || json.Unmarshal(body, &answer) != nil {
		t.Fatalf("status %d, body %s", status, body)
	}
	return answer
}

// A plain file is scanned, a gzip file without an index has no range,
// a file without timestamps has no format, and an index answers once
// it is stored; the request itself writes nothing to the cache.
func TestTimeRange_AnswersEachKindOfFile(t *testing.T) {
	base, files, cache := timeRangeServer(t)

	plain := fetchRange(t, base, files["app.log"])
	if plain.Source != "scan" || plain.FirstMs == nil || *plain.FirstMs != 1765350004574 ||
		plain.LastMs == nil || *plain.LastMs != 1765353599390 || plain.Example == nil ||
		*plain.Example != "2025-12-10 07:00:04.574" || plain.CLICommand != "rx time-range "+files["app.log"] {
		t.Fatalf("plain: %+v", plain)
	}
	gz := fetchRange(t, base, files["app.log.gz"])
	if gz.Source != "none" || gz.Format == nil || gz.FirstMs != nil || gz.LastMs != nil {
		t.Fatalf("gzip without an index: %+v", gz)
	}
	notes := fetchRange(t, base, files["notes.txt"])
	if notes.Format != nil || notes.Example != nil || notes.FirstMs != nil || notes.Source != "scan" {
		t.Fatalf("no format: %+v", notes)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatalf("the cache holds %v (%v); a time-range request writes nothing", entries, err)
	}

	idx, err := index.Build(files["app.log.gz"], index.BuildOptions{})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save: %v", err)
	}
	indexed := fetchRange(t, base, files["app.log.gz"])
	if indexed.Source != "index" || indexed.FirstMs == nil || *indexed.FirstMs != *plain.FirstMs ||
		indexed.LastMs == nil || *indexed.LastMs != *plain.LastMs || *indexed.Example != *plain.Example {
		t.Fatalf("gzip with an index: %+v", indexed)
	}
	t.Setenv("RX_NO_INDEX", "true")
	if again := fetchRange(t, base, files["app.log.gz"]); again.Source != "none" {
		t.Fatalf("under RX_NO_INDEX the index was read: %+v", again)
	}
}

// Errors are those of GET /v1/samples.
func TestTimeRange_Errors(t *testing.T) {
	base, files, _ := timeRangeServer(t)
	cases := []struct {
		name string
		path string
		want int
	}{
		{"a directory", files["dir"], http.StatusBadRequest},
		{"a binary file", files["blob.bin"], http.StatusBadRequest},
		{"outside the search root", "/etc/hosts", http.StatusForbidden},
		{"a missing file", filepath.Join(filepath.Dir(files["app.log"]), "nope.log"), http.StatusNotFound},
		{"no path", "", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := getTimeRange(t, base, tc.path); status != tc.want {
				t.Fatalf("status %d, want %d: %s", status, tc.want, body)
			}
		})
	}
}

// fetchRange sends GET /v1/time-range for path and decodes its 200.
func fetchRange(t *testing.T, base, path string) rxtypes.TimeRangeResponse {
	t.Helper()
	status, body := getTimeRange(t, base, path)
	return rangeAnswer(t, status, body)
}

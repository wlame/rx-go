package webapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainsRoot makes a search root with a directory of rotated logs, and
// installs it. The directory's name and the logs' names hold spaces,
// brackets, a plus sign and non-ASCII letters, which a query string
// carries percent-encoded.
func chainsRoot(t *testing.T) (root, logs string) {
	t.Helper()
	root = t.TempDir()
	logs = filepath.Join(root, "my logs [1] + журнал")
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write([]byte("2026-10-01 00:00:00 LINE 1\n"))
	_ = w.Close()
	files := map[string][]byte{
		"my app (1).log.1":    []byte("2026-10-02 00:00:00 LINE 2\n"),
		"my app (1).log.2.gz": compressed.Bytes(),
		"журнал.log":          []byte("2026-10-02 00:00:00 LINE 2\n"),
		"журнал.log.1":        []byte("2026-10-01 00:00:00 LINE 1\n"),
		"single.log":          []byte("LINE 1\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(logs, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	return root, logs
}

// getChains asks GET /v1/logs/chains with query and returns the status
// and the body.
func getChains(t *testing.T, base string, query url.Values) (int, []byte) {
	t.Helper()
	resp, err := http.Get(base + "/v1/logs/chains?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	return resp.StatusCode, body.Bytes()
}

// The route lists a directory's chains; names with spaces, brackets and
// non-ASCII letters come back as they are, and their handles go back
// into a query URL-encoded.
func TestLogChains_ListsTheChainsOfADirectory(t *testing.T) {
	_, logs := chainsRoot(t)
	ts := newTestServer(t)

	status, raw := getChains(t, ts.URL, url.Values{"path": {logs}})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var resp rxtypes.ChainsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Path != logs || len(resp.Chains) != 2 {
		t.Fatalf("answer %s", raw)
	}
	app, journal := resp.Chains[0], resp.Chains[1]
	if app.Name != "my app (1).log" || app.Path != filepath.Join(logs, "my app (1).log") ||
		!slices.Equal(app.Parts, []string{"my app (1).log.2.gz", "my app (1).log.1"}) || app.HasActive ||
		!slices.Equal(app.CompressionFormats, []string{"gzip"}) || len(app.Missing) != 0 || app.IsIndexed {
		t.Fatalf("first chain %+v", app)
	}
	if journal.Name != "журнал.log" || !slices.Equal(journal.Parts, []string{"журнал.log.1", "журнал.log"}) ||
		!journal.HasActive {
		t.Fatalf("second chain %+v", journal)
	}
	// The JSON arrays are never null.
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	first := generic["chains"].([]any)[0].(map[string]any)
	for _, key := range []string{"parts", "missing", "compression_formats"} {
		if _, ok := first[key].([]any); !ok {
			t.Fatalf("%s is %v, want an array", key, first[key])
		}
	}
}

// The errors are those of GET /v1/tree for the same path.
func TestLogChains_ErrorsAsTheTree(t *testing.T) {
	root, logs := chainsRoot(t)
	ts := newTestServer(t)
	cases := []struct {
		label string
		query url.Values
		want  int
	}{
		{"a file", url.Values{"path": {filepath.Join(logs, "single.log")}}, http.StatusBadRequest},
		{"outside the roots", url.Values{"path": {t.TempDir()}}, http.StatusForbidden},
		{"a hidden directory", url.Values{"path": {filepath.Join(root, ".hidden")}}, http.StatusForbidden},
		{"a missing directory", url.Values{"path": {filepath.Join(root, "absent")}}, http.StatusNotFound},
		{"no path", url.Values{}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		status, raw := getChains(t, ts.URL, tc.query)
		if status != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.label, status, tc.want, raw)
		}
		treeStatus := status
		if tc.query.Get("path") != "" {
			resp, err := http.Get(ts.URL + "/v1/tree?" + tc.query.Encode())
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			treeStatus = resp.StatusCode
		}
		if treeStatus != status {
			t.Errorf("%s: chains %d, tree %d", tc.label, status, treeStatus)
		}
	}
}

// /health lists log_chains, and the contract is 1.7.
func TestHealth_ListsLogChains(t *testing.T) {
	if !slices.Contains(Features(), "log_chains") {
		t.Errorf("features %v lack log_chains", Features())
	}
	if ContractVersion != "1.7" {
		t.Errorf("contract %s, want 1.7", ContractVersion)
	}
}

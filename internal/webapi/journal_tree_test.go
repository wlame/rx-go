package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// copyJournal writes the journal fixture name into dir under as and
// returns its path.
func copyJournal(t *testing.T, name, dir, as string) string {
	t.Helper()
	data, err := os.ReadFile(journalfixture.Journal(t, name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, as)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The tree lists a journal as a compressed text file in the journal
// format, whatever its name, and a journal rx cannot read as a file that
// is not text; samples, index and trace refuse that one with the
// journal's reason.
func TestTree_ListsJournalsByTheirHeaders(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	copyJournal(t, "basic-zstd-compact", root, "app.log")
	unreadable := copyJournal(t, "basic-none-regular", root, "system.journal")
	journalfixture.AddUnknownIncompatibleFlag(t, unreadable)
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/tree?path=" + url.QueryEscape(root))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/tree: %d", resp.StatusCode)
	}
	var tree rxtypes.TreeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entries := map[string]rxtypes.TreeEntry{}
	for _, e := range tree.Entries {
		entries[e.Name] = e
	}
	want := map[string]bool{"app.log": true, "system.journal": false}
	for name, isText := range want {
		e, ok := entries[name]
		if !ok {
			t.Fatalf("the tree does not list %s: %+v", name, tree.Entries)
		}
		if e.IsText == nil || *e.IsText != isText {
			t.Errorf("%s: is_text %v, want %v", name, e.IsText, isText)
		}
		if e.IsCompressed == nil || !*e.IsCompressed || e.CompressionFormat == nil || *e.CompressionFormat != "journal" {
			t.Errorf("%s: is_compressed %v, compression_format %v; want true, journal", name, e.IsCompressed, e.CompressionFormat)
		}
	}

	const reason = "not a text file: a systemd journal rx cannot read: incompatible flags"
	resp, err = http.Get(ts.URL + "/v1/samples?path=" + url.QueryEscape(unreadable) + "&lines=1&context=0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	requireRefusal(t, resp, http.StatusBadRequest, reason)

	zero := 0
	payload, _ := json.Marshal(rxtypes.IndexRequest{Path: unreadable, Threshold: &zero})
	resp, err = http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	requireRefusal(t, resp, http.StatusBadRequest, reason)

	resp, err = http.Get(ts.URL + "/v1/trace?regexp=service&path=" + url.QueryEscape(unreadable))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	requireRefusal(t, resp, http.StatusOK, reason)
}

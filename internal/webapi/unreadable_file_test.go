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
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A file the request names and the server may not read is refused the
// same way by every route: 403 with "Permission denied: <path>", as the
// CLI exits 4. GET /v1/trace used to answer 200 with the file skipped,
// GET /v1/samples 500, and POST /v1/index 400 "not a text file".
func TestUnreadableFileIsForbiddenOnEveryRoute(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	locked := filepath.Join(root, "locked.log")
	if err := os.WriteFile(locked, []byte("LINE 1 NEEDLE\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	ts := newTestServer(t)
	query := url.QueryEscape(locked)
	want := "Permission denied: " + locked

	for _, route := range []string{
		"/v1/trace?path=" + query + "&regexp=NEEDLE",
		"/v1/samples?path=" + query + "&lines=1",
	} {
		resp, err := http.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("get %s: %v", route, err)
		}
		requireRefusal(t, resp, http.StatusForbidden, want)
	}
	zero := 0
	payload, _ := json.Marshal(rxtypes.IndexRequest{Path: locked, Threshold: &zero})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	requireRefusal(t, resp, http.StatusForbidden, want)
}

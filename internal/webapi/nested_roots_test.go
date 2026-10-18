package webapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// GET /v1/samples serves a file of a hidden root beside its parent root.
func TestSamples_NestedHiddenRootServesItsFiles(t *testing.T) {
	a := t.TempDir()
	b := filepath.Join(a, ".b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(b, "x.log")
	if err := os.WriteFile(x, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := paths.SetSearchRoots([]string{a, b}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	paths.SetIncludeHidden(false)
	ts := newTestServer(t)
	status, _, body := get(t, fmt.Sprintf("%s/v1/samples?path=%s&lines=2&context=0", ts.URL, x), "")
	if status != http.StatusOK {
		t.Errorf("status %d: %s; want 200", status, body)
	}
}

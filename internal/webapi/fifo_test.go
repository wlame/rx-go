package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A request that names a named pipe is answered at once with 400 "Not a
// regular file", never left waiting on the pipe for a writer.
func TestNamedPipeIsRefusedOnEveryRouteWithoutBlocking(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	pipe := filepath.Join(root, "pipe.log")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	ts := newTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}
	want := "Not a regular file: " + pipe
	query := url.QueryEscape(pipe)

	for _, route := range []string{
		"/v1/trace?path=" + query + "&regexp=NEEDLE",
		"/v1/samples?path=" + query + "&lines=1",
	} {
		resp, err := client.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("get %s: %v", route, err)
		}
		requireRefusal(t, resp, http.StatusBadRequest, want)
	}
	zero := 0
	payload, _ := json.Marshal(rxtypes.IndexRequest{Path: pipe, Threshold: &zero})
	resp, err := client.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	requireRefusal(t, resp, http.StatusBadRequest, want)
}

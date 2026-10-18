package webapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/tasks"
)

// The cache's own files are not part of the viewer: a request for
// .metadata.json, for the download in flight or for a staging file
// answers the viewer's index page, as any unknown path does, while the
// bundle's own files are still served.
func TestStaticFiles_TheCachesOwnFilesAreNotServed(t *testing.T) {
	cacheDir := t.TempDir()
	if err := extractTestTarball(t, "testdata/rx-viewer-v0.2.0-dist.tar.gz", cacheDir); err != nil {
		t.Fatalf("extract fixture: %v", err)
	}
	for _, name := range []string{"version.json", ".metadata.json", "dist.tar.gz.tmp"} {
		if err := os.WriteFile(filepath.Join(cacheDir, name), []byte(`{"the cache's own": "`+name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, ".staging"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, ".staging", "index.html"), []byte("<html>staging</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(Config{
		AppVersion:  "test-0.0.0",
		Frontend:    frontend.NewManager(frontend.Config{CacheDir: cacheDir}),
		Hooks:       hooks.NewDispatcher(hooks.DispatcherConfig{}),
		TaskManager: tasks.New(tasks.Config{}),
	}))
	t.Cleanup(ts.Close)

	fetch := func(path string) (int, string, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
	}
	for _, path := range []string{"/.metadata.json", "/dist.tar.gz.tmp", "/.staging/index.html"} {
		status, contentType, body := fetch(path)
		if status != http.StatusOK || !strings.HasPrefix(contentType, "text/html") || strings.Contains(body, "the cache's own") || strings.Contains(body, "staging") {
			t.Errorf("GET %s: status %d, type %s, body %q; want the viewer's index page", path, status, contentType, body)
		}
	}
	if status, _, body := fetch("/favicon.svg"); status != http.StatusOK || !strings.Contains(body, "<svg") {
		t.Errorf("GET /favicon.svg: status %d, body %q; want the bundle's file", status, body)
	}
	if status, contentType, _ := fetch("/version.json"); status != http.StatusOK || !strings.Contains(contentType, "json") {
		t.Errorf("GET /version.json: status %d, type %s; want the bundle's file", status, contentType)
	}
}

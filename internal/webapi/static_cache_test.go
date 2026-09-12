package webapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/tasks"
)

// newViewerServer serves the v0.2.0 viewer bundle plus a version.json, the
// file a build writes beside index.html.
func newViewerServer(t *testing.T) *httptest.Server {
	t.Helper()
	cacheDir := t.TempDir()
	if err := extractTestTarball(t, "testdata/rx-viewer-v0.2.0-dist.tar.gz", cacheDir); err != nil {
		t.Fatalf("extract fixture: %v", err)
	}
	versionJSON := []byte(`{"version": "v0.2.0"}`)
	if err := os.WriteFile(filepath.Join(cacheDir, "version.json"), versionJSON, 0o600); err != nil {
		t.Fatalf("write version.json: %v", err)
	}
	srv := NewServer(Config{
		AppVersion:  "test-0.0.0",
		Frontend:    frontend.NewManager(frontend.Config{CacheDir: cacheDir}),
		Hooks:       hooks.NewDispatcher(hooks.DispatcherConfig{}),
		TaskManager: tasks.New(tasks.Config{}),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// A file whose name carries its content hash can be cached for good: a new
// build gives it a new name. A file that keeps its name across builds must
// be revalidated, or a browser shows the previous viewer's version.json for
// a year after an upgrade.
func TestStaticFiles_OnlyHashedAssetsAreCachedForGood(t *testing.T) {
	ts := newViewerServer(t)

	cases := []struct {
		path string
		want string
	}{
		{path: "/assets/index-D8mRyN95.js", want: "public, max-age=31536000, immutable"},
		{path: "/assets/index-BsAyAYl0.css", want: "public, max-age=31536000, immutable"},
		{path: "/version.json", want: "no-cache"},
		{path: "/favicon.svg", want: "no-cache"},
		{path: "/", want: "no-cache, no-store, must-revalidate"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control %q, want %q", got, tc.want)
			}
		})
	}
}

// Revalidating costs a 304 with no body when the file has not changed.
func TestStaticFiles_RevalidationAnswersNotModified(t *testing.T) {
	ts := newViewerServer(t)

	first, err := http.Get(ts.URL + "/version.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = first.Body.Close()
	lastModified := first.Header.Get("Last-Modified")
	if lastModified == "" {
		t.Fatal("no Last-Modified on version.json")
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/version.json", nil)
	req.Header.Set("If-Modified-Since", lastModified)
	second, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	_ = second.Body.Close()

	if second.StatusCode != http.StatusNotModified {
		t.Errorf("status %d, want 304", second.StatusCode)
	}
}

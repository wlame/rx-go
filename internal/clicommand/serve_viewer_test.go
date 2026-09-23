package clicommand

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/frontend"
)

// failingGitHub answers every request with 500 and counts the requests.
func failingGitHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// cachedViewerManager returns a manager over a cache holding viewer
// 0.2.0, last checked at lastCheck, that asks apiBase for releases.
func cachedViewerManager(t *testing.T, apiBase string, lastCheck time.Time) *frontend.Manager {
	t.Helper()
	t.Setenv("RX_FRONTEND_URL", "")
	t.Setenv("RX_FRONTEND_VERSION", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>0.2.0</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	fm := frontend.NewManager(frontend.Config{CacheDir: dir, APIBase: apiBase})
	md := &frontend.CacheMetadata{
		Version:   frontend.Version{Version: "0.2.0"},
		LastCheck: lastCheck.UTC().Format(time.RFC3339),
	}
	if err := fm.WriteMetadata(md); err != nil {
		t.Fatal(err)
	}
	return fm
}

func TestPrepareViewer_FailedCheckWarnsOnceAndServesTheCache(t *testing.T) {
	api, _ := failingGitHub(t)
	fm := cachedViewerManager(t, api.URL, time.Now().Add(-48*time.Hour))
	var stderr bytes.Buffer

	served := prepareViewer(fm, viewerCheckDaily, &stderr)

	if served.Reason != frontend.ServedFromCache || served.Version != "0.2.0" {
		t.Errorf("served = %+v, want the cached 0.2.0", served)
	}
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Warning: ") || !strings.HasSuffix(lines[0], ". Viewer: v0.2.0 (cached).") {
		t.Errorf("stderr = %q, want one warning line naming the cached viewer", stderr.String())
	}
	if !fm.IsAvailable() {
		t.Error("the cached viewer must still be served")
	}
}

func TestPrepareViewer_SkipFrontendAsksNothing(t *testing.T) {
	api, hits := failingGitHub(t)
	fm := cachedViewerManager(t, api.URL, time.Now().Add(-48*time.Hour))
	var stderr bytes.Buffer

	served := prepareViewer(fm, viewerUnmanaged, &stderr)

	if hits.Load() != 0 {
		t.Errorf("GitHub asked %d times, want 0 under --skip-frontend", hits.Load())
	}
	if served.Reason != frontend.ServedFromCache || stderr.Len() != 0 {
		t.Errorf("served = %+v, stderr = %q; want the cache and no warning", served, stderr.String())
	}
}

func TestPrepareViewer_UpdateViewerChecksNow(t *testing.T) {
	api, hits := failingGitHub(t)
	fm := cachedViewerManager(t, api.URL, time.Now().Add(-time.Hour))
	var stderr bytes.Buffer

	_ = prepareViewer(fm, viewerCheckNow, &stderr)

	if hits.Load() != 1 {
		t.Errorf("GitHub asked %d times, want 1 under --update-viewer", hits.Load())
	}
}

func TestRunServe_UpdateViewerWithSkipFrontendIsAUsageError(t *testing.T) {
	t.Cleanup(resetServeGlobals)
	params := serveParams{
		host:         "127.0.0.1",
		port:         freePort(t),
		searchRoots:  []string{t.TempDir()},
		appVersion:   "test",
		skipFrontend: true,
		updateViewer: true,
	}

	err := runServe(&bytes.Buffer{}, params)

	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
		t.Fatalf("runServe error = %v, want exit code %d", err, ExitUsageError)
	}
}

package clicommand

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// cachedViewerManager returns a manager over a cache holding the oldest
// viewer the supported range takes (frontend.MinViewerVersion), last
// checked at lastCheck, that asks apiBase for releases.
func cachedViewerManager(t *testing.T, apiBase string, lastCheck time.Time) *frontend.Manager {
	t.Helper()
	t.Setenv("RX_FRONTEND_URL", "")
	t.Setenv("RX_FRONTEND_VERSION", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"+frontend.MinViewerVersion+"</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	fm := frontend.NewManager(frontend.Config{CacheDir: dir, APIBase: apiBase})
	md := &frontend.CacheMetadata{
		Version:   frontend.Version{Version: frontend.MinViewerVersion},
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

	if served.Reason != frontend.ServedFromCache || served.Version != frontend.MinViewerVersion {
		t.Errorf("served = %+v, want the cached %s", served, frontend.MinViewerVersion)
	}
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Warning: ") || !strings.HasSuffix(lines[0], ". Viewer: v"+frontend.MinViewerVersion+" (cached).") {
		t.Errorf("stderr = %q, want one warning line naming the cached viewer", stderr.String())
	}
	if !fm.IsAvailable() {
		t.Error("the cached viewer must still be served")
	}
}

// viewerTarball builds a dist.tar.gz that installs as viewer version.
func viewerTarball(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"index.html", "<html>" + version + "</html>"},
		{"assets/app.js", "console.log(1)"},
		{"version.json", `{"version":"` + version + `"}`},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// nextPatch returns version with its patch number one higher: a release
// newer than version and, for MinViewerVersion, inside the range.
func nextPatch(t *testing.T, version string) string {
	t.Helper()
	parts := strings.Split(version, ".")
	patch, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("version %q: %v", version, err)
	}
	parts[len(parts)-1] = strconv.Itoa(patch + 1)
	return strings.Join(parts, ".")
}

// releaseWithForbiddenSidecar answers like GitHub for one release, tag,
// whose bundle is a valid viewer and whose .sha256 sidecar answers 403.
func releaseWithForbiddenSidecar(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	bundle := viewerTarball(t, strings.TrimPrefix(tag, "v"))
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + frontend.GitHubRepo + "/releases":
			_, _ = fmt.Fprintf(w, `[{"tag_name":%q,"assets":[{"name":"dist.tar.gz","browser_download_url":%q}]}]`,
				tag, srv.URL+"/dist.tar.gz")
		case "/dist.tar.gz":
			_, _ = w.Write(bundle)
		case "/dist.tar.gz.sha256":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPrepareViewer_UnreadableSidecarKeepsTheCachedViewer: a newer
// release whose sidecar answers 403 is not installed unverified; the
// cached viewer stays, and one warning line says why.
func TestPrepareViewer_UnreadableSidecarKeepsTheCachedViewer(t *testing.T) {
	newer := nextPatch(t, frontend.MinViewerVersion)
	releases := releaseWithForbiddenSidecar(t, "v"+newer)
	fm := cachedViewerManager(t, releases.URL, time.Now().Add(-48*time.Hour))
	var stderr bytes.Buffer

	served := prepareViewer(fm, viewerCheckDaily, &stderr)

	if served.Reason != frontend.ServedFromCache || served.Version != frontend.MinViewerVersion {
		t.Errorf("served = %+v, want the cached %s, not %s", served, frontend.MinViewerVersion, newer)
	}
	want := "frontend checksum: sidecar returned 403. Viewer: v" + frontend.MinViewerVersion + " (cached)."
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Warning: ") || !strings.HasSuffix(lines[0], want) {
		t.Errorf("stderr = %q, want one warning line ending in %q", stderr.String(), want)
	}
	index, err := os.ReadFile(fm.IndexHTMLPath())
	if err != nil || string(index) != "<html>"+frontend.MinViewerVersion+"</html>" {
		t.Errorf("index.html = %q (%v), want the cached viewer's", index, err)
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

// --update-viewer has nothing to update when RX_FRONTEND_PATH names the
// viewer: rx serves that directory as it is, so the flag is refused
// rather than silently doing nothing.
func TestRunServe_UpdateViewerWithLocalPathIsAUsageError(t *testing.T) {
	t.Cleanup(resetServeGlobals)
	t.Setenv("RX_FRONTEND_PATH", t.TempDir())
	params := serveParams{
		host:         "127.0.0.1",
		port:         freePort(t),
		searchRoots:  []string{t.TempDir()},
		appVersion:   "test",
		updateViewer: true,
	}

	err := runServe(&bytes.Buffer{}, params)

	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
		t.Fatalf("runServe error = %v, want exit code %d", err, ExitUsageError)
	}
	if !strings.Contains(exitErr.Error(), "RX_FRONTEND_PATH") {
		t.Errorf("message %q does not name RX_FRONTEND_PATH", exitErr.Error())
	}
}

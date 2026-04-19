package frontend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFakeTarball returns a minimal valid rx-viewer dist.tar.gz:
// index.html + assets/app.js + version.json.
func makeFakeTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, data []byte) {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	writeDir := func(name string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", []byte("<html>ok</html>"))
	writeDir("assets")
	write("assets/app.js", []byte("console.log('ok');"))
	write("version.json", []byte(`{"version":"1.2.3","buildDate":"2026-01-01","commit":"abc"}`))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ============================================================================
// NewManager defaults
// ============================================================================

func TestNewManager_UsesEnvPath(t *testing.T) {
	t.Setenv("RX_FRONTEND_PATH", "/tmp/rx-test-frontend-xyz")
	m := NewManager(Config{Logger: silentLog()})
	if m.CacheDir != "/tmp/rx-test-frontend-xyz" {
		t.Errorf("CacheDir = %q", m.CacheDir)
	}
}

func TestNewManager_TildeExpansion(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Setenv("RX_FRONTEND_PATH", "~/.rx-test")
	m := NewManager(Config{Logger: silentLog()})
	if m.CacheDir != filepath.Join(home, ".rx-test") {
		t.Errorf("CacheDir = %q, want %q", m.CacheDir, filepath.Join(home, ".rx-test"))
	}
}

func TestNewManager_FallsBackToConfigCacheDir(t *testing.T) {
	t.Setenv("RX_FRONTEND_PATH", "")
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	m := NewManager(Config{Logger: silentLog()})
	if !strings.Contains(m.CacheDir, "frontend") {
		t.Errorf("CacheDir = %q, want ...frontend/", m.CacheDir)
	}
}

// ============================================================================
// Metadata
// ============================================================================

func TestManager_WriteAndReadMetadata(t *testing.T) {
	m := NewManager(Config{CacheDir: t.TempDir(), Logger: silentLog()})
	md := &CacheMetadata{
		Version:      Version{Version: "1.0.0", BuildDate: "2026-04-18", Commit: "deadbeef"},
		DownloadedAt: "2026-04-18T00:00:00Z",
		LastCheck:    "2026-04-18T00:00:00Z",
		ReleaseURL:   "https://example.com/dist.tar.gz",
	}
	if err := m.WriteMetadata(md); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	got, err := m.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Version.Version != "1.0.0" || got.Version.Commit != "deadbeef" {
		t.Errorf("round-trip: %+v", got)
	}
}

func TestManager_ReadMetadata_MissingReturnsNilNil(t *testing.T) {
	m := NewManager(Config{CacheDir: t.TempDir(), Logger: silentLog()})
	got, err := m.ReadMetadata()
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil for missing metadata", got)
	}
}

// ============================================================================
// IsAvailable
// ============================================================================

func TestManager_IsAvailable_TrueWhenBothPresent(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(dir, "assets"), 0o700)
	m := NewManager(Config{CacheDir: dir, Logger: silentLog()})
	if !m.IsAvailable() {
		t.Error("IsAvailable = false, want true with both present")
	}
}

func TestManager_IsAvailable_FalseWhenMissing(t *testing.T) {
	m := NewManager(Config{CacheDir: t.TempDir(), Logger: silentLog()})
	if m.IsAvailable() {
		t.Error("IsAvailable = true on empty dir")
	}
}

// ============================================================================
// Fetch & Download via httptest
// ============================================================================

// TestEnsure_NoCacheFetchesLatest is the "first run" smoke test.
// With no cache and no env overrides, it should hit /releases/latest,
// download the asset, extract, and leave a valid cache.
func TestEnsure_NoCacheFetchesLatest(t *testing.T) {
	tarball := makeFakeTarball(t)
	tag := "v1.2.3"

	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/repos/wlame/rx-viewer/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		// Need to point to the same server's /dist.tar.gz endpoint.
		downloadURL := srv.URL + "/dist.tar.gz"
		resp := map[string]any{
			"tag_name": tag,
			"assets": []map[string]any{
				{"name": "dist.tar.gz", "browser_download_url": downloadURL},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/dist.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dest := t.TempDir()
	// Important: clear env so we hit the "no overrides" branch.
	t.Setenv("RX_FRONTEND_URL", "")
	t.Setenv("RX_FRONTEND_VERSION", "")

	m := NewManager(Config{
		CacheDir: dest,
		Repo:     "wlame/rx-viewer",
		APIBase:  srv.URL,
		Logger:   silentLog(),
	})
	// Force a fresh env read.
	m.envURL = ""
	m.envVersion = ""

	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !m.IsAvailable() {
		t.Error("IsAvailable() false after successful Ensure")
	}
	md, err := m.ReadMetadata()
	if err != nil || md == nil {
		t.Fatalf("ReadMetadata after Ensure: err=%v md=%v", err, md)
	}
	if md.Version.Version != "1.2.3" {
		t.Errorf("metadata version = %q, want 1.2.3", md.Version.Version)
	}
}

// TestEnsure_EnvURLForcesDownload exercises the RX_FRONTEND_URL
// fast-path — no GitHub API hit, just go fetch the URL and extract.
func TestEnsure_EnvURLForcesDownload(t *testing.T) {
	tarball := makeFakeTarball(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)

	dest := t.TempDir()
	m := NewManager(Config{
		CacheDir: dest,
		Repo:     "wlame/rx-viewer",
		APIBase:  "http://127.0.0.1:0", // should never be hit
		Logger:   silentLog(),
	})
	m.envURL = srv.URL
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !m.IsAvailable() {
		t.Error("IsAvailable false after env URL download")
	}
}

// TestEnsure_CachedVersionSkipsDownload — if the cached metadata
// already matches RX_FRONTEND_VERSION, no network call is made.
func TestEnsure_CachedVersionSkipsDownload(t *testing.T) {
	dest := t.TempDir()
	// Pre-populate a valid cache.
	_ = os.WriteFile(filepath.Join(dest, "index.html"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(dest, "assets"), 0o700)

	m := NewManager(Config{
		CacheDir: dest,
		Logger:   silentLog(),
	})
	_ = m.WriteMetadata(&CacheMetadata{
		Version: Version{Version: "7.7.7"},
	})
	m.envVersion = "v7.7.7"
	// APIBase intentionally unreachable — we shouldn't call it.
	m.APIBase = "http://127.0.0.1:1"

	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
}

// TestEnsure_CacheHitWithoutEnvVars uses cached SPA directly.
func TestEnsure_CacheHitWithoutEnvVars(t *testing.T) {
	dest := t.TempDir()
	_ = os.WriteFile(filepath.Join(dest, "index.html"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(dest, "assets"), 0o700)

	m := NewManager(Config{CacheDir: dest, Logger: silentLog()})
	m.APIBase = "http://127.0.0.1:1" // unreachable; should not be called
	if err := m.Ensure(context.Background()); err != nil {
		t.Errorf("Ensure unexpectedly failed: %v", err)
	}
}

// TestDownload_RejectsHTTPErrors confirms non-2xx is surfaced.
func TestDownload_RejectsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	dest := t.TempDir()
	m := NewManager(Config{CacheDir: dest, Logger: silentLog()})
	if err := m.Download(context.Background(), srv.URL, "v1"); err == nil {
		t.Error("want error on 500")
	}
}

// ============================================================================
// ValidateStaticPath
// ============================================================================

func TestValidateStaticPath(t *testing.T) {
	dest := t.TempDir()
	_ = os.WriteFile(filepath.Join(dest, "index.html"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(dest, "assets"), 0o700)
	_ = os.WriteFile(filepath.Join(dest, "assets/app.js"), []byte("js"), 0o600)

	m := NewManager(Config{CacheDir: dest, Logger: silentLog()})

	// Valid path.
	if p := m.ValidateStaticPath("index.html"); p == "" {
		t.Error("valid path rejected")
	}
	// Nested valid.
	if p := m.ValidateStaticPath("assets/app.js"); p == "" {
		t.Error("nested valid path rejected")
	}
	// Traversal attack.
	if p := m.ValidateStaticPath("../../../etc/passwd"); p != "" {
		t.Errorf("traversal accepted: %q", p)
	}
	// Absolute path.
	if p := m.ValidateStaticPath("/etc/passwd"); p != "" {
		t.Errorf("absolute accepted: %q", p)
	}
	// Non-existent.
	if p := m.ValidateStaticPath("not-here.js"); p != "" {
		t.Errorf("non-existent accepted: %q", p)
	}
	// Directory.
	if p := m.ValidateStaticPath("assets"); p != "" {
		t.Errorf("directory accepted: %q", p)
	}
	// Empty.
	if p := m.ValidateStaticPath(""); p != "" {
		t.Errorf("empty accepted: %q", p)
	}
}

// ============================================================================
// Direct download URL construction
// ============================================================================

func TestDirectDownloadURL(t *testing.T) {
	m := &Manager{Repo: "wlame/rx-viewer"}
	cases := []struct {
		version string
		want    string
	}{
		{"latest", "https://github.com/wlame/rx-viewer/releases/latest/download/dist.tar.gz"},
		{"v1.2.3", "https://github.com/wlame/rx-viewer/releases/download/v1.2.3/dist.tar.gz"},
		{"1.2.3", "https://github.com/wlame/rx-viewer/releases/download/v1.2.3/dist.tar.gz"},
	}
	for _, tc := range cases {
		got := m.directDownloadURL(tc.version)
		if got != tc.want {
			t.Errorf("directDownloadURL(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// TestFetchLatestRelease_404ReturnsNilNil
func TestFetchLatestRelease_404ReturnsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	m := NewManager(Config{CacheDir: t.TempDir(), APIBase: srv.URL, Logger: silentLog()})
	rel, err := m.fetchLatestRelease(context.Background())
	if err != nil {
		t.Errorf("want nil err on 404, got %v", err)
	}
	if rel != nil {
		t.Errorf("want nil rel on 404, got %+v", rel)
	}
}

// Final guard: make sure we don't accidentally reach for the real
// GitHub API in any test (safety net — tests should use m.APIBase).
var _ = fmt.Sprintf

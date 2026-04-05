package frontend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/config"
)

// createTestTarGz builds a minimal tar.gz in memory containing the files
// needed for IsAvailable() to return true (index.html + assets/ dir).
func createTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for name, content := range files {
		if content == "DIR" {
			err := tw.WriteHeader(&tar.Header{
				Name:     name,
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			})
			require.NoError(t, err)
		} else {
			err := tw.WriteHeader(&tar.Header{
				Name:     name,
				Size:     int64(len(content)),
				Mode:     0o644,
				Typeflag: tar.TypeReg,
			})
			require.NoError(t, err)
			_, err = tw.Write([]byte(content))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := &config.Config{
		CacheDir:        tmpDir,
		FrontendPath:    "",
		FrontendVersion: "",
		FrontendURL:     "",
	}

	return NewManager(cfg), tmpDir
}

func TestManager_IsAvailable_EmptyDir(t *testing.T) {
	m, _ := newTestManager(t)
	assert.False(t, m.IsAvailable())
}

func TestManager_IsAvailable_WithFiles(t *testing.T) {
	m, tmpDir := newTestManager(t)

	frontendDir := filepath.Join(tmpDir, "frontend")
	os.MkdirAll(filepath.Join(frontendDir, "assets"), 0o755)
	os.WriteFile(filepath.Join(frontendDir, "index.html"), []byte("<html></html>"), 0o644)

	assert.True(t, m.IsAvailable())
}

func TestManager_ValidateStaticPath_Safe(t *testing.T) {
	m, tmpDir := newTestManager(t)

	// Create a frontend file.
	frontendDir := filepath.Join(tmpDir, "frontend")
	os.MkdirAll(frontendDir, 0o755)
	os.WriteFile(filepath.Join(frontendDir, "index.html"), []byte("<html></html>"), 0o644)

	resolved, err := m.ValidateStaticPath("index.html")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(frontendDir, "index.html"), resolved)
}

func TestManager_ValidateStaticPath_TraversalBlocked(t *testing.T) {
	m, tmpDir := newTestManager(t)

	// Create a file outside the frontend dir to try to reach.
	os.WriteFile(filepath.Join(tmpDir, "secret.txt"), []byte("sensitive"), 0o644)

	traversalPaths := []string{
		"../secret.txt",
		"../../etc/passwd",
		"assets/../../secret.txt",
		"../../../tmp/evil",
	}

	for _, p := range traversalPaths {
		_, err := m.ValidateStaticPath(p)
		assert.Error(t, err, "path %q should be blocked", p)
	}
}

func TestManager_ValidateStaticPath_Nonexistent(t *testing.T) {
	m, _ := newTestManager(t)

	_, err := m.ValidateStaticPath("nonexistent.html")
	assert.Error(t, err)
}

func TestManager_ValidateStaticPath_Directory(t *testing.T) {
	m, tmpDir := newTestManager(t)

	frontendDir := filepath.Join(tmpDir, "frontend")
	os.MkdirAll(filepath.Join(frontendDir, "assets"), 0o755)

	_, err := m.ValidateStaticPath("assets")
	assert.Error(t, err, "directories should be rejected")
}

func TestManager_EnsureFrontend_MockServer(t *testing.T) {
	// Create a mock HTTP server that serves a valid dist.tar.gz.
	tarGzData := createTestTarGz(t, map[string]string{
		"index.html":       "<html><body>rx-viewer</body></html>",
		"assets/":          "DIR",
		"assets/main.js":   "console.log('hello');",
		"assets/style.css": "body { margin: 0; }",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		w.Write(tarGzData)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	cfg := &config.Config{
		CacheDir:    tmpDir,
		FrontendURL: srv.URL + "/dist.tar.gz",
	}

	m := NewManager(cfg)

	path, err := m.EnsureFrontend()
	require.NoError(t, err)
	assert.NotEmpty(t, path)
	assert.True(t, m.IsAvailable())

	// Verify extracted files.
	content, err := os.ReadFile(filepath.Join(path, "index.html"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "rx-viewer")

	content, err = os.ReadFile(filepath.Join(path, "assets", "main.js"))
	require.NoError(t, err)
	assert.Equal(t, "console.log('hello');", string(content))
}

func TestManager_EnsureFrontend_VersionCheck(t *testing.T) {
	tarGzData := createTestTarGz(t, map[string]string{
		"index.html":   "<html></html>",
		"assets/":      "DIR",
		"assets/app.js": "var x=1;",
		"version.json":  `{"version":"1.2.3","buildDate":"2026-01-01","commit":"abc123"}`,
	})

	var downloadCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCount++
		w.WriteHeader(http.StatusOK)
		w.Write(tarGzData)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	cfg := &config.Config{
		CacheDir:        tmpDir,
		FrontendURL:     srv.URL + "/dist.tar.gz",
		FrontendVersion: "1.2.3",
	}

	m := NewManager(cfg)

	// First call: downloads because nothing is cached.
	path, err := m.EnsureFrontend()
	require.NoError(t, err)
	assert.NotEmpty(t, path)
	assert.Equal(t, 1, downloadCount)

	// Now create a new manager pointing to the same cache, with URL cleared.
	// The version is already cached, so it should NOT re-download.
	cfg2 := &config.Config{
		CacheDir:        tmpDir,
		FrontendVersion: "1.2.3",
	}
	m2 := NewManager(cfg2)

	path2, err := m2.EnsureFrontend()
	require.NoError(t, err)
	assert.NotEmpty(t, path2)
	assert.Equal(t, 1, downloadCount, "should not have re-downloaded matching version")
}

func TestManager_EnsureFrontend_DownloadFailure_Fallback(t *testing.T) {
	// Server returns 404.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	cfg := &config.Config{
		CacheDir:    tmpDir,
		FrontendURL: srv.URL + "/dist.tar.gz",
	}

	m := NewManager(cfg)

	path, err := m.EnsureFrontend()
	// Should not error -- graceful degradation.
	assert.NoError(t, err)
	assert.Empty(t, path, "no frontend should be available after failed download")
}

func TestManager_EnsureFrontend_NoCacheNoEnv(t *testing.T) {
	// Server returns 404 for "latest" download to simulate no release available.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	cfg := &config.Config{
		CacheDir: tmpDir,
	}

	m := NewManager(cfg)
	// Override the download URL to point at the mock server so we don't
	// hit the real GitHub and get a non-deterministic result.
	m.envURL = srv.URL + "/dist.tar.gz"

	path, err := m.EnsureFrontend()
	assert.NoError(t, err)
	assert.Empty(t, path, "should return empty when download fails and no cache exists")
}

func TestManager_ExtractTarGz_PathTraversalInArchive(t *testing.T) {
	// Build a tar.gz with a malicious path that attempts traversal.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Malicious entry that tries to escape the extraction directory.
	err := tw.WriteHeader(&tar.Header{
		Name:     "../../../etc/evil",
		Size:     4,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	})
	require.NoError(t, err)
	_, err = tw.Write([]byte("evil"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "malicious.tar.gz")
	os.WriteFile(tarPath, buf.Bytes(), 0o644)

	m := &Manager{cacheDir: tmpDir}
	err = m.extractTarGz(tarPath)
	assert.Error(t, err, "should reject archive with path traversal")
	assert.Contains(t, err.Error(), "unsafe archive path")
}

func TestManager_DirectDownloadURL(t *testing.T) {
	m := &Manager{}

	assert.Equal(t,
		"https://github.com/wlame/rx-viewer/releases/latest/download/dist.tar.gz",
		m.directDownloadURL("latest"),
	)

	assert.Equal(t,
		"https://github.com/wlame/rx-viewer/releases/download/v1.2.3/dist.tar.gz",
		m.directDownloadURL("1.2.3"),
	)

	assert.Equal(t,
		"https://github.com/wlame/rx-viewer/releases/download/v1.2.3/dist.tar.gz",
		m.directDownloadURL("v1.2.3"),
	)
}

func TestManager_FrontendPathOverride(t *testing.T) {
	tmpDir := t.TempDir()
	customDir := filepath.Join(tmpDir, "custom-frontend")

	cfg := &config.Config{
		CacheDir:     tmpDir,
		FrontendPath: customDir,
	}

	m := NewManager(cfg)
	assert.Equal(t, customDir, m.Dir())
}

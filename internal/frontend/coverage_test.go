package frontend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// expandHome — all branches
// ============================================================================

// TestExpandHome covers the zero / tilde-only / tilde-slash / plain cases.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"~", home},
		{"~/foo", filepath.Join(home, "foo")},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~other", "~other"}, // tilde + non-slash → unchanged
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := expandHome(tc.in)
			if got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ============================================================================
// Metadata round-trip
// ============================================================================

// TestMetadata_RoundTrip writes + reads metadata via the Manager.
func TestMetadata_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{CacheDir: dir})
	md := &CacheMetadata{
		Version:      Version{Version: "1.2.3", BuildDate: "2026-01-01", Commit: "abc"},
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		LastCheck:    time.Now().UTC().Format(time.RFC3339),
		ReleaseURL:   "https://example.com/r",
	}
	if err := m.WriteMetadata(md); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	back, err := m.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if back.Version.Version != md.Version.Version {
		t.Errorf("Version: got %v, want %v", back.Version, md.Version)
	}
	if back.ReleaseURL != md.ReleaseURL {
		t.Errorf("ReleaseURL: got %s, want %s", back.ReleaseURL, md.ReleaseURL)
	}
}

// TestReadMetadata_Missing returns (nil, nil) on absent file.
func TestReadMetadata_Missing(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{CacheDir: dir})
	md, err := m.ReadMetadata()
	if err != nil {
		t.Errorf("ReadMetadata on missing file: got err %v, want nil", err)
	}
	if md != nil {
		t.Errorf("expected nil metadata, got %v", md)
	}
}

// TestReadMetadata_BadJSON rejects malformed content.
func TestReadMetadata_BadJSON(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{CacheDir: dir})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(m.MetadataPath(), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := m.ReadMetadata(); err == nil {
		t.Errorf("expected error for malformed JSON")
	}
}

// TestIsAvailable_False — missing index.html means not available.
func TestIsAvailable_False(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{CacheDir: dir})
	if m.IsAvailable() {
		t.Errorf("IsAvailable should be false on empty cache")
	}
}

// TestIsAvailable_WithoutAssets — index.html but no assets/ dir → false.
func TestIsAvailable_WithoutAssets(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o644)
	m := NewManager(Config{CacheDir: dir})
	if m.IsAvailable() {
		t.Errorf("IsAvailable should be false without assets/")
	}
}

// TestIsAvailable_True — both present.
func TestIsAvailable_True(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	m := NewManager(Config{CacheDir: dir})
	if !m.IsAvailable() {
		t.Errorf("IsAvailable should be true with index.html + assets/")
	}
}

// ============================================================================
// fetchLatestRelease — GitHub API error paths
// ============================================================================

// TestFetchLatestRelease_404 returns nil (no releases yet).
func TestFetchLatestRelease_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m := NewManager(Config{
		APIBase:  srv.URL,
		CacheDir: t.TempDir(),
	})
	info, err := m.fetchLatestRelease(context.Background())
	if err != nil {
		t.Errorf("404 should yield (nil, nil), got err %v", err)
	}
	if info != nil {
		t.Errorf("404 should yield (nil, nil), got info %v", info)
	}
}

// TestFetchLatestRelease_500 returns an error.
func TestFetchLatestRelease_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := NewManager(Config{APIBase: srv.URL, CacheDir: t.TempDir()})
	_, err := m.fetchLatestRelease(context.Background())
	if err == nil {
		t.Errorf("expected error on 500")
	}
}

// TestFetchLatestRelease_BadJSON returns an error.
func TestFetchLatestRelease_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	m := NewManager(Config{APIBase: srv.URL, CacheDir: t.TempDir()})
	_, err := m.fetchLatestRelease(context.Background())
	if err == nil {
		t.Errorf("expected error on bad JSON")
	}
}

// TestFetchLatestRelease_Good parses a valid response.
func TestFetchLatestRelease_Good(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.0.0","assets":[{"name":"dist.tar.gz","browser_download_url":"http://x"}]}`))
	}))
	defer srv.Close()
	m := NewManager(Config{APIBase: srv.URL, CacheDir: t.TempDir()})
	info, err := m.fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil release")
	}
	if info.TagName != "v1.0.0" {
		t.Errorf("tag: got %s, want v1.0.0", info.TagName)
	}
	if len(info.Assets) != 1 {
		t.Errorf("assets: got %d, want 1", len(info.Assets))
	}
}

// ============================================================================
// Ensure — end-to-end
// ============================================================================

// TestEnsure_DirectURL_Override exercises the RX_FRONTEND_URL path.
func TestEnsure_DirectURL_Override(t *testing.T) {
	tarball := buildFakeTarballV("1.2.3")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer backend.Close()

	dir := t.TempDir()
	t.Setenv("RX_FRONTEND_URL", backend.URL+"/dist.tar.gz")
	m := NewManager(Config{CacheDir: dir})
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !m.IsAvailable() {
		t.Errorf("cache should be available after Ensure")
	}
}

// Note: RX_FRONTEND_VERSION-based download tests are challenging to
// stub because the manager resolves the download URL from GitHub's real
// CDN. Coverage for that code path is provided by the direct-URL override
// test (TestEnsure_DirectURL_Override) plus the GitHub API stub tests
// in TestFetchLatestRelease_Good.

// TestEnsure_CacheHit skips GitHub entirely on valid cache.
func TestEnsure_CacheHit(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate a valid cache.
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>cached</html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	md := &CacheMetadata{
		Version: Version{Version: "1.0.0"},
	}
	b, _ := json.Marshal(md)
	_ = os.WriteFile(filepath.Join(dir, ".metadata.json"), b, 0o644)

	// Provide a non-responsive API base so a cache miss would fail loud.
	m := NewManager(Config{
		CacheDir: dir,
		APIBase:  "http://127.0.0.1:1", // nothing listens here
	})
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure on cache hit: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	if string(got) != "<html>cached</html>" {
		t.Errorf("cached content overwritten: %q", got)
	}
}

// TestEnsure_NoReleases — latest endpoint returns 404. Should fail loudly.
func TestEnsure_NoReleases(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()
	dir := t.TempDir()
	m := NewManager(Config{CacheDir: dir, APIBase: api.URL})
	if err := m.Ensure(context.Background()); err == nil {
		t.Errorf("expected error when no releases available")
	}
}

// ============================================================================
// extractTarGz error paths
// ============================================================================

// TestExtractTarGz_BadGzip rejects non-gzip input.
func TestExtractTarGz_BadGzip(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.tar.gz")
	_ = os.WriteFile(badPath, []byte("not gzip"), 0o644)
	err := extractTarGz(badPath, dir)
	if err == nil {
		t.Errorf("expected error for non-gzip stream")
	}
}

// TestExtractTarGz_TraversalAttempt rejects path-traversal.
func TestExtractTarGz_TraversalAttempt(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "../evil.txt", Size: 4, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()
	_ = gz.Close()

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "evil.tar.gz")
	_ = os.WriteFile(tarPath, buf.Bytes(), 0o644)
	err := extractTarGz(tarPath, dir)
	if err == nil {
		t.Errorf("expected error for path traversal")
	}
}

// TestExtractTarGz_AbsolutePath rejects entries starting with /.
func TestExtractTarGz_AbsolutePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-specific path semantics")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "/etc/evil", Size: 4, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()
	_ = gz.Close()

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "abs.tar.gz")
	_ = os.WriteFile(tarPath, buf.Bytes(), 0o644)
	_ = extractTarGz(tarPath, dir)
	if _, err := os.Stat("/etc/evil"); err == nil {
		t.Errorf("/etc/evil was created (absolute path extraction)")
	}
}

// TestExtractTarGz_Happy path creates expected files.
func TestExtractTarGz_Happy(t *testing.T) {
	tarball := buildFakeTarballV("1.0.0")
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "good.tar.gz")
	_ = os.WriteFile(tarPath, tarball, 0o644)
	if err := extractTarGz(tarPath, dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, name := range []string{"index.html", "version.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("extracted file missing: %s (%v)", name, err)
		}
	}
}

// ============================================================================
// Helpers
// ============================================================================

// buildFakeTarballV returns a minimal valid rx-viewer dist tarball with
// an assets/ dir (needed for IsAvailable) and a version.json carrying
// the given version string.
func buildFakeTarballV(version string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{
			Name: name, Size: int64(len(data)), Mode: 0o600, Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write(data)
	}
	writeDir := func(name string) {
		_ = tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o700, Typeflag: tar.TypeDir,
		})
	}
	write("index.html", []byte("<html>"+version+"</html>"))
	writeDir("assets/")
	write("assets/app.js", []byte("console.log('"+version+"');"))
	write("version.json", []byte(fmt.Sprintf(`{"version":%q,"buildDate":"2026-04-18","commit":"abc"}`, version)))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// TestNewManager_AppliesDefaults covers the env-var fallbacks.
func TestNewManager_AppliesDefaults(t *testing.T) {
	t.Setenv("RX_FRONTEND_PATH", t.TempDir())
	m := NewManager(Config{})
	if m.CacheDir == "" {
		t.Errorf("CacheDir should be populated from RX_FRONTEND_PATH")
	}
	if m.Repo != GitHubRepo {
		t.Errorf("Repo: got %s, want %s", m.Repo, GitHubRepo)
	}
	if !strings.HasPrefix(m.APIBase, "https://") {
		t.Errorf("APIBase: got %s, want https://...", m.APIBase)
	}
}

// TestDistURL_SelectsDistAsset picks the "dist.tar.gz" entry.
func TestDistURL_SelectsDistAsset(t *testing.T) {
	r := &releaseInfo{
		Assets: []releaseAsset{
			{Name: "other.zip", BrowserDownloadURL: "http://wrong"},
			{Name: "dist.tar.gz", BrowserDownloadURL: "http://correct"},
		},
	}
	if got := r.DistURL(); got != "http://correct" {
		t.Errorf("DistURL: got %s, want http://correct", got)
	}
}

// TestDistURL_EmptyIfMissing returns empty string when no dist asset.
func TestDistURL_EmptyIfMissing(t *testing.T) {
	r := &releaseInfo{
		Assets: []releaseAsset{{Name: "x.zip"}},
	}
	if got := r.DistURL(); got != "" {
		t.Errorf("DistURL: got %s, want empty", got)
	}
}

// TestDirectDownloadURL_AllBranches covers the three cases.
func TestDirectDownloadURL_AllBranches(t *testing.T) {
	m := NewManager(Config{CacheDir: t.TempDir()})
	// "latest"
	if u := m.directDownloadURL("latest"); !strings.Contains(u, "releases/latest/download") {
		t.Errorf("latest URL wrong: %s", u)
	}
	// "v1.2.3"
	u := m.directDownloadURL("v1.2.3")
	if !strings.Contains(u, "releases/download/v1.2.3") {
		t.Errorf("tagged URL wrong: %s", u)
	}
	// "1.2.3" (should prepend v)
	u2 := m.directDownloadURL("1.2.3")
	if u != u2 {
		t.Errorf("URLs should match regardless of v-prefix: %s vs %s", u, u2)
	}
}

// TestDownload_EmptyURL returns a clear error.
func TestDownload_EmptyURL(t *testing.T) {
	m := NewManager(Config{CacheDir: t.TempDir()})
	if err := m.Download(context.Background(), "", "1.0.0"); err == nil {
		t.Errorf("expected error for empty URL")
	}
}

// TestDownload_ServerError returns a clear error.
func TestDownload_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := NewManager(Config{CacheDir: t.TempDir()})
	if err := m.Download(context.Background(), srv.URL+"/x.tar.gz", "1.0.0"); err == nil {
		t.Errorf("expected error for 500 response")
	}
}

// TestDownload_InvalidTarball corrupts the body mid-stream to test the
// error path after HTTP success.
func TestDownload_InvalidTarball(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not a tar.gz at all"))
	}))
	defer srv.Close()
	m := NewManager(Config{CacheDir: t.TempDir()})
	if err := m.Download(context.Background(), srv.URL+"/x.tar.gz", "1.0.0"); err == nil {
		t.Errorf("expected error for non-tarball body")
	}
}

// TestClearExistingContent_PreservesMetadata empties the cache dir but
// keeps .metadata.json.
func TestClearExistingContent_PreservesMetadata(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{CacheDir: dir})
	// Populate.
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o644)
	_ = os.WriteFile(m.MetadataPath(), []byte("{}"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("x"), 0o644)

	if err := m.clearExistingContent(); err != nil {
		t.Fatalf("clearExistingContent: %v", err)
	}
	if _, err := os.Stat(m.MetadataPath()); err != nil {
		t.Errorf("metadata preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		t.Errorf("index.html should have been cleared")
	}
}

// TestReadDistVersion_NoFile returns an error.
func TestReadDistVersion_NoFile(t *testing.T) {
	dir := t.TempDir()
	if v, err := readDistVersion(dir); err == nil {
		t.Errorf("missing version.json: got %v, expected error", v)
	}
}

// TestReadDistVersion_Malformed returns an error.
func TestReadDistVersion_Malformed(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "version.json"), []byte("junk"), 0o644)
	if _, err := readDistVersion(dir); err == nil {
		t.Errorf("malformed version.json should error")
	}
}

// TestReadDistVersion_Valid reads the nested JSON.
func TestReadDistVersion_Valid(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "version.json"),
		[]byte(`{"version":"3.4.5","buildDate":"x","commit":"c"}`), 0o644)
	got, err := readDistVersion(dir)
	if err != nil {
		t.Fatalf("readDistVersion: %v", err)
	}
	if got.Version != "3.4.5" {
		t.Errorf("Version: got %q, want 3.4.5", got.Version)
	}
}

// TestStreamToFile writes bytes and verifies content.
func TestStreamToFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.bin")
	if err := streamToFile(bytes.NewReader([]byte("hello")), p); err != nil {
		t.Fatalf("streamToFile: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "hello" {
		t.Errorf("content: got %q, want hello", got)
	}
}

// TestWriteMetadata_DirCreated verifies the helper creates missing
// parent dirs.
func TestWriteMetadata_DirCreated(t *testing.T) {
	deep := filepath.Join(t.TempDir(), "a", "b", "c")
	m := NewManager(Config{CacheDir: deep})
	md := &CacheMetadata{Version: Version{Version: "0.1.0"}}
	if err := m.WriteMetadata(md); err != nil {
		t.Errorf("WriteMetadata should create parents: %v", err)
	}
}

// TestValidateTarPath_Variants covers all the validation branches.
func TestValidateTarPath_Variants(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{"empty_name", "", true},
		{"absolute", "/etc/passwd", true},
		{"dotdot_segment", "foo/../bar", true},
		{"pure_dotdot", "..", true},
		{"legal_nested", "assets/app.js", false},
		{"legal_shallow", "index.html", false},
		{"legal_dot_relative", "./index.html", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTarPath(dir, tc.entry)
			got := err != nil
			if got != tc.wantErr {
				t.Errorf("validateTarPath(%q): err=%v, wantErr=%v", tc.entry, err, tc.wantErr)
			}
		})
	}
}

// TestExtractTarGz_SymlinkRejected — symlinks escaping dest are rejected.
func TestExtractTarGz_SymlinkRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Write a legal regular file first.
	_ = tw.WriteHeader(&tar.Header{
		Name: "index.html", Size: 3, Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte("hi\n"))
	// Now a symlink trying to escape.
	_ = tw.WriteHeader(&tar.Header{
		Name:     "escape",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	})
	_ = tw.Close()
	_ = gz.Close()

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "sym.tar.gz")
	_ = os.WriteFile(tarPath, buf.Bytes(), 0o644)
	// Regardless of outcome, /etc/passwd shouldn't be modified and
	// no symlink to /etc/passwd should have been created in dest.
	_ = extractTarGz(tarPath, dir)
	if fi, err := os.Lstat(filepath.Join(dir, "escape")); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			// Ensure the symlink destination is inside the dest dir.
			target, _ := os.Readlink(filepath.Join(dir, "escape"))
			if strings.HasPrefix(target, "/") {
				t.Errorf("symlink points outside dest: %s", target)
			}
		}
	}
}

// TestWriteMetadata_ReadOnly returns an error on a read-only parent.
func TestWriteMetadata_ReadOnly(t *testing.T) {
	dir := t.TempDir()
	// Make the dir read-only.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	m := NewManager(Config{CacheDir: filepath.Join(dir, "blocked")})
	err := m.WriteMetadata(&CacheMetadata{Version: Version{Version: "x"}})
	if err == nil {
		t.Errorf("expected error writing into read-only parent")
	}
}

// TestIndexHTMLPath / TestMetadataPath verify string construction.
func TestIndexHTMLPath(t *testing.T) {
	m := NewManager(Config{CacheDir: "/tmp/fe"})
	if m.IndexHTMLPath() != "/tmp/fe/index.html" {
		t.Errorf("IndexHTMLPath: got %s", m.IndexHTMLPath())
	}
	if m.MetadataPath() != "/tmp/fe/.metadata.json" {
		t.Errorf("MetadataPath: got %s", m.MetadataPath())
	}
}

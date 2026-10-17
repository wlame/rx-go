package frontend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bundleFixture builds a minimal viewer bundle and returns its bytes and
// hex digest.
func bundleFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"index.html", "<!doctype html>"},
		{"version.json", `{"version":"0.9.9"}`},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	data := []byte(buf.String())
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// stringWriter adapts strings.Builder to io.Writer for the gzip writer.
type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// bundleServer serves a bundle at /dist.tar.gz and the sidecar text at
// /dist.tar.gz.sha256.
func bundleServer(t *testing.T, bundle []byte, sidecar string) *httptest.Server {
	t.Helper()
	return bundleServerWithSidecar(t, bundle, bodyHandler(sidecar))
}

// bundleServerWithSidecar serves a bundle at /dist.tar.gz and answers
// /dist.tar.gz.sha256 with the sidecar handler.
func bundleServerWithSidecar(t *testing.T, bundle []byte, sidecar http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dist.tar.gz":
			_, _ = w.Write(bundle)
		case "/dist.tar.gz.sha256":
			sidecar(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// statusHandler answers with the status and an empty body.
func statusHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }
}

// bodyHandler answers 200 with the body.
func bodyHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

// dropConnection closes the connection before any answer is written, so
// the client sees a transport error, as on a network failure. net/http
// recovers the http.ErrAbortHandler panic quietly and closes the socket.
func dropConnection(http.ResponseWriter, *http.Request) {
	panic(http.ErrAbortHandler)
}

// cacheWithPreviousBundle returns a cache directory holding an index.html
// that stands for the viewer installed before the download.
func cacheWithPreviousBundle(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "index.html"), []byte("previous bundle"), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	return cache
}

// TestDownload_VerifiesMatchingSidecar is scenario 1: a bundle whose
// digest matches is extracted and served.
func TestDownload_VerifiesMatchingSidecar(t *testing.T) {
	bundle, digest := bundleFixture(t)
	srv := bundleServer(t, bundle, digest+"  dist.tar.gz\n")

	cache := t.TempDir()
	m := NewManager(Config{CacheDir: cache})
	if err := m.Download(context.Background(), srv.URL+"/dist.tar.gz", "v0.9.9"); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "index.html")); err != nil {
		t.Errorf("index.html not extracted: %v", err)
	}
}

// TestDownload_RejectsMismatchedSidecar is scenario 2: a digest that does
// not match is fatal, and the cache that was already there survives.
func TestDownload_RejectsMismatchedSidecar(t *testing.T) {
	bundle, _ := bundleFixture(t)
	srv := bundleServer(t, bundle, strings.Repeat("0", 64)+"  dist.tar.gz\n")

	cache := t.TempDir()
	previous := filepath.Join(cache, "index.html")
	if err := os.WriteFile(previous, []byte("previous bundle"), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	err := m0(cache).Download(context.Background(), srv.URL+"/dist.tar.gz", "v0.9.9")

	if err == nil {
		t.Fatalf("Download: got nil, want a checksum failure")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error should name the checksum: %v", err)
	}
	content, readErr := os.ReadFile(previous)
	if readErr != nil {
		t.Fatalf("the previous bundle was destroyed: %v", readErr)
	}
	if string(content) != "previous bundle" {
		t.Errorf("the previous bundle was overwritten: %q", content)
	}
}

// TestDownload_RefusesASidecarItCannotRead: a bundle is installed only
// when its sidecar is read and holds the bundle's digest. A missing
// sidecar (404), any other answer but 200, a request that fails and a
// body that is not a digest refuse the bundle, and the cached viewer
// stays as it was.
func TestDownload_RefusesASidecarItCannotRead(t *testing.T) {
	cases := []struct {
		name      string
		sidecar   http.HandlerFunc
		wantInErr string
	}{
		{"absent", statusHandler(http.StatusNotFound), "sidecar returned 404"},
		{"forbidden", statusHandler(http.StatusForbidden), "sidecar returned 403"},
		{"server error", statusHandler(http.StatusInternalServerError), "sidecar returned 500"},
		{"dropped connection", dropConnection, "fetch sidecar"},
		{"not a digest", bodyHandler("hello\n"), "does not hold a sha256 digest"},
		{"empty body", bodyHandler(""), "sidecar is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle, _ := bundleFixture(t)
			srv := bundleServerWithSidecar(t, bundle, tc.sidecar)
			cache := cacheWithPreviousBundle(t)

			err := m0(cache).Download(context.Background(), srv.URL+"/dist.tar.gz", "v0.9.9")

			if err == nil {
				t.Fatalf("Download: got nil, want the bundle refused")
			}
			for _, want := range []string{"frontend checksum", tc.wantInErr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should contain %q", err, want)
				}
			}
			content, readErr := os.ReadFile(filepath.Join(cache, "index.html"))
			if readErr != nil || string(content) != "previous bundle" {
				t.Errorf("the previous bundle did not survive: %q (%v)", content, readErr)
			}
			for _, leftover := range []string{tempFileName, stagingDirName, metadataFileName} {
				if _, statErr := os.Stat(filepath.Join(cache, leftover)); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("%s is in the cache after a refusal (stat: %v)", leftover, statErr)
				}
			}
		})
	}
}

// TestDownload_KeepsCacheWhenTheBundleIsCorrupt covers the other way a
// download can fail after the bytes arrive.
func TestDownload_KeepsCacheWhenTheBundleIsCorrupt(t *testing.T) {
	corrupt := []byte("this is not a gzip stream")
	sum := sha256.Sum256(corrupt)
	srv := bundleServer(t, corrupt, hex.EncodeToString(sum[:]))

	cache := t.TempDir()
	previous := filepath.Join(cache, "index.html")
	if err := os.WriteFile(previous, []byte("previous bundle"), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if err := m0(cache).Download(context.Background(), srv.URL+"/dist.tar.gz", "v0.9.9"); err == nil {
		t.Fatalf("Download: got nil, want an extraction failure")
	}
	content, readErr := os.ReadFile(previous)
	if readErr != nil || string(content) != "previous bundle" {
		t.Errorf("the previous bundle did not survive: %q (%v)", content, readErr)
	}
}

// TestVerifySHA256 pins the sidecar parsing rules.
func TestVerifySHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dist.tar.gz")
	if err := os.WriteFile(path, []byte("bundle bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum := sha256.Sum256([]byte("bundle bytes"))
	digest := hex.EncodeToString(sum[:])

	cases := []struct {
		name    string
		sidecar string
		wantErr bool
	}{
		{"sha256sum format", fmt.Sprintf("%s  dist.tar.gz\n", digest), false},
		{"bare digest", digest, false},
		{"uppercase digest", strings.ToUpper(digest), false},
		{"mismatch", strings.Repeat("0", 64) + "  dist.tar.gz", true},
		{"not a digest", "hello", true},
		{"empty", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifySHA256(path, tc.sidecar)
			if (err != nil) != tc.wantErr {
				t.Errorf("verifySHA256: err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// m0 builds a Manager on a cache dir with the default HTTP client.
func m0(cacheDir string) *Manager { return NewManager(Config{CacheDir: cacheDir}) }

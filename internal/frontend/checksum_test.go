package frontend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// bundleServer serves a bundle at /dist.tar.gz and, when sidecar is not
// empty, its digest at /dist.tar.gz.sha256.
func bundleServer(t *testing.T, bundle []byte, sidecar string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dist.tar.gz":
			_, _ = w.Write(bundle)
		case "/dist.tar.gz.sha256":
			if sidecar == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(sidecar))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
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

// TestDownload_AcceptsMissingSidecarWithWarning is scenario 3: releases
// published before the sidecar existed still install.
func TestDownload_AcceptsMissingSidecarWithWarning(t *testing.T) {
	bundle, _ := bundleFixture(t)
	srv := bundleServer(t, bundle, "")

	cache := t.TempDir()
	if err := m0(cache).Download(context.Background(), srv.URL+"/dist.tar.gz", "v0.9.9"); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "index.html")); err != nil {
		t.Errorf("index.html not extracted: %v", err)
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

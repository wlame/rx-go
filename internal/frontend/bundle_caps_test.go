package frontend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tarball with more entries than the cap, and one whose entries total
// more than the cap, are refused before anything of them is kept.
func TestExtractTarGz_RefusesABundlePastTheCaps(t *testing.T) {
	many := make([]tarEntry, 0, maxBundleEntries+1)
	for i := 0; i <= maxBundleEntries; i++ {
		many = append(many, tarEntry{name: "assets/f" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "_" + itoa(i), data: []byte("1")})
	}
	dest := t.TempDir()
	err := extractTarGz(writeTarGz(t, many), dest)
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("a bundle of %d entries: %v, want a refusal naming the entry cap", len(many), err)
	}

	// An entry whose declared size alone passes the total cap (and not
	// the per-entry one): the header says so before any byte is read, so
	// the tarball itself stays small.
	big := writeTarGzWithDeclaredSize(t, maxBundleTotalBytes+1)
	err = extractTarGz(big, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "total") {
		t.Errorf("a bundle totalling past the cap: %v, want a refusal naming the total cap", err)
	}
}

// writeTarGzWithDeclaredSize writes a tarball with one header that
// declares an entry of size bytes and no data behind it: the reader
// refuses the total from the header before it reads an entry's bytes,
// so the tarball itself stays small.
func writeTarGzWithDeclaredSize(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "big.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "assets/big", Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	// No data follows: Close fails on the short entry, which is fine; the
	// header is what the reader checks first.
	_ = tw.Flush()
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	return p
}

func itoa(i int) string {
	var b []byte
	if i == 0 {
		return "0"
	}
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// A release asset longer than the cap is refused with an error that
// names the cap, the temp file is removed and nothing is unpacked.
func TestStreamToFile_RefusesABodyPastTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dist.tar.gz.tmp")
	if err := streamToFile(bytes.NewReader(make([]byte, 100)), path, 100); err != nil {
		t.Errorf("a body of exactly the cap: %v, want accepted", err)
	}
	err := streamToFile(bytes.NewReader(make([]byte, 101)), path, 100)
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("a body past the cap: %v, want a refusal naming the cap", err)
	}
}

// The cache's own files are never served as part of the viewer: a path
// with a component that starts with a dot, and the download in flight,
// answer what any unknown path answers.
func TestValidateStaticPath_RefusesTheCachesOwnFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".metadata.json", "dist.tar.gz.tmp", "favicon.svg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".staging"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".staging", "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{CacheDir: dir, Logger: silentLog()})
	for _, refused := range []string{".metadata.json", "dist.tar.gz.tmp", ".staging/index.html", "assets/../.metadata.json"} {
		if got := m.ValidateStaticPath(refused); got != "" {
			t.Errorf("ValidateStaticPath(%q) = %q, want nothing served", refused, got)
		}
	}
	if got := m.ValidateStaticPath("favicon.svg"); got == "" {
		t.Error("favicon.svg is not served any more")
	}
}

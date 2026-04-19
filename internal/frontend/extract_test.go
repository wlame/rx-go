package frontend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ============================================================================
// Tarball extractor — traversal attacks and normal content
// ============================================================================
//
// Writing synthetic tarballs on the fly keeps tests fast and reproducible.
// Each helper returns a path to a temp tar.gz file.

type tarEntry struct {
	name     string
	data     []byte
	typeflag byte
	linkname string
}

// writeTarGz assembles a tarball from entries.
func writeTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bundle.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o600,
			Size:     int64(len(e.data)),
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractTarGz_NormalContent(t *testing.T) {
	dest := t.TempDir()
	tb := writeTarGz(t, []tarEntry{
		{name: "index.html", data: []byte("<html></html>")},
		{name: "assets/", typeflag: tar.TypeDir},
		{name: "assets/app.js", data: []byte("console.log('hi');")},
	})
	if err := extractTarGz(tb, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "index.html")); err != nil {
		t.Errorf("index.html missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "assets/app.js")); err != nil {
		t.Errorf("assets/app.js missing: %v", err)
	}
}

func TestExtractTarGz_RejectsParentTraversal(t *testing.T) {
	dest := t.TempDir()
	tb := writeTarGz(t, []tarEntry{
		{name: "../etc/passwd", data: []byte("root:x:0:0")},
	})
	err := extractTarGz(tb, dest)
	if !errors.Is(err, ErrTarballTraversal) {
		t.Fatalf("want ErrTarballTraversal, got %v", err)
	}
	// NO file was created outside destDir.
	if _, err := os.Stat(filepath.Join(dest, "..", "etc", "passwd")); err == nil {
		t.Error("attacker file landed outside destDir")
	}
}

func TestExtractTarGz_RejectsAbsolutePath(t *testing.T) {
	dest := t.TempDir()
	tb := writeTarGz(t, []tarEntry{
		{name: "/etc/passwd", data: []byte("root:x:0:0")},
	})
	err := extractTarGz(tb, dest)
	if !errors.Is(err, ErrTarballTraversal) {
		t.Errorf("want ErrTarballTraversal, got %v", err)
	}
}

func TestExtractTarGz_RejectsDeepTraversal(t *testing.T) {
	dest := t.TempDir()
	tb := writeTarGz(t, []tarEntry{
		{name: "a/b/../../../etc/passwd", data: []byte("root")},
	})
	err := extractTarGz(tb, dest)
	if !errors.Is(err, ErrTarballTraversal) {
		t.Errorf("want ErrTarballTraversal, got %v", err)
	}
}

func TestExtractTarGz_SymlinkInsideOK(t *testing.T) {
	dest := t.TempDir()
	tb := writeTarGz(t, []tarEntry{
		{name: "real.js", data: []byte("real")},
		{name: "alias.js", typeflag: tar.TypeSymlink, linkname: "real.js"},
	})
	if err := extractTarGz(tb, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
}

func TestExtractTarGz_SymlinkEscapingIsRejected(t *testing.T) {
	dest := t.TempDir()
	tb := writeTarGz(t, []tarEntry{
		{name: "outside", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	err := extractTarGz(tb, dest)
	if !errors.Is(err, ErrTarballTraversal) {
		t.Errorf("want ErrTarballTraversal, got %v", err)
	}
}

func TestExtractTarGz_UnsupportedEntryType(t *testing.T) {
	dest := t.TempDir()
	// Hardlink is unsupported in our extractor.
	tb := writeTarGz(t, []tarEntry{
		{name: "hl", typeflag: tar.TypeLink, linkname: "anywhere"},
	})
	err := extractTarGz(tb, dest)
	if !errors.Is(err, ErrTarballUnsupportedEntry) {
		t.Errorf("want ErrTarballUnsupportedEntry, got %v", err)
	}
}

// ============================================================================
// validateTarPath direct unit tests
// ============================================================================

func TestValidateTarPath_Cases(t *testing.T) {
	dest := t.TempDir()
	absDest, _ := filepath.Abs(dest)
	okCases := []string{
		"index.html",
		"assets/app.js",
		"deeply/nested/file.txt",
	}
	for _, c := range okCases {
		if err := validateTarPath(absDest, c); err != nil {
			t.Errorf("unexpected rejection of %q: %v", c, err)
		}
	}
	badCases := []string{
		"",
		"/etc/passwd",
		"../boom",
		"a/../b", // still contains ".." segment
		"../../etc",
	}
	for _, c := range badCases {
		if err := validateTarPath(absDest, c); err == nil {
			t.Errorf("%q should have been rejected", c)
		}
	}
}

// ============================================================================
// Edge case: entry size exceeds the 512 MB cap
// ============================================================================

// The 512 MB entry-size cap is a defense against a pathological
// tarball claiming a massive size. It's hard to exercise in a unit
// test without producing a 512 MB file on disk; the code path is
// simple enough (an Int64 compare) that we settle for inspecting
// the constant via go vet / manual review. If this ever becomes a
// source of bugs, replace with an integration test that generates
// a fake-large tarball from `io.LimitReader` + custom `tar.Writer`.

// ============================================================================
// Integration: roundtrip via a real io stream (no filesystem attacker)
// ============================================================================

func TestExtractTarGz_IOCopyHappens(t *testing.T) {
	// Assemble a tarball in memory and verify file contents on disk
	// match what we put in.
	content := []byte("hello from a test")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "note.txt",
		Mode:     0o600,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	tb := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(tb, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := extractTarGz(tb, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file contents mismatch: got %q want %q", got, content)
	}
}

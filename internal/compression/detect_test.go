package compression

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestIsCompoundArchive(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo.tar.gz", true},
		{"foo.TGZ", true},
		{"foo.tar.zst", true},
		{"foo.tzst", true},
		{"foo.tar.xz", true},
		{"foo.txz", true},
		{"foo.tar.bz2", true},
		{"foo.tbz2", true},
		{"foo.tbz", true},
		{"foo.gz", false},
		{"foo.zst", false},
		{"foo.log", false},
		{"foo", false},
		{"/some/path/app.tar.gz", true},
		{"/path/to/archive.TAR.GZ", true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := IsCompoundArchive(tc.path); got != tc.want {
				t.Errorf("IsCompoundArchive(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestDetectByExtension(t *testing.T) {
	cases := []struct {
		path string
		want Format
	}{
		{"foo.gz", FormatGzip},
		{"foo.GZ", FormatGzip},
		{"foo.gzip", FormatGzip},
		{"foo.zst", FormatZstd},
		{"foo.zstd", FormatZstd},
		{"foo.xz", FormatXz},
		{"foo.bz2", FormatBz2},
		{"foo.bzip2", FormatBz2},
		{"foo.log", FormatNone},
		{"foo", FormatNone},
		// Compound archive should ignore the extension match.
		// NOTE: detectByExtension itself only checks the final extension;
		// the compound-archive gate is in DetectFromPath. So at this level
		// the .gz part of .tar.gz DOES match as gzip.
		{"foo.tar.gz", FormatGzip}, // raw extension check
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := detectByExtension(tc.path); got != tc.want {
				t.Errorf("detectByExtension(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestDetectFromReader_MagicBytes(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   Format
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, FormatGzip},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}, FormatZstd},
		{"xz", []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, FormatXz},
		{"bz2", []byte{0x42, 0x5a, 0x68, 0x39}, FormatBz2},
		{"unknown", []byte{0x00, 0x01, 0x02, 0x03}, FormatNone},
		{"empty", []byte{}, FormatNone},
		{"short", []byte{0x1f}, FormatNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectFromReader(bytes.NewReader(tc.header))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectFromPath_CompoundArchiveSkipped(t *testing.T) {
	// A real .tar.gz file has gzip magic bytes but the compound-archive
	// guard must still return FormatNone so the engine skips it.
	dir := t.TempDir()
	p := filepath.Join(dir, "foo.tar.gz")
	if err := os.WriteFile(p, []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectFromPath(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != FormatNone {
		t.Errorf("compound archive must yield FormatNone, got %q", got)
	}
}

func TestDetectFromPath_ExtensionWins(t *testing.T) {
	// If extension says gzip, we trust it without reading magic bytes.
	// Write garbage content; detection should still say gzip.
	dir := t.TempDir()
	p := filepath.Join(dir, "foo.gz")
	if err := os.WriteFile(p, []byte("not actually gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectFromPath(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != FormatGzip {
		t.Errorf("got %q, want %q", got, FormatGzip)
	}
}

func TestDetectFromPath_FallbackToMagicBytes(t *testing.T) {
	// Extension says nothing; magic bytes say gzip.
	dir := t.TempDir()
	p := filepath.Join(dir, "data.log")
	if err := os.WriteFile(p, []byte{0x1f, 0x8b, 0x08, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectFromPath(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != FormatGzip {
		t.Errorf("expected FormatGzip from magic bytes, got %q", got)
	}
}

func TestDetectFromPath_MissingFileIsError(t *testing.T) {
	_, err := DetectFromPath("/nonexistent/path/foo.log")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestIsCompressed(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.log")
	gz := filepath.Join(dir, "data.gz")
	os.WriteFile(plain, []byte("hello"), 0o644)
	os.WriteFile(gz, []byte{0x1f, 0x8b, 0x00}, 0o644)

	if IsCompressed(plain) {
		t.Error("plain.log should not be compressed")
	}
	if !IsCompressed(gz) {
		t.Error("data.gz should be compressed")
	}
	// Nonexistent → false (err swallowed).
	if IsCompressed("/nonexistent") {
		t.Error("nonexistent should yield false")
	}
}

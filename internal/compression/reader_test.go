package compression

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testContent = "line one\nline two\nline three\nERROR something happened\nline five\n"

// createGzipFile compresses testContent into a .gz file and returns its path.
func createGzipFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "test.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := gzip.NewWriter(f)
	_, err = w.Write([]byte(testContent))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return path
}

// createZstdFile compresses testContent into a .zst file and returns its path.
func createZstdFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "test.zst")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w, err := zstd.NewWriter(f)
	require.NoError(t, err)
	_, err = w.Write([]byte(testContent))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return path
}

// createBZ2File compresses testContent into a .bz2 file using the compress command.
// Since Go's stdlib only has a bzip2 reader (no writer), we create a minimal valid bz2
// by using the reader round-trip: compress with an external approach or use a known fixture.
// For simplicity, we test bz2 detection + reader using a programmatically created fixture.
func createBZ2File(t *testing.T, dir string) string {
	t.Helper()
	// The stdlib only has bzip2.NewReader, no writer. We'll use dsnet/compress/bzip2
	// or skip if not available. For testing purposes, create a bz2 file via shell.
	path := filepath.Join(dir, "test.bz2")

	// Write the uncompressed content, then compress with bzip2 command.
	srcPath := filepath.Join(dir, "test_bz2_src.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte(testContent), 0644))

	// Check if bzip2 is available.
	if _, err := lookPath("bzip2"); err != nil {
		t.Skip("bzip2 command not available")
	}

	cmd := execCommand("bzip2", "-k", "-f", srcPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("bzip2 compression failed: %s: %s", err, out)
	}

	// bzip2 -k creates test_bz2_src.txt.bz2
	bz2Path := srcPath + ".bz2"
	// Rename to expected path.
	require.NoError(t, os.Rename(bz2Path, path))

	return path
}

// createXZFile compresses testContent into a .xz file using the xz command.
func createXZFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "test.xz")

	srcPath := filepath.Join(dir, "test_xz_src.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte(testContent), 0644))

	if _, err := lookPath("xz"); err != nil {
		t.Skip("xz command not available")
	}

	cmd := execCommand("xz", "-k", "-f", srcPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("xz compression failed: %s: %s", err, out)
	}

	xzPath := srcPath + ".xz"
	require.NoError(t, os.Rename(xzPath, path))

	return path
}

func TestNewReader_Gzip(t *testing.T) {
	dir := t.TempDir()
	path := createGzipFile(t, dir)

	rc, format, err := NewReader(path)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, FormatGzip, format)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, testContent, string(data))
}

func TestNewReader_Zstd(t *testing.T) {
	dir := t.TempDir()
	path := createZstdFile(t, dir)

	rc, format, err := NewReader(path)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, FormatZstd, format)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, testContent, string(data))
}

func TestNewReader_BZ2(t *testing.T) {
	dir := t.TempDir()
	path := createBZ2File(t, dir)

	rc, format, err := NewReader(path)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, FormatBZ2, format)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, testContent, string(data))
}

func TestNewReader_XZ(t *testing.T) {
	dir := t.TempDir()
	path := createXZFile(t, dir)

	rc, format, err := NewReader(path)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, FormatXZ, format)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, testContent, string(data))
}

func TestNewReader_PlainTextReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "plain.txt", []byte("hello"))

	_, _, err := NewReader(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not compressed")
}

func TestNewReader_NonexistentFile(t *testing.T) {
	_, _, err := NewReader("/nonexistent/file.gz")
	assert.Error(t, err)
}

func TestNewReader_GzipRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Compress.
	original := "Hello, World!\nThis is a test.\nLine three.\n"
	gzPath := filepath.Join(dir, "roundtrip.gz")

	f, err := os.Create(gzPath)
	require.NoError(t, err)
	w := gzip.NewWriter(f)
	_, err = w.Write([]byte(original))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, f.Close())

	// Decompress via NewReader.
	rc, _, err := NewReader(gzPath)
	require.NoError(t, err)
	defer rc.Close()

	var buf bytes.Buffer
	_, err = io.Copy(&buf, rc)
	require.NoError(t, err)
	assert.Equal(t, original, buf.String())
}

func TestNewReader_ZstdRoundTrip(t *testing.T) {
	dir := t.TempDir()

	original := "Zstd round trip test.\nMultiple lines.\nEnd.\n"
	zstPath := filepath.Join(dir, "roundtrip.zst")

	f, err := os.Create(zstPath)
	require.NoError(t, err)
	enc, err := zstd.NewWriter(f)
	require.NoError(t, err)
	_, err = enc.Write([]byte(original))
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	require.NoError(t, f.Close())

	rc, _, err := NewReader(zstPath)
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

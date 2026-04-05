package compression

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestFile creates a temporary file with the given content and returns its path.
func writeTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0644))
	return path
}

// --- Magic byte detection tests ---

func TestDetect_GzipMagic(t *testing.T) {
	dir := t.TempDir()
	// Gzip magic (1f 8b) followed by arbitrary bytes.
	data := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00}
	path := writeTestFile(t, dir, "test.dat", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatGzip, format)
}

func TestDetect_XZMagic(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00, 0x01, 0x02}
	path := writeTestFile(t, dir, "test.dat", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatXZ, format)
}

func TestDetect_BZ2Magic(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0x42, 0x5a, 0x68, 0x39, 0x01, 0x02}
	path := writeTestFile(t, dir, "test.dat", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatBZ2, format)
}

func TestDetect_ZstdMagic(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00}
	path := writeTestFile(t, dir, "test.dat", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatZstd, format)
}

func TestDetect_PlainText(t *testing.T) {
	dir := t.TempDir()
	data := []byte("hello world\nthis is plain text\n")
	path := writeTestFile(t, dir, "test.txt", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

// --- Extension fallback tests ---

func TestDetect_GzipExtension(t *testing.T) {
	dir := t.TempDir()
	// File has .gz extension but no magic bytes (empty-ish file with text).
	data := []byte("not really gzip")
	path := writeTestFile(t, dir, "test.gz", data)

	format, err := Detect(path)
	require.NoError(t, err)
	// Magic bytes don't match, so extension fallback kicks in.
	assert.Equal(t, FormatGzip, format)
}

func TestDetect_XZExtension(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "test.xz", []byte("not xz"))

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatXZ, format)
}

func TestDetect_BZ2Extension(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "test.bz2", []byte("not bz2"))

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatBZ2, format)
}

func TestDetect_ZstExtension(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "test.zst", []byte("not zst"))

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatZstd, format)
}

func TestDetect_ZstdExtension(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "test.zstd", []byte("not zstd"))

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatZstd, format)
}

// --- Seekable zstd detection ---

func TestDetect_SeekableZstd(t *testing.T) {
	dir := t.TempDir()

	// Build a minimal file with zstd magic header and seekable footer.
	var data []byte
	// Zstd magic at the start.
	data = append(data, 0x28, 0xb5, 0x2f, 0xfd)
	// Pad to make room for footer.
	data = append(data, make([]byte, 100)...)
	// Seekable footer (last 9 bytes): magic(4) + num_frames(4) + flags(1)
	var footer [9]byte
	binary.LittleEndian.PutUint32(footer[0:4], SeekTableFooterMagic)
	binary.LittleEndian.PutUint32(footer[4:8], 1) // 1 frame
	footer[8] = 0                                   // no checksums
	data = append(data, footer[:]...)

	path := writeTestFile(t, dir, "test.zst", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatSeekableZstd, format)
}

func TestDetect_ZstdWithoutSeekableFooter(t *testing.T) {
	dir := t.TempDir()
	// Zstd magic but no seekable footer.
	data := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	path := writeTestFile(t, dir, "test.zst", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatZstd, format)
}

// --- Compound archive exclusion ---

func TestDetect_TarGzExcluded(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0x1f, 0x8b, 0x08, 0x00} // gzip magic
	path := writeTestFile(t, dir, "archive.tar.gz", data)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format, "compound archives should be excluded")
}

func TestDetect_TgzExcluded(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "archive.tgz", []byte{0x1f, 0x8b})

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

func TestDetect_TarBz2Excluded(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "archive.tar.bz2", []byte{0x42, 0x5a, 0x68})

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

func TestDetect_TarXzExcluded(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "archive.tar.xz", []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00})

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

func TestDetect_TarZstExcluded(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "archive.tar.zst", []byte{0x28, 0xb5, 0x2f, 0xfd})

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

// --- IsCompressed convenience ---

func TestIsCompressed_True(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "test.gz", []byte{0x1f, 0x8b, 0x08})
	assert.True(t, IsCompressed(path))
}

func TestIsCompressed_False(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "test.txt", []byte("plain text"))
	assert.False(t, IsCompressed(path))
}

// --- Nonexistent file ---

func TestDetect_NonexistentFile(t *testing.T) {
	format, err := Detect("/nonexistent/path/to/file.txt")
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

func TestDetect_NonexistentFileWithExtension(t *testing.T) {
	// Can't open file, so magic detection fails, but extension fallback works.
	format, err := Detect("/nonexistent/path/to/file.gz")
	require.NoError(t, err)
	assert.Equal(t, FormatGzip, format)
}

// --- Format.String() ---

func TestCompressionFormat_String(t *testing.T) {
	tests := []struct {
		format CompressionFormat
		want   string
	}{
		{FormatNone, "none"},
		{FormatGzip, "gzip"},
		{FormatXZ, "xz"},
		{FormatBZ2, "bz2"},
		{FormatZstd, "zstd"},
		{FormatSeekableZstd, "seekable_zstd"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.format.String())
	}
}

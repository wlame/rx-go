package fileutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx/internal/config"
)

// makeTestTree creates a temporary directory tree with text, binary, and compressed files
// for scanner tests. Returns the root directory path.
func makeTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Text files.
	require.NoError(t, os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hello world\n"), 0644))

	sub := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "main.go"), []byte("package main\n"), 0644))

	deep := filepath.Join(sub, "pkg", "util")
	require.NoError(t, os.MkdirAll(deep, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(deep, "helpers.go"), []byte("package util\n"), 0644))

	// Binary file (contains null bytes).
	require.NoError(t, os.WriteFile(filepath.Join(root, "image.bin"), []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00}, 0644))

	// Hidden directory with a file inside — should be skipped.
	hidden := filepath.Join(root, ".hidden")
	require.NoError(t, os.MkdirAll(hidden, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hidden, "secret.txt"), []byte("secret\n"), 0644))

	// node_modules directory — should be skipped.
	nm := filepath.Join(root, "node_modules")
	require.NoError(t, os.MkdirAll(nm, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "dep.js"), []byte("module.exports = {};\n"), 0644))

	// Gzip-magic file (just the header to trigger compression detection).
	require.NoError(t, os.WriteFile(filepath.Join(root, "data.gz"), []byte{0x1f, 0x8b, 0x08, 0x00}, 0644))

	return root
}

func defaultCfg() config.Config {
	return config.Config{MaxFiles: 1000}
}

func TestScanDirectory_WalksTree(t *testing.T) {
	root := makeTestTree(t)
	files, skipped, err := ScanDirectory(root, defaultCfg())
	require.NoError(t, err)

	// Expect: readme.txt, main.go, helpers.go, data.gz (compressed = not skipped).
	// Skipped: image.bin.
	// NOT included: .hidden/secret.txt, node_modules/dep.js.
	assert.Len(t, files, 4, "expected 4 processable files (3 text + 1 compressed)")
	assert.Len(t, skipped, 1, "expected 1 skipped binary file")

	// Verify the binary file was skipped.
	assert.Contains(t, skipped[0], "image.bin")
}

func TestScanDirectory_SkipsHiddenDirs(t *testing.T) {
	root := makeTestTree(t)
	files, _, err := ScanDirectory(root, defaultCfg())
	require.NoError(t, err)

	for _, f := range files {
		assert.NotContains(t, f.Path, ".hidden", "hidden directory files should not appear")
		assert.NotContains(t, f.Path, "node_modules", "node_modules files should not appear")
	}
}

func TestScanDirectory_MaxFilesLimit(t *testing.T) {
	root := makeTestTree(t)
	cfg := config.Config{MaxFiles: 2}

	files, skipped, err := ScanDirectory(root, cfg)
	require.NoError(t, err)

	total := len(files) + len(skipped)
	assert.LessOrEqual(t, total, 2, "total files should not exceed MaxFiles limit")
}

func TestScanDirectory_ClassifiesCompressedFiles(t *testing.T) {
	root := makeTestTree(t)
	files, _, err := ScanDirectory(root, defaultCfg())
	require.NoError(t, err)

	var found bool
	for _, f := range files {
		if filepath.Base(f.Path) == "data.gz" {
			found = true
			assert.Equal(t, ClassCompressed, f.Classification)
			assert.Equal(t, CompressionGzip, f.CompressionFormat)
		}
	}
	assert.True(t, found, "data.gz should be found in scan results")
}

func TestScanDirectory_EmptyDirectory(t *testing.T) {
	root := t.TempDir()
	files, skipped, err := ScanDirectory(root, defaultCfg())
	require.NoError(t, err)
	assert.Empty(t, files)
	assert.Empty(t, skipped)
}

func TestScanDirectory_NonExistentDirectory(t *testing.T) {
	_, _, err := ScanDirectory("/nonexistent/path/that/does/not/exist", defaultCfg())
	assert.Error(t, err)
}

// --- Binary detection tests ---

func TestContainsNull_TextContent(t *testing.T) {
	assert.False(t, containsNull([]byte("hello world\nfoo bar\n")))
}

func TestContainsNull_BinaryContent(t *testing.T) {
	assert.True(t, containsNull([]byte{0x89, 0x50, 0x4E, 0x47, 0x00}))
}

func TestContainsNull_EmptySlice(t *testing.T) {
	assert.False(t, containsNull([]byte{}))
}

func TestContainsNull_NullAtEnd(t *testing.T) {
	assert.True(t, containsNull([]byte("text content\x00")))
}

// --- Compression detection tests ---

func TestDetectCompression_AllFormats(t *testing.T) {
	tests := []struct {
		name   string
		data   []byte
		expect CompressionFormat
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, CompressionGzip},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}, CompressionZstd},
		{"xz", []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, CompressionXZ},
		{"bz2", []byte{0x42, 0x5a, 0x68, 0x39}, CompressionBZ2},
		{"plain text", []byte("hello world"), CompressionNone},
		{"empty", []byte{}, CompressionNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, DetectCompression(tt.data))
		})
	}
}

func TestDetectCompressionByPath(t *testing.T) {
	dir := t.TempDir()

	// Write a gzip-magic file.
	gzPath := filepath.Join(dir, "test.gz")
	require.NoError(t, os.WriteFile(gzPath, []byte{0x1f, 0x8b, 0x08, 0x00}, 0644))

	fmt, err := DetectCompressionByPath(gzPath)
	require.NoError(t, err)
	assert.Equal(t, CompressionGzip, fmt)

	// Write a plain text file.
	txtPath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(txtPath, []byte("plain text\n"), 0644))

	fmt, err = DetectCompressionByPath(txtPath)
	require.NoError(t, err)
	assert.Equal(t, CompressionNone, fmt)
}

// --- File classification tests ---

func TestClassifyFile_TextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "text.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello\nworld\n"), 0644))

	fi, isBinary := classifyFile(path, 12)
	assert.False(t, isBinary)
	assert.Equal(t, ClassText, fi.Classification)
	assert.Equal(t, CompressionNone, fi.CompressionFormat)
}

func TestClassifyFile_BinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.dat")
	require.NoError(t, os.WriteFile(path, []byte{0x00, 0x01, 0x02}, 0644))

	fi, isBinary := classifyFile(path, 3)
	assert.True(t, isBinary)
	assert.Equal(t, ClassBinary, fi.Classification)
}

func TestClassifyFile_CompressedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.zst")
	require.NoError(t, os.WriteFile(path, []byte{0x28, 0xb5, 0x2f, 0xfd, 0x04}, 0644))

	fi, isBinary := classifyFile(path, 5)
	assert.False(t, isBinary, "compressed files should not be marked as skipped")
	assert.Equal(t, ClassCompressed, fi.Classification)
	assert.Equal(t, CompressionZstd, fi.CompressionFormat)
}

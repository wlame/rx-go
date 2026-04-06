package fileutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx/internal/config"
)

func TestValidateFile_ValidTextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.txt")
	require.NoError(t, os.WriteFile(path, []byte("content\n"), 0644))

	err := ValidateFile(path)
	assert.NoError(t, err)
}

func TestValidateFile_NonExistentFile(t *testing.T) {
	err := ValidateFile("/nonexistent/path/file.txt")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "file not found")
}

func TestValidateFile_Directory(t *testing.T) {
	dir := t.TempDir()
	err := ValidateFile(dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a file")
}

func TestValidateFile_PermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission tests unreliable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("permission tests unreliable when running as root")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "noperm.txt")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0000))

	err := ValidateFile(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}

// --- IsTextFile tests ---

func TestIsTextFile_TextContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "text.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world\nline two\n"), 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.True(t, isText)
}

func TestIsTextFile_BinaryContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.dat")
	require.NoError(t, os.WriteFile(path, []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x01}, 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.False(t, isText)
}

func TestIsTextFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	require.NoError(t, os.WriteFile(path, []byte{}, 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.True(t, isText, "empty file should be considered text")
}

func TestIsTextFile_NullByteAtEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tricky.txt")
	require.NoError(t, os.WriteFile(path, append([]byte("looks like text"), 0x00), 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.False(t, isText, "file with null byte should be detected as binary")
}

func TestIsTextFile_NonExistent(t *testing.T) {
	_, err := IsTextFile("/nonexistent/file.txt")
	assert.Error(t, err)
}

func TestIsTextFile_LargeTextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")

	// Create a file larger than 8KB — only the first 8KB is checked.
	data := make([]byte, 16*1024)
	for i := range data {
		data[i] = 'A' + byte(i%26)
	}
	require.NoError(t, os.WriteFile(path, data, 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.True(t, isText)
}

func TestIsTextFile_NullByteAfter8KB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sneaky.txt")

	// First 8KB is text, null byte at position 8193.
	data := make([]byte, 10*1024)
	for i := range data {
		data[i] = 'x'
	}
	data[8193] = 0x00
	require.NoError(t, os.WriteFile(path, data, 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.True(t, isText, "null byte beyond 8KB should not be detected")
}

// --- ValidateFileSize tests ---

func TestValidateFileSize_WithinLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	require.NoError(t, os.WriteFile(path, []byte("small content"), 0644))

	cfg := config.Config{LargeFileMB: 50}
	err := ValidateFileSize(path, cfg)
	assert.NoError(t, err)
}

func TestValidateFileSize_ExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")

	// Create a 2MB file.
	data := make([]byte, 2*1024*1024)
	require.NoError(t, os.WriteFile(path, data, 0644))

	// Set limit to 1MB.
	cfg := config.Config{LargeFileMB: 1}
	err := ValidateFileSize(path, cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds size limit")
}

func TestValidateFileSize_ExactlyAtLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact.txt")

	// Create a file exactly at the 1MB limit.
	data := make([]byte, 1*1024*1024)
	require.NoError(t, os.WriteFile(path, data, 0644))

	cfg := config.Config{LargeFileMB: 1}
	err := ValidateFileSize(path, cfg)
	assert.NoError(t, err, "file exactly at limit should pass")
}

func TestValidateFileSize_NonExistentFile(t *testing.T) {
	cfg := config.Config{LargeFileMB: 50}
	err := ValidateFileSize("/nonexistent/file.txt", cfg)
	assert.Error(t, err)
}

// --- Binary file with .txt extension ---

func TestIsTextFile_BinaryWithTxtExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sneaky.txt")

	// File has .txt extension but contains null bytes — should be classified as binary.
	data := []byte("looks like text but has\x00null bytes inside\n")
	require.NoError(t, os.WriteFile(path, data, 0644))

	isText, err := IsTextFile(path)
	require.NoError(t, err)
	assert.False(t, isText, "file with null bytes should be binary regardless of .txt extension")
}

// --- Symlink to text file ---

func TestIsTextFile_SymlinkToTextFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks may require special privileges on Windows")
	}

	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(realPath, []byte("real content\n"), 0644))

	linkPath := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(realPath, linkPath))

	isText, err := IsTextFile(linkPath)
	require.NoError(t, err)
	assert.True(t, isText, "symlink to text file should be classified as text")
}

func TestIsTextFile_SymlinkToBinaryFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks may require special privileges on Windows")
	}

	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.bin")
	require.NoError(t, os.WriteFile(realPath, []byte{0x89, 0x50, 0x00, 0x47}, 0644))

	linkPath := filepath.Join(dir, "link.bin")
	require.NoError(t, os.Symlink(realPath, linkPath))

	isText, err := IsTextFile(linkPath)
	require.NoError(t, err)
	assert.False(t, isText, "symlink to binary file should be classified as binary")
}

// --- ClassifyFile: wrong extension (magic bytes should win) ---

func TestClassifyFile_GzipMagicWithTxtExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")

	// File has .txt extension but gzip magic bytes — compression detection should win.
	data := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00}
	require.NoError(t, os.WriteFile(path, data, 0644))

	fi := ClassifyFile(path, int64(len(data)))
	assert.Equal(t, ClassCompressed, fi.Classification, "gzip magic should override .txt extension")
	assert.Equal(t, CompressionGzip, fi.CompressionFormat)
}

// --- ClassifyFile: empty gzip file (just the 2-byte header) ---

func TestClassifyFile_EmptyGzipFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.gz")

	// Valid gzip header (2 magic bytes) with no payload.
	data := []byte{0x1f, 0x8b}
	require.NoError(t, os.WriteFile(path, data, 0644))

	fi := ClassifyFile(path, int64(len(data)))
	assert.Equal(t, ClassCompressed, fi.Classification, "file with gzip magic should be compressed even if empty")
	assert.Equal(t, CompressionGzip, fi.CompressionFormat)
}

// --- ClassifyFile: binary despite .txt extension ---

func TestClassifyFile_BinaryContentTxtExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.txt")

	// Null bytes in content — binary regardless of extension.
	data := []byte("text\x00more\x00binary\n")
	require.NoError(t, os.WriteFile(path, data, 0644))

	fi := ClassifyFile(path, int64(len(data)))
	assert.Equal(t, ClassBinary, fi.Classification, "null bytes should force binary classification")
}

// --- ScanDirectory: hidden files skipped ---

func TestScanDirectory_HiddenFilesSkipped(t *testing.T) {
	dir := t.TempDir()

	// Create a hidden file directly in the root (not in a hidden directory).
	// Note: ScanDirectory skips hidden directories, not hidden files.
	// However, files starting with . inside hidden directories are also skipped.
	hiddenDir := filepath.Join(dir, ".config")
	require.NoError(t, os.MkdirAll(hiddenDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hiddenDir, "settings.txt"), []byte("key=value\n"), 0644))

	// Also create a visible file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("visible\n"), 0644))

	cfg := config.Config{MaxFiles: 1000}
	files, _, err := ScanDirectory(dir, cfg)
	require.NoError(t, err)

	// Only visible.txt should be found — .config/ directory is hidden and skipped.
	assert.Len(t, files, 1)
	assert.Contains(t, files[0].Path, "visible.txt")

	for _, f := range files {
		assert.NotContains(t, f.Path, ".config", "hidden directory contents should be skipped")
	}
}

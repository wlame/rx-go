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

package compression

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetector_DetectByMagic(t *testing.T) {
	detector := NewDetector()

	tests := []struct {
		name     string
		magic    []byte
		expected Format
	}{
		{
			name:     "gzip",
			magic:    []byte{0x1f, 0x8b, 0x08, 0x00},
			expected: FormatGzip,
		},
		{
			name:     "zstd",
			magic:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00},
			expected: FormatZstd,
		},
		{
			name:     "bzip2",
			magic:    []byte{0x42, 0x5a, 0x68, 0x39},
			expected: FormatBzip2,
		},
		{
			name:     "xz",
			magic:    []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00},
			expected: FormatXz,
		},
		{
			name:     "lz4",
			magic:    []byte{0x04, 0x22, 0x4d, 0x18},
			expected: FormatLz4,
		},
		{
			name:     "plain text",
			magic:    []byte("This is plain text"),
			expected: FormatNone,
		},
		{
			name:     "empty",
			magic:    []byte{},
			expected: FormatNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detector.detectByMagic(tt.magic)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDetector_DetectByExtension(t *testing.T) {
	detector := NewDetector()

	tests := []struct {
		ext      string
		expected Format
	}{
		{".gz", FormatGzip},
		{".gzip", FormatGzip},
		{".zst", FormatZstd},
		{".zstd", FormatZstd},
		{".bz2", FormatBzip2},
		{".bzip2", FormatBzip2},
		{".xz", FormatXz},
		{".lz4", FormatLz4},
		{".txt", FormatNone},
		{".log", FormatNone},
		{"", FormatNone},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			result := detector.detectByExtension(tt.ext)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDetector_DetectFile_Gzip(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.gz")

	// Create gzip file with magic bytes
	content := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	err := os.WriteFile(testFile, content, 0644)
	require.NoError(t, err)

	detector := NewDetector()
	format, err := detector.DetectFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, FormatGzip, format)
}

func TestDetector_DetectFile_PlainText(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	content := []byte("This is plain text")
	err := os.WriteFile(testFile, content, 0644)
	require.NoError(t, err)

	detector := NewDetector()
	format, err := detector.DetectFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, FormatNone, format)
}

func TestDetector_DetectFile_WrongExtension(t *testing.T) {
	tmpDir := t.TempDir()
	// File has .txt extension but gzip magic bytes
	testFile := filepath.Join(tmpDir, "test.txt")

	content := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	err := os.WriteFile(testFile, content, 0644)
	require.NoError(t, err)

	detector := NewDetector()
	format, err := detector.DetectFile(testFile)
	require.NoError(t, err)
	// Should trust magic bytes over extension
	assert.Equal(t, FormatGzip, format)
}

func TestDetector_IsCompressed(t *testing.T) {
	tmpDir := t.TempDir()

	// Compressed file
	gzipFile := filepath.Join(tmpDir, "test.gz")
	gzipContent := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	err := os.WriteFile(gzipFile, gzipContent, 0644)
	require.NoError(t, err)

	// Plain file
	plainFile := filepath.Join(tmpDir, "test.txt")
	plainContent := []byte("plain text")
	err = os.WriteFile(plainFile, plainContent, 0644)
	require.NoError(t, err)

	detector := NewDetector()

	assert.True(t, detector.IsCompressed(gzipFile))
	assert.False(t, detector.IsCompressed(plainFile))
}

func TestFormat_IsCompressed(t *testing.T) {
	tests := []struct {
		format   Format
		expected bool
	}{
		{FormatNone, false},
		{FormatUnknown, false},
		{FormatGzip, true},
		{FormatZstd, true},
		{FormatBzip2, true},
		{FormatXz, true},
		{FormatLz4, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.format.IsCompressed())
		})
	}
}

func TestFormat_DecompressorCommand(t *testing.T) {
	tests := []struct {
		format   Format
		expected string
	}{
		{FormatGzip, "gzip"},
		{FormatZstd, "zstd"},
		{FormatBzip2, "bzip2"},
		{FormatXz, "xz"},
		{FormatLz4, "lz4"},
		{FormatNone, ""},
		{FormatUnknown, ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.format.DecompressorCommand())
		})
	}
}

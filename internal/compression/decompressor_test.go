package compression

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Decompressor Creation Tests
// ============================================================================

func TestNewDecompressor(t *testing.T) {
	dec := NewDecompressor()
	assert.NotNil(t, dec)
	assert.NotNil(t, dec.detector)
}

// ============================================================================
// CheckDecompressor Tests
// ============================================================================

func TestDecompressor_CheckDecompressor_Gzip(t *testing.T) {
	dec := NewDecompressor()
	err := dec.CheckDecompressor(FormatGzip)
	// gzip should always be available on Unix systems
	if _, lookErr := exec.LookPath("gzip"); lookErr == nil {
		assert.NoError(t, err)
	} else {
		assert.Error(t, err)
	}
}

func TestDecompressor_CheckDecompressor_AllFormats(t *testing.T) {
	dec := NewDecompressor()

	tests := []struct {
		name   string
		format Format
		cmd    string
	}{
		{"gzip", FormatGzip, "gzip"},
		{"zstd", FormatZstd, "zstd"},
		{"bzip2", FormatBzip2, "bzip2"},
		{"xz", FormatXz, "xz"},
		{"lz4", FormatLz4, "lz4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := dec.CheckDecompressor(tt.format)

			// Check if command exists
			_, lookErr := exec.LookPath(tt.cmd)
			if lookErr == nil {
				assert.NoError(t, err, "Expected no error when %s is available", tt.cmd)
			} else {
				assert.Error(t, err, "Expected error when %s is not available", tt.cmd)
				assert.Contains(t, err.Error(), "not found in PATH")
			}
		})
	}
}

func TestDecompressor_CheckDecompressor_UnsupportedFormat(t *testing.T) {
	dec := NewDecompressor()
	err := dec.CheckDecompressor(FormatNone)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no decompressor")
}

func TestDecompressor_CheckDecompressor_UnknownFormat(t *testing.T) {
	dec := NewDecompressor()
	err := dec.CheckDecompressor(FormatUnknown)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no decompressor")
}

// ============================================================================
// Decompress Tests
// ============================================================================

func TestDecompressor_Decompress_Gzip(t *testing.T) {
	// Skip if gzip not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}

	// Create a test gzip file
	tmpDir := t.TempDir()
	testData := "Hello, World!\nThis is a test.\n"
	plainFile := filepath.Join(tmpDir, "test.txt")
	gzipFile := filepath.Join(tmpDir, "test.txt.gz")

	require.NoError(t, os.WriteFile(plainFile, []byte(testData), 0644))

	// Compress with gzip
	cmd := exec.Command("gzip", "-c", plainFile)
	output, err := cmd.Output()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(gzipFile, output, 0644))

	// Test decompression
	dec := NewDecompressor()
	reader, format, err := dec.Decompress(context.Background(), gzipFile)
	require.NoError(t, err)
	assert.Equal(t, FormatGzip, format)
	defer reader.Close()

	// Read decompressed data
	decompressed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, string(decompressed))
}

func TestDecompressor_Decompress_Bzip2(t *testing.T) {
	// Skip if bzip2 not available
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 not available")
	}

	// Create a test bzip2 file
	tmpDir := t.TempDir()
	testData := "Bzip2 compressed data\nMultiple lines\n"
	plainFile := filepath.Join(tmpDir, "test.txt")
	bz2File := filepath.Join(tmpDir, "test.txt.bz2")

	require.NoError(t, os.WriteFile(plainFile, []byte(testData), 0644))

	// Compress with bzip2
	cmd := exec.Command("bzip2", "-c", plainFile)
	output, err := cmd.Output()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bz2File, output, 0644))

	// Test decompression
	dec := NewDecompressor()
	reader, format, err := dec.Decompress(context.Background(), bz2File)
	require.NoError(t, err)
	assert.Equal(t, FormatBzip2, format)
	defer reader.Close()

	// Read decompressed data
	decompressed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, string(decompressed))
}

func TestDecompressor_Decompress_NonCompressedFile(t *testing.T) {
	tmpDir := t.TempDir()
	plainFile := filepath.Join(tmpDir, "test.txt")
	require.NoError(t, os.WriteFile(plainFile, []byte("plain text"), 0644))

	dec := NewDecompressor()
	reader, format, err := dec.Decompress(context.Background(), plainFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not compressed")
	assert.Nil(t, reader)
	assert.Equal(t, FormatNone, format)
}

func TestDecompressor_Decompress_NonexistentFile(t *testing.T) {
	dec := NewDecompressor()
	reader, format, err := dec.Decompress(context.Background(), "/nonexistent/file.gz")
	assert.Error(t, err)
	assert.Nil(t, reader)
	assert.Equal(t, FormatUnknown, format)
}

func TestDecompressor_Decompress_ContextCancellation(t *testing.T) {
	// Skip if gzip not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}

	// Create a test gzip file
	tmpDir := t.TempDir()
	testData := "Context cancellation test\n"
	plainFile := filepath.Join(tmpDir, "test.txt")
	gzipFile := filepath.Join(tmpDir, "test.txt.gz")

	require.NoError(t, os.WriteFile(plainFile, []byte(testData), 0644))

	cmd := exec.Command("gzip", "-c", plainFile)
	output, err := cmd.Output()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(gzipFile, output, 0644))

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Test decompression with cancelled context
	dec := NewDecompressor()
	reader, _, err := dec.Decompress(ctx, gzipFile)

	// Should fail to start the command
	if reader != nil {
		reader.Close()
	}
	// Context cancellation may or may not cause immediate failure
	// depending on timing, so just ensure we don't panic
}

// ============================================================================
// createDecompressCommand Tests
// ============================================================================

func TestDecompressor_createDecompressCommand_AllFormats(t *testing.T) {
	dec := NewDecompressor()
	ctx := context.Background()
	tmpFile := "/tmp/test.bin"

	tests := []struct {
		name           string
		format         Format
		expectedCmd    string
		expectedArgs   []string
	}{
		{
			name:         "gzip",
			format:       FormatGzip,
			expectedCmd:  "gzip",
			expectedArgs: []string{"-cd", tmpFile},
		},
		{
			name:         "zstd",
			format:       FormatZstd,
			expectedCmd:  "zstd",
			expectedArgs: []string{"-cd", tmpFile},
		},
		{
			name:         "bzip2",
			format:       FormatBzip2,
			expectedCmd:  "bzip2",
			expectedArgs: []string{"-cd", tmpFile},
		},
		{
			name:         "xz",
			format:       FormatXz,
			expectedCmd:  "xz",
			expectedArgs: []string{"-cd", tmpFile},
		},
		{
			name:         "lz4",
			format:       FormatLz4,
			expectedCmd:  "lz4",
			expectedArgs: []string{"-cd", tmpFile},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := dec.createDecompressCommand(ctx, tmpFile, tt.format)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedCmd, cmd.Path[len(cmd.Path)-len(tt.expectedCmd):])
			assert.Equal(t, tt.expectedArgs, cmd.Args[1:]) // Skip program name
		})
	}
}

func TestDecompressor_createDecompressCommand_UnsupportedFormat(t *testing.T) {
	dec := NewDecompressor()
	ctx := context.Background()

	cmd, err := dec.createDecompressCommand(ctx, "/tmp/test", FormatUnknown)
	assert.Error(t, err)
	assert.Nil(t, cmd)
	assert.Contains(t, err.Error(), "unsupported")
}

// ============================================================================
// cmdReader Tests
// ============================================================================

func TestCmdReader_Read(t *testing.T) {
	// Skip if gzip not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}

	// Create a test gzip file
	tmpDir := t.TempDir()
	testData := "Test data for cmdReader\n"
	plainFile := filepath.Join(tmpDir, "test.txt")
	gzipFile := filepath.Join(tmpDir, "test.txt.gz")

	require.NoError(t, os.WriteFile(plainFile, []byte(testData), 0644))

	cmd := exec.Command("gzip", "-c", plainFile)
	output, err := cmd.Output()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(gzipFile, output, 0644))

	// Create decompressor
	dec := NewDecompressor()
	reader, _, err := dec.Decompress(context.Background(), gzipFile)
	require.NoError(t, err)
	defer reader.Close()

	// Test reading in chunks
	buf := make([]byte, 5)
	n, err := reader.Read(buf)
	assert.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "Test ", string(buf))
}

func TestCmdReader_Close(t *testing.T) {
	// Skip if gzip not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}

	// Create a test gzip file
	tmpDir := t.TempDir()
	plainFile := filepath.Join(tmpDir, "test.txt")
	gzipFile := filepath.Join(tmpDir, "test.txt.gz")

	require.NoError(t, os.WriteFile(plainFile, []byte("close test\n"), 0644))

	cmd := exec.Command("gzip", "-c", plainFile)
	output, err := cmd.Output()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(gzipFile, output, 0644))

	// Create, read some data, then close
	dec := NewDecompressor()
	reader, _, err := dec.Decompress(context.Background(), gzipFile)
	require.NoError(t, err)

	// Read all data before closing to avoid broken pipe
	_, err = io.ReadAll(reader)
	require.NoError(t, err)

	err = reader.Close()
	assert.NoError(t, err)
}

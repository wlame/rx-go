package trace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Fixtures Setup
// ============================================================================

// createTestLogFile creates a test log file with sample data
func createTestLogFile(t *testing.T, tmpDir string) string {
	t.Helper()
	logData := `2024-01-01 10:00:00 INFO Application started
2024-01-01 10:00:01 DEBUG Loading configuration
2024-01-01 10:00:02 ERROR Failed to connect to database
2024-01-01 10:00:03 WARNING Retrying connection
2024-01-01 10:00:04 INFO Connection established
2024-01-01 10:00:05 DEBUG Processing request #1
2024-01-01 10:00:06 ERROR Invalid input: user_id missing
2024-01-01 10:00:07 INFO Request completed
`
	logFile := filepath.Join(tmpDir, "test.log")
	require.NoError(t, os.WriteFile(logFile, []byte(logData), 0644))
	return logFile
}

// compressFile compresses a file using the specified format
func compressFile(t *testing.T, srcFile string, format compression.Format) string {
	t.Helper()

	var compressedFile string
	var cmd *exec.Cmd

	switch format {
	case compression.FormatGzip:
		compressedFile = srcFile + ".gz"
		cmd = exec.Command("gzip", "-c", srcFile)
	case compression.FormatBzip2:
		compressedFile = srcFile + ".bz2"
		cmd = exec.Command("bzip2", "-c", srcFile)
	case compression.FormatZstd:
		compressedFile = srcFile + ".zst"
		cmd = exec.Command("zstd", "-c", srcFile)
	case compression.FormatXz:
		compressedFile = srcFile + ".xz"
		cmd = exec.Command("xz", "-c", srcFile)
	case compression.FormatLz4:
		compressedFile = srcFile + ".lz4"
		cmd = exec.Command("lz4", "-c", srcFile)
	default:
		t.Fatalf("unsupported format: %s", format)
	}

	output, err := cmd.Output()
	if err != nil {
		// Command not available, skip
		t.Skipf("%s not available", format.DecompressorCommand())
	}

	require.NoError(t, os.WriteFile(compressedFile, output, 0644))
	return compressedFile
}

// ============================================================================
// NewCompressedPipeline Tests
// ============================================================================

func TestNewCompressedPipeline(t *testing.T) {
	ctx := context.Background()
	pipeline := NewCompressedPipeline(
		ctx,
		"/path/to/file.gz",
		[]string{"ERROR"},
		true,
		compression.FormatGzip,
	)

	assert.NotNil(t, pipeline)
	assert.Equal(t, "/path/to/file.gz", pipeline.filePath)
	assert.Equal(t, []string{"ERROR"}, pipeline.patterns)
	assert.True(t, pipeline.caseSensitive)
	assert.Equal(t, compression.FormatGzip, pipeline.format)
}

// ============================================================================
// createDecompressCommand Tests
// ============================================================================

func TestCompressedPipeline_createDecompressCommand_AllFormats(t *testing.T) {
	ctx := context.Background()
	testFile := "/tmp/test.log"

	tests := []struct {
		name         string
		format       compression.Format
		expectedCmd  string
		expectedArgs []string
	}{
		{
			name:         "gzip",
			format:       compression.FormatGzip,
			expectedCmd:  "gzip",
			expectedArgs: []string{"-cd", testFile},
		},
		{
			name:         "zstd",
			format:       compression.FormatZstd,
			expectedCmd:  "zstd",
			expectedArgs: []string{"-cd", testFile},
		},
		{
			name:         "bzip2",
			format:       compression.FormatBzip2,
			expectedCmd:  "bzip2",
			expectedArgs: []string{"-cd", testFile},
		},
		{
			name:         "xz",
			format:       compression.FormatXz,
			expectedCmd:  "xz",
			expectedArgs: []string{"-cd", testFile},
		},
		{
			name:         "lz4",
			format:       compression.FormatLz4,
			expectedCmd:  "lz4",
			expectedArgs: []string{"-cd", testFile},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipeline := NewCompressedPipeline(ctx, testFile, []string{"test"}, false, tt.format)
			cmd, err := pipeline.createDecompressCommand()
			require.NoError(t, err)
			assert.Contains(t, cmd.Path, tt.expectedCmd)
			assert.Equal(t, tt.expectedArgs, cmd.Args[1:]) // Skip program name
		})
	}
}

func TestCompressedPipeline_createDecompressCommand_UnsupportedFormat(t *testing.T) {
	ctx := context.Background()
	pipeline := NewCompressedPipeline(ctx, "/tmp/test", []string{"test"}, false, compression.FormatUnknown)

	cmd, err := pipeline.createDecompressCommand()
	assert.Error(t, err)
	assert.Nil(t, cmd)
	assert.Contains(t, err.Error(), "unsupported")
}

// ============================================================================
// createRipgrepCommand Tests
// ============================================================================

func TestCompressedPipeline_createRipgrepCommand_CaseSensitive(t *testing.T) {
	ctx := context.Background()
	pipeline := NewCompressedPipeline(
		ctx,
		"/tmp/test.gz",
		[]string{"ERROR", "WARNING"},
		true, // case sensitive
		compression.FormatGzip,
	)

	cmd := pipeline.createRipgrepCommand()
	args := cmd.Args

	assert.Contains(t, args, "--case-sensitive")
	assert.Contains(t, args, "-e")
	assert.Contains(t, args, "ERROR")
	assert.Contains(t, args, "WARNING")
	assert.Contains(t, args, "--json")
	assert.Equal(t, "-", args[len(args)-1]) // Read from stdin
}

func TestCompressedPipeline_createRipgrepCommand_CaseInsensitive(t *testing.T) {
	ctx := context.Background()
	pipeline := NewCompressedPipeline(
		ctx,
		"/tmp/test.gz",
		[]string{"error"},
		false, // case insensitive
		compression.FormatGzip,
	)

	cmd := pipeline.createRipgrepCommand()
	args := cmd.Args

	assert.Contains(t, args, "--ignore-case")
	assert.NotContains(t, args, "--case-sensitive")
}

// ============================================================================
// SearchCompressed Integration Tests
// ============================================================================

func TestSearchCompressed_Gzip_FindMatches(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	gzipFile := compressFile(t, logFile, compression.FormatGzip)

	// Search for "ERROR" in compressed file
	ctx := context.Background()
	matches, err := SearchCompressed(ctx, gzipFile, []string{"ERROR"}, false, compression.FormatGzip)

	require.NoError(t, err)
	assert.Len(t, matches, 2, "Expected 2 ERROR matches")

	// Verify match content
	assert.Contains(t, matches[0].LineText, "ERROR")
	assert.Contains(t, matches[1].LineText, "ERROR")
}

func TestSearchCompressed_Bzip2_FindMatches(t *testing.T) {
	// Skip if bzip2 or rg not available
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	bz2File := compressFile(t, logFile, compression.FormatBzip2)

	// Search for "INFO" in compressed file
	ctx := context.Background()
	matches, err := SearchCompressed(ctx, bz2File, []string{"INFO"}, false, compression.FormatBzip2)

	require.NoError(t, err)
	assert.Len(t, matches, 3, "Expected 3 INFO matches")
}

func TestSearchCompressed_MultiplePatterns(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	gzipFile := compressFile(t, logFile, compression.FormatGzip)

	// Search for multiple patterns
	ctx := context.Background()
	matches, err := SearchCompressed(ctx, gzipFile, []string{"ERROR", "WARNING"}, false, compression.FormatGzip)

	require.NoError(t, err)
	assert.Len(t, matches, 3, "Expected 2 ERROR + 1 WARNING = 3 matches")
}

func TestSearchCompressed_NoMatches(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	gzipFile := compressFile(t, logFile, compression.FormatGzip)

	// Search for non-existent pattern
	ctx := context.Background()
	matches, err := SearchCompressed(ctx, gzipFile, []string{"NONEXISTENT"}, false, compression.FormatGzip)

	require.NoError(t, err)
	assert.Empty(t, matches, "Expected no matches")
}

func TestSearchCompressed_CaseSensitive(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	gzipFile := compressFile(t, logFile, compression.FormatGzip)

	// Case-sensitive search (should not match lowercase "error")
	ctx := context.Background()
	matches, err := SearchCompressed(ctx, gzipFile, []string{"error"}, true, compression.FormatGzip)

	require.NoError(t, err)
	assert.Empty(t, matches, "Expected no matches with case-sensitive search for lowercase 'error'")

	// Case-insensitive search (should match "ERROR")
	matches, err = SearchCompressed(ctx, gzipFile, []string{"error"}, false, compression.FormatGzip)

	require.NoError(t, err)
	assert.Len(t, matches, 2, "Expected 2 matches with case-insensitive search")
}

func TestSearchCompressed_NonexistentFile(t *testing.T) {
	ctx := context.Background()
	_, err := SearchCompressed(ctx, "/nonexistent/file.gz", []string{"ERROR"}, false, compression.FormatGzip)

	// Should error because file doesn't exist
	assert.Error(t, err)
}

func TestSearchCompressed_ContextCancellation(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	gzipFile := compressFile(t, logFile, compression.FormatGzip)

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Should fail or return quickly
	_, err := SearchCompressed(ctx, gzipFile, []string{"ERROR"}, false, compression.FormatGzip)

	// May or may not error depending on timing, but should not hang
	_ = err
}

// ============================================================================
// CompressedPipeline.Run Tests
// ============================================================================

func TestCompressedPipeline_Run_Success(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	logFile := createTestLogFile(t, tmpDir)
	gzipFile := compressFile(t, logFile, compression.FormatGzip)

	ctx := context.Background()
	pipeline := NewCompressedPipeline(ctx, gzipFile, []string{"DEBUG"}, false, compression.FormatGzip)

	matches, err := pipeline.Run()
	require.NoError(t, err)
	assert.Len(t, matches, 2, "Expected 2 DEBUG matches")
}

func TestCompressedPipeline_Run_DecompressorFailure(t *testing.T) {
	ctx := context.Background()
	// Use a non-existent file to trigger decompressor failure
	pipeline := NewCompressedPipeline(ctx, "/nonexistent/file.gz", []string{"test"}, false, compression.FormatGzip)

	matches, err := pipeline.Run()
	assert.Error(t, err)
	assert.Nil(t, matches)
}

// ============================================================================
// Edge Cases
// ============================================================================

func TestSearchCompressed_EmptyFile(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.log")
	require.NoError(t, os.WriteFile(emptyFile, []byte(""), 0644))
	gzipFile := compressFile(t, emptyFile, compression.FormatGzip)

	ctx := context.Background()
	matches, err := SearchCompressed(ctx, gzipFile, []string{"ERROR"}, false, compression.FormatGzip)

	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestSearchCompressed_LargeFile(t *testing.T) {
	// Skip if gzip or rg not available
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available")
	}

	tmpDir := t.TempDir()

	// Create a larger log file
	largeData := ""
	for i := 0; i < 1000; i++ {
		largeData += "2024-01-01 10:00:00 INFO Message " + string(rune('0'+i%10)) + "\n"
		if i%10 == 0 {
			largeData += "2024-01-01 10:00:00 ERROR Error " + string(rune('0'+i%10)) + "\n"
		}
	}

	largeFile := filepath.Join(tmpDir, "large.log")
	require.NoError(t, os.WriteFile(largeFile, []byte(largeData), 0644))
	gzipFile := compressFile(t, largeFile, compression.FormatGzip)

	ctx := context.Background()
	matches, err := SearchCompressed(ctx, gzipFile, []string{"ERROR"}, false, compression.FormatGzip)

	require.NoError(t, err)
	assert.Greater(t, len(matches), 90, "Expected at least 90 ERROR matches")
}

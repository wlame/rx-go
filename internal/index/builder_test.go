package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/models"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)
	cfg := config.Load()
	return &cfg
}

func TestBuildIndex_RegularFile(t *testing.T) {
	cfg := testConfig(t)

	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, "this is line number "+strings.Repeat("x", 20))
	}
	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	idx, err := BuildIndex(path, cfg)
	require.NoError(t, err)
	require.NotNil(t, idx)

	assert.Equal(t, UnifiedIndexVersion, idx.Version)
	assert.Equal(t, models.IndexTypeRegular, idx.IndexType)
	assert.NotEmpty(t, idx.LineIndex)
	// First entry should always be line 1 at offset 0.
	assert.Equal(t, []int{1, 0}, idx.LineIndex[0])

	// Analysis should be populated.
	require.NotNil(t, idx.Analysis)
	require.NotNil(t, idx.Analysis.LineCount)
	assert.Equal(t, 100, *idx.Analysis.LineCount)
	require.NotNil(t, idx.Analysis.LineEnding)
	assert.Equal(t, "LF", *idx.Analysis.LineEnding)
}

func TestBuildIndex_RegularFile_EmptyFile(t *testing.T) {
	cfg := testConfig(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))

	idx, err := BuildIndex(path, cfg)
	require.NoError(t, err)
	require.NotNil(t, idx)

	assert.Equal(t, models.IndexTypeRegular, idx.IndexType)
	assert.Equal(t, [][]int{{1, 0}}, idx.LineIndex)
}

func TestBuildIndex_RegularFile_VerifyLineOffsets(t *testing.T) {
	cfg := testConfig(t)

	dir := t.TempDir()
	content := "line1\nline2\nline3\nline4\nline5\n"
	path := filepath.Join(dir, "offsets.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	idx, err := BuildIndex(path, cfg)
	require.NoError(t, err)
	require.NotNil(t, idx)

	// First entry: line 1 at offset 0.
	assert.Equal(t, 1, idx.LineIndex[0][0])
	assert.Equal(t, 0, idx.LineIndex[0][1])
}

func TestBuildIndex_CRLF_LineEnding(t *testing.T) {
	cfg := testConfig(t)

	dir := t.TempDir()
	content := "line1\r\nline2\r\nline3\r\n"
	path := filepath.Join(dir, "crlf.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	idx, err := BuildIndex(path, cfg)
	require.NoError(t, err)
	require.NotNil(t, idx)
	require.NotNil(t, idx.Analysis)
	require.NotNil(t, idx.Analysis.LineEnding)
	assert.Equal(t, "CRLF", *idx.Analysis.LineEnding)
}

func TestBuildIndex_CompressedFile_Gzip(t *testing.T) {
	cfg := testConfig(t)

	dir := t.TempDir()
	gzPath := filepath.Join(dir, "test.log.gz")
	createGzipTestFile(t, gzPath, []byte("line1\nline2\nline3\n"))

	idx, err := BuildIndex(gzPath, cfg)
	require.NoError(t, err)
	require.NotNil(t, idx)

	assert.Equal(t, models.IndexTypeCompressed, idx.IndexType)
	require.NotNil(t, idx.TotalLines)
	assert.Equal(t, 3, *idx.TotalLines)
	require.NotNil(t, idx.CompressionFormat)
	assert.Equal(t, "gzip", *idx.CompressionFormat)
	assert.Equal(t, []int{1, 0}, idx.LineIndex[0])
}

func TestBuildIndex_DispatchesByFormat(t *testing.T) {
	cfg := testConfig(t)

	dir := t.TempDir()

	// Plain text file should get IndexTypeRegular.
	txtPath := filepath.Join(dir, "plain.log")
	require.NoError(t, os.WriteFile(txtPath, []byte("hello\n"), 0o644))
	idx, err := BuildIndex(txtPath, cfg)
	require.NoError(t, err)
	assert.Equal(t, models.IndexTypeRegular, idx.IndexType)

	// Gzip file should get IndexTypeCompressed.
	gzPath := filepath.Join(dir, "compressed.log.gz")
	createGzipTestFile(t, gzPath, []byte("hello\n"))
	idx, err = BuildIndex(gzPath, cfg)
	require.NoError(t, err)
	assert.Equal(t, models.IndexTypeCompressed, idx.IndexType)
}

func TestDetectLineEnding(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"LF_only", "line1\nline2\nline3\n", "LF"},
		{"CRLF_only", "line1\r\nline2\r\nline3\r\n", "CRLF"},
		{"mixed", "line1\nline2\r\nline3\n", "mixed"},
		{"empty", "", "LF"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+".txt")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))
			got := detectLineEnding(path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeAnalysis_EmptyData(t *testing.T) {
	a := computeAnalysis(nil, 0, 0, 0, 0, 0, "LF")
	require.NotNil(t, a)
	assert.Equal(t, 0, *a.LineCount)
	assert.Equal(t, 0.0, *a.LineLengthAvg)
}

func TestComputeAnalysis_WithData(t *testing.T) {
	lengths := []int{10, 20, 30, 40, 50}
	a := computeAnalysis(lengths, 2, 7, 50, 5, 100, "LF")
	require.NotNil(t, a)
	assert.Equal(t, 7, *a.LineCount)
	assert.Equal(t, 2, *a.EmptyLineCount)
	assert.Equal(t, 50, *a.LineLengthMax)
	assert.InDelta(t, 30.0, *a.LineLengthAvg, 0.01)
}

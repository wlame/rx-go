package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/compression"
	"github.com/wlame/rx/internal/config"
)

// testSearchContent is a small multi-line text with known patterns.
const testSearchContent = `2024-01-01 INFO starting service
2024-01-02 ERROR database connection failed
2024-01-03 WARN disk space low
2024-01-04 ERROR timeout reached
2024-01-05 INFO service recovered
`

// createGzipSearchFile creates a gzip file with testSearchContent.
func createGzipSearchFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "search.log.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := gzip.NewWriter(f)
	_, err = w.Write([]byte(testSearchContent))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return path
}

// createZstdSearchFile creates a non-seekable zstd file with testSearchContent.
func createZstdSearchFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "search.log.zst")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w, err := zstd.NewWriter(f)
	require.NoError(t, err)
	_, err = w.Write([]byte(testSearchContent))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return path
}

// createSeekableZstdSearchFile creates a seekable zstd file with testSearchContent
// using a small frame size for multiple frames.
func createSeekableZstdSearchFile(t *testing.T, dir string, content string, frameSize int) string {
	t.Helper()
	path := filepath.Join(dir, "seekable.log.zst")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	opts := compression.CompressOpts{
		FrameSize:        frameSize,
		CompressionLevel: 1,
	}
	_, err = compression.CreateSeekableZstd(strings.NewReader(content), f, opts)
	require.NoError(t, err)
	return path
}

// checkRgAvailable skips the test if rg is not found on PATH.
func checkRgAvailable(t *testing.T) {
	t.Helper()
	if err := ValidatePattern("test"); err != nil {
		t.Skip("rg (ripgrep) not available on PATH")
	}
}

// --- 3.4: Single-stream compressed search tests ---

func TestSearchCompressedFile_Gzip(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createGzipSearchFile(t, dir)

	matches, err := SearchCompressedFile(
		context.Background(), path,
		[]string{"ERROR"}, nil, 8,
	)
	require.NoError(t, err)

	assert.Equal(t, 2, len(matches), "should find two ERROR lines")
	for _, m := range matches {
		require.NotNil(t, m.LineText)
		assert.Contains(t, *m.LineText, "ERROR")
	}
}

func TestSearchCompressedFile_Zstd(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createZstdSearchFile(t, dir)

	matches, err := SearchCompressedFile(
		context.Background(), path,
		[]string{"ERROR"}, nil, 8,
	)
	require.NoError(t, err)

	assert.Equal(t, 2, len(matches))
	for _, m := range matches {
		require.NotNil(t, m.LineText)
		assert.Contains(t, *m.LineText, "ERROR")
	}
}

func TestSearchCompressedFile_NoMatches(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createGzipSearchFile(t, dir)

	matches, err := SearchCompressedFile(
		context.Background(), path,
		[]string{"NONEXISTENT_PATTERN"}, nil, 8,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, len(matches))
}

func TestSearchCompressedFile_MultiplePatterns(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createGzipSearchFile(t, dir)

	matches, err := SearchCompressedFile(
		context.Background(), path,
		[]string{"ERROR", "WARN"}, nil, 8,
	)
	require.NoError(t, err)

	// 2 ERROR + 1 WARN = 3 matching lines.
	assert.Equal(t, 3, len(matches))
}

func TestSearchCompressedFile_ContextCancellation(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createGzipSearchFile(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := SearchCompressedFile(ctx, path, []string{"ERROR"}, nil, 8)
	// Should return context error or partial results — not panic.
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	}
}

func TestSearchCompressedFile_MatchHasLineNumber(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createGzipSearchFile(t, dir)

	matches, err := SearchCompressedFile(
		context.Background(), path,
		[]string{"ERROR"}, nil, 8,
	)
	require.NoError(t, err)
	require.Greater(t, len(matches), 0)

	// Matches should have relative line numbers from rg.
	for _, m := range matches {
		assert.NotNil(t, m.RelativeLineNumber)
		if m.RelativeLineNumber != nil {
			assert.Greater(t, *m.RelativeLineNumber, 0)
		}
	}

	// Absolute line number should be -1 (no index available).
	for _, m := range matches {
		assert.Equal(t, -1, m.AbsoluteLineNumber)
	}
}

func TestSearchCompressedFile_MatchHasSubmatches(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createGzipSearchFile(t, dir)

	matches, err := SearchCompressedFile(
		context.Background(), path,
		[]string{"ERROR"}, nil, 8,
	)
	require.NoError(t, err)
	require.Greater(t, len(matches), 0)

	for _, m := range matches {
		require.NotNil(t, m.Submatches)
		assert.Greater(t, len(*m.Submatches), 0)
		assert.Equal(t, "ERROR", (*m.Submatches)[0].Text)
	}
}

// --- 3.5: Seekable zstd parallel search tests ---

func TestSearchSeekableZstd_Basic(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()

	// Use a small frame size to get multiple frames.
	path := createSeekableZstdSearchFile(t, dir, testSearchContent, 40)

	cfg := config.Load()
	cfg.MinChunkSizeMB = 0 // Disable fast path to force parallel search.
	cfg.FrameBatchSizeMB = 1 // Small batch to exercise batching logic.

	matches, err := SearchSeekableZstd(
		context.Background(), path,
		[]string{"ERROR"}, nil, &cfg,
	)
	require.NoError(t, err)

	assert.Equal(t, 2, len(matches), "should find two ERROR lines")
	for _, m := range matches {
		require.NotNil(t, m.LineText)
		assert.Contains(t, *m.LineText, "ERROR")
	}
}

func TestSearchSeekableZstd_OffsetAdjustment(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()

	// Generate enough content for multiple frames.
	var buf bytes.Buffer
	for i := 0; i < 20; i++ {
		buf.WriteString("normal line\n")
	}
	buf.WriteString("ERROR found here\n")
	for i := 0; i < 20; i++ {
		buf.WriteString("another normal line\n")
	}
	content := buf.String()

	path := createSeekableZstdSearchFile(t, dir, content, 50)

	cfg := config.Load()
	cfg.MinChunkSizeMB = 0
	cfg.FrameBatchSizeMB = 1

	matches, err := SearchSeekableZstd(
		context.Background(), path,
		[]string{"ERROR"}, nil, &cfg,
	)
	require.NoError(t, err)
	require.Equal(t, 1, len(matches))

	// The offset should be the position within the full decompressed content.
	expectedOffset := strings.Index(content, "ERROR found here")
	assert.Equal(t, expectedOffset, matches[0].Offset,
		"offset should reflect position in full decompressed stream")
}

func TestSearchSeekableZstd_FastPath(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()

	// Small content — should trigger fast path (single-stream).
	path := createSeekableZstdSearchFile(t, dir, testSearchContent, 1024)

	cfg := config.Load()
	cfg.MinChunkSizeMB = 100 // Set high threshold so fast path triggers.

	matches, err := SearchSeekableZstd(
		context.Background(), path,
		[]string{"ERROR"}, nil, &cfg,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, len(matches))
}

func TestSearchSeekableZstd_NoMatches(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createSeekableZstdSearchFile(t, dir, testSearchContent, 40)

	cfg := config.Load()
	cfg.MinChunkSizeMB = 0

	matches, err := SearchSeekableZstd(
		context.Background(), path,
		[]string{"NONEXISTENT"}, nil, &cfg,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, len(matches))
}

func TestSearchSeekableZstd_MultiplePatterns(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()
	path := createSeekableZstdSearchFile(t, dir, testSearchContent, 40)

	cfg := config.Load()
	cfg.MinChunkSizeMB = 0
	cfg.FrameBatchSizeMB = 1

	matches, err := SearchSeekableZstd(
		context.Background(), path,
		[]string{"ERROR", "WARN"}, nil, &cfg,
	)
	require.NoError(t, err)

	// 2 ERROR + 1 WARN = 3 matching lines.
	assert.Equal(t, 3, len(matches))
}

func TestSearchSeekableZstd_ResultsAreSorted(t *testing.T) {
	checkRgAvailable(t)
	dir := t.TempDir()

	// Generate content with multiple matches spread across frames.
	var buf bytes.Buffer
	for i := 0; i < 10; i++ {
		buf.WriteString("ERROR line\n")
		buf.WriteString("normal line\n")
	}
	content := buf.String()

	path := createSeekableZstdSearchFile(t, dir, content, 30)

	cfg := config.Load()
	cfg.MinChunkSizeMB = 0
	cfg.FrameBatchSizeMB = 1

	matches, err := SearchSeekableZstd(
		context.Background(), path,
		[]string{"ERROR"}, nil, &cfg,
	)
	require.NoError(t, err)
	assert.Equal(t, 10, len(matches))

	// Verify matches are sorted by offset.
	for i := 1; i < len(matches); i++ {
		assert.LessOrEqual(t, matches[i-1].Offset, matches[i].Offset,
			"matches should be sorted by offset")
	}
}

// --- planFrameBatches tests ---

func TestPlanFrameBatches_SingleBatch(t *testing.T) {
	table := &compression.SeekTable{
		Frames: []compression.FrameEntry{
			{DecompressedSize: 100},
			{DecompressedSize: 100},
		},
	}
	// Target larger than total — all frames in one batch.
	batches := planFrameBatches(table, 1000)
	assert.Equal(t, 1, len(batches))
	assert.Equal(t, 2, len(batches[0].frames))
}

func TestPlanFrameBatches_MultipleBatches(t *testing.T) {
	table := &compression.SeekTable{
		Frames: []compression.FrameEntry{
			{DecompressedSize: 100},
			{DecompressedSize: 100},
			{DecompressedSize: 100},
			{DecompressedSize: 100},
		},
	}
	// Target 150 — each batch gets ~2 frames (100+100 >= 150).
	batches := planFrameBatches(table, 150)
	assert.GreaterOrEqual(t, len(batches), 2, "should split into multiple batches")

	// All frames should be accounted for.
	total := 0
	for _, b := range batches {
		total += len(b.frames)
	}
	assert.Equal(t, 4, total)
}

func TestPlanFrameBatches_EmptyTable(t *testing.T) {
	table := &compression.SeekTable{}
	batches := planFrameBatches(table, 1000)
	assert.Equal(t, 0, len(batches))
}

func TestPlanFrameBatches_StartIdxCorrect(t *testing.T) {
	table := &compression.SeekTable{
		Frames: []compression.FrameEntry{
			{DecompressedSize: 50},
			{DecompressedSize: 50},
			{DecompressedSize: 50},
			{DecompressedSize: 50},
		},
	}
	batches := planFrameBatches(table, 60)

	// Verify startIdx tracks the cumulative frame index.
	frameIdx := 0
	for _, b := range batches {
		assert.Equal(t, frameIdx, b.startIdx)
		frameIdx += len(b.frames)
	}
}

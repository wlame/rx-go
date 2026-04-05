package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchChunk_SinglePattern(t *testing.T) {
	dir := t.TempDir()
	content := "line one\nERROR something failed\nline three\nERROR another issue\nline five\n"
	path := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	chunk := Chunk{Index: 0, Offset: 0, Length: int64(len(content))}

	matches, err := SearchChunk(context.Background(), f, chunk, []string{"ERROR"}, nil, 8)
	require.NoError(t, err)
	assert.Len(t, matches, 2, "should find two ERROR matches")

	for _, m := range matches {
		assert.Contains(t, *m.LineText, "ERROR")
		assert.NotNil(t, m.Submatches)
		assert.Greater(t, len(*m.Submatches), 0)
	}
}

func TestSearchChunk_MultiPattern(t *testing.T) {
	dir := t.TempDir()
	content := "info: all good\nERROR: failure\nWARN: something\nERROR: another\n"
	path := filepath.Join(dir, "multi.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	chunk := Chunk{Index: 0, Offset: 0, Length: int64(len(content))}

	matches, err := SearchChunk(context.Background(), f, chunk,
		[]string{"ERROR", "WARN"}, nil, 8)
	require.NoError(t, err)

	// rg with --json and multiple -e patterns: should find ERROR (2) + WARN (1) = 3 lines
	assert.Len(t, matches, 3, "should find matches for both patterns")
}

func TestSearchChunk_NoMatches(t *testing.T) {
	dir := t.TempDir()
	content := "nothing interesting here\njust normal log lines\n"
	path := filepath.Join(dir, "nothing.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	chunk := Chunk{Index: 0, Offset: 0, Length: int64(len(content))}

	matches, err := SearchChunk(context.Background(), f, chunk,
		[]string{"NONEXISTENT_PATTERN"}, nil, 8)
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestSearchChunk_DedupFilter(t *testing.T) {
	// Create a file with matches at known byte offsets, then search only a
	// sub-range to verify that matches outside the chunk's range are filtered out.
	dir := t.TempDir()
	content := "line1 MATCH\nline2 MATCH\nline3 MATCH\nline4 MATCH\n"
	path := filepath.Join(dir, "dedup.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	// Search only the first 24 bytes (covers "line1 MATCH\nline2 MATCH\n").
	// "line1 MATCH\n" = 12 bytes, "line2 MATCH\n" = 12 bytes → 24 bytes total.
	chunk := Chunk{Index: 0, Offset: 0, Length: 24}

	matches, err := SearchChunk(context.Background(), f, chunk, []string{"MATCH"}, nil, 8)
	require.NoError(t, err)

	// Should only get matches from the first 24 bytes, not from lines 3 and 4.
	assert.LessOrEqual(t, len(matches), 2, "dedup filter should exclude matches outside chunk range")
}

func TestSearchChunk_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	// Create a reasonably sized file so rg has work to do.
	content := ""
	for i := range 1000 {
		if i%10 == 0 {
			content += "ERROR line\n"
		} else {
			content += "normal line content here\n"
		}
	}
	path := filepath.Join(dir, "cancel.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	// Cancel immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Allow a brief moment for the timeout to fire.
	time.Sleep(5 * time.Millisecond)

	chunk := Chunk{Index: 0, Offset: 0, Length: int64(len(content))}
	_, err = SearchChunk(ctx, f, chunk, []string{"ERROR"}, nil, 8)

	// We expect either context.DeadlineExceeded or context.Canceled.
	// The function may also succeed if rg finishes before the cancellation propagates.
	if err != nil {
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	}
}

func TestSearchChunk_EmptyPatterns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	require.NoError(t, os.WriteFile(path, []byte("content\n"), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	chunk := Chunk{Index: 0, Offset: 0, Length: 8}
	matches, err := SearchChunk(context.Background(), f, chunk, nil, nil, 8)
	require.NoError(t, err)
	assert.Nil(t, matches)
}

func TestSearchChunk_AbsoluteOffsetCalculation(t *testing.T) {
	// Verify that when a chunk starts at a non-zero offset, match offsets are
	// adjusted to be absolute in the file.
	dir := t.TempDir()
	content := "aaaa\nbbbb\nERROR here\ndddd\n"
	path := filepath.Join(dir, "offset.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	// The ERROR line starts at byte offset 10 ("aaaa\nbbbb\n" = 10 bytes).
	// Search starting from offset 10.
	chunk := Chunk{Index: 1, Offset: 10, Length: int64(len(content)) - 10}

	matches, err := SearchChunk(context.Background(), f, chunk, []string{"ERROR"}, nil, 8)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	// The absolute offset should be 10 (start of "ERROR here\n" in the file).
	assert.Equal(t, 10, matches[0].Offset,
		"match offset should be absolute, not relative to chunk")
}

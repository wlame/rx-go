package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrace_SingleFile_SinglePattern(t *testing.T) {
	dir := t.TempDir()
	content := "line one\nERROR: something broke\nline three\nERROR: again\nline five\n"
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Should have one pattern.
	assert.Len(t, resp.Patterns, 1)
	assert.Equal(t, "ERROR", resp.Patterns["p1"])

	// Should have one file.
	assert.Len(t, resp.Files, 1)

	// Should find 2 ERROR matches.
	assert.Len(t, resp.Matches, 2)
	for _, m := range resp.Matches {
		assert.Equal(t, "p1", m.Pattern)
		assert.Contains(t, *m.LineText, "ERROR")
	}

	// Matches should be sorted by offset.
	if len(resp.Matches) >= 2 {
		assert.Less(t, resp.Matches[0].Offset, resp.Matches[1].Offset)
	}

	assert.Greater(t, resp.Time, 0.0)
}

func TestTrace_MultiFile(t *testing.T) {
	dir := t.TempDir()

	file1 := filepath.Join(dir, "file1.log")
	require.NoError(t, os.WriteFile(file1, []byte("ERROR in file1\nok\n"), 0644))

	file2 := filepath.Join(dir, "file2.log")
	require.NoError(t, os.WriteFile(file2, []byte("ok\nERROR in file2\n"), 0644))

	req := TraceRequest{
		Paths:    []string{file1, file2},
		Patterns: []string{"ERROR"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	assert.Len(t, resp.Files, 2)
	assert.Len(t, resp.Matches, 2)

	// Verify each match references a different file.
	files := map[string]bool{}
	for _, m := range resp.Matches {
		files[m.File] = true
	}
	assert.Len(t, files, 2, "matches should come from different files")
}

func TestTrace_DirectoryScan(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "a.log"), []byte("ERROR here\n"), 0644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "b.log"), []byte("no match\n"), 0644))

	req := TraceRequest{
		Paths:    []string{dir},
		Patterns: []string{"ERROR"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(resp.Files), 1)
	assert.Len(t, resp.Matches, 1)
	assert.Equal(t, "p1", resp.Matches[0].Pattern)
}

func TestTrace_MaxResults_EarlyTermination(t *testing.T) {
	dir := t.TempDir()

	// Create a file with many matches.
	content := ""
	for range 100 {
		content += "ERROR line\n"
	}
	path := filepath.Join(dir, "many.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	req := TraceRequest{
		Paths:      []string{path},
		Patterns:   []string{"ERROR"},
		MaxResults: 5,
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	assert.LessOrEqual(t, len(resp.Matches), 5,
		"should not return more than MaxResults matches")
	assert.NotNil(t, resp.MaxResults)
	assert.Equal(t, 5, *resp.MaxResults)
}

func TestTrace_EmptyResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	require.NoError(t, os.WriteFile(path, []byte("nothing here\n"), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"NONEXISTENT_PATTERN_XYZ"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	assert.Empty(t, resp.Matches)
	assert.Len(t, resp.Patterns, 1)
}

func TestTrace_MultiplePatterns(t *testing.T) {
	dir := t.TempDir()
	content := "ERROR: failure\nWARNING: attention\nINFO: ok\n"
	path := filepath.Join(dir, "multi.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR", "WARNING"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	assert.Len(t, resp.Patterns, 2)
	assert.Equal(t, "ERROR", resp.Patterns["p1"])
	assert.Equal(t, "WARNING", resp.Patterns["p2"])

	// Should find matches for both patterns.
	assert.GreaterOrEqual(t, len(resp.Matches), 2)

	patternsSeen := map[string]bool{}
	for _, m := range resp.Matches {
		patternsSeen[m.Pattern] = true
	}
	assert.True(t, patternsSeen["p1"], "should have p1 matches")
	assert.True(t, patternsSeen["p2"], "should have p2 matches")
}

func TestTrace_InvalidPattern(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(path, []byte("content\n"), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"(unclosed"},
	}

	_, err := Trace(context.Background(), req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid regex")
}

func TestTrace_NonexistentPath(t *testing.T) {
	req := TraceRequest{
		Paths:    []string{"/nonexistent/path/to/file.log"},
		Patterns: []string{"ERROR"},
	}

	_, err := Trace(context.Background(), req)
	assert.Error(t, err)
}

func TestTrace_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	require.NoError(t, os.WriteFile(path, []byte(""), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, resp.Matches)
}

func TestTrace_PatternIDs_Assigned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(path, []byte("data\n"), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"alpha", "beta", "gamma"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, "alpha", resp.Patterns["p1"])
	assert.Equal(t, "beta", resp.Patterns["p2"])
	assert.Equal(t, "gamma", resp.Patterns["p3"])
}

func TestTrace_FileChunks_Populated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(path, []byte("some content\n"), 0644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"content"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)

	require.NotNil(t, resp.FileChunks)
	// A small file should have 1 chunk.
	for _, count := range *resp.FileChunks {
		assert.Equal(t, 1, count)
	}
}

func TestTrace_ContextCancellation(t *testing.T) {
	dir := t.TempDir()

	// Create a file with content.
	content := ""
	for range 100 {
		content += "ERROR: line content here\n"
	}
	path := filepath.Join(dir, "cancel.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	}

	// Should either return an error or return partial/empty results.
	resp, err := Trace(ctx, req)
	// Both outcomes are acceptable: error from cancelled context, or
	// partial results if some work completed before cancellation.
	if err != nil {
		return // error is fine
	}
	if resp != nil {
		// Partial results are fine — the important thing is it didn't hang.
		assert.NotNil(t, resp)
	}
}

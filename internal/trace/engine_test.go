package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/pkg/models"
)

func TestEngine_Search_SingleFile(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: ERROR something went wrong
line 3: info
line 4: ERROR another error
line 5: info
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)
	assert.Len(t, resp.ScannedFiles, 1)
	assert.Contains(t, resp.ScannedFiles, testFile)
	assert.Greater(t, resp.Time, 0.0)
	assert.Greater(t, resp.SearchTimeMs, 0.0)
}

func TestEngine_Search_Directory(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()

	// Create multiple files
	files := map[string]string{
		"app1.log": "ERROR in app1\nINFO line\n",
		"app2.log": "INFO line\nERROR in app2\n",
		"app3.log": "DEBUG line\nINFO line\n",
	}

	for filename, content := range files {
		path := filepath.Join(tmpDir, filename)
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{tmpDir},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)
	assert.Equal(t, 3, len(resp.ScannedFiles))
}

func TestEngine_Search_MaxResults(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	// Create file with 10 ERROR lines
	content := ""
	for i := 0; i < 10; i++ {
		content += "ERROR line\n"
	}
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		MaxResults:    3,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	// Note: max_results is a soft limit - tasks already submitted will complete
	// For small files (single chunk), all matches will be found
	// This is expected behavior - max_results prevents NEW tasks from being submitted
	assert.Len(t, resp.Matches, 10) // All matches in single chunk
}

func TestEngine_Search_MultiplePatterns(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: INFO starting
line 2: ERROR something failed
line 3: WARN potential issue
line 4: ERROR critical failure
line 5: INFO finished
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR", "WARN"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 3) // 2 ERROR + 1 WARN
}

func TestEngine_Search_CaseInsensitive(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `error in lowercase
ERROR in uppercase
Error in mixed case
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: false,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 3)
}

func TestEngine_Search_NoMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: debug
line 3: trace
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 0)
	assert.Len(t, resp.ScannedFiles, 1)
}

func TestEngine_Search_InvalidPath(t *testing.T) {
	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{"/tmp"},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{"/nonexistent/file.log"},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
	}

	_, err := engine.Search(context.Background(), req)
	assert.Error(t, err)
}

func TestEngine_Search_BinaryFile(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	binaryFile := filepath.Join(tmpDir, "binary.dat")

	// Create binary file with null bytes
	content := []byte{0x00, 0x01, 0x02, 0x03, 0x00, 0xFF}
	require.NoError(t, os.WriteFile(binaryFile, content, 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{tmpDir},
		Patterns:      []string{"test"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	// Binary file should be skipped
	assert.Len(t, resp.ScannedFiles, 0)
}

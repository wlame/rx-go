package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/hooks"
)

// hookCall records query parameters from a single webhook delivery.
type hookCall struct {
	params map[string]string
}

// hookCollector is a thread-safe collector for webhook calls received by an httptest.Server.
type hookCollector struct {
	mu    sync.Mutex
	calls []hookCall
}

func (hc *hookCollector) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := make(map[string]string)
		for k, v := range r.URL.Query() {
			params[k] = v[0]
		}
		hc.mu.Lock()
		hc.calls = append(hc.calls, hookCall{params: params})
		hc.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (hc *hookCollector) getCalls() []hookCall {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	result := make([]hookCall, len(hc.calls))
	copy(result, hc.calls)
	return result
}

func TestTrace_Hooks_OnFileScanned_And_OnComplete(t *testing.T) {
	// Set up a test file with known content.
	dir := t.TempDir()
	content := "ERROR: first failure\nok line\nERROR: second failure\n"
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	// Start httptest servers to receive hook calls.
	fileCollector := &hookCollector{}
	fileSrv := httptest.NewServer(fileCollector.handler())
	defer fileSrv.Close()

	completeCollector := &hookCollector{}
	completeSrv := httptest.NewServer(completeCollector.handler())
	defer completeSrv.Close()

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
		Hooks: &hooks.HookCallbacks{
			OnFileScanned: fileSrv.URL,
			OnComplete:    completeSrv.URL,
		},
		RequestID: "test-hooks-123",
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Len(t, resp.Matches, 2, "should find 2 ERROR matches")

	// Hooks are fired asynchronously — wait briefly for delivery.
	time.Sleep(300 * time.Millisecond)

	// Verify on_file_scanned was called once (one file).
	fileCalls := fileCollector.getCalls()
	require.Len(t, fileCalls, 1, "on_file_scanned should fire once for one file")
	assert.Equal(t, "test-hooks-123", fileCalls[0].params["request_id"])
	assert.Equal(t, path, fileCalls[0].params["path"])
	assert.Equal(t, "2", fileCalls[0].params["matches"])

	// Verify on_complete was called once.
	completeCalls := completeCollector.getCalls()
	require.Len(t, completeCalls, 1, "on_complete should fire once")
	assert.Equal(t, "test-hooks-123", completeCalls[0].params["request_id"])
	assert.Equal(t, "2", completeCalls[0].params["total_matches"])
	assert.Equal(t, "1", completeCalls[0].params["total_files"])
	// Duration should be present and non-empty.
	assert.NotEmpty(t, completeCalls[0].params["duration"])
}

func TestTrace_Hooks_OnMatchFound_Fires_When_MaxResults_Set(t *testing.T) {
	dir := t.TempDir()
	content := "ERROR: first\nERROR: second\nERROR: third\n"
	path := filepath.Join(dir, "matches.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	matchCollector := &hookCollector{}
	matchSrv := httptest.NewServer(matchCollector.handler())
	defer matchSrv.Close()

	req := TraceRequest{
		Paths:      []string{path},
		Patterns:   []string{"ERROR"},
		MaxResults: 10, // Must be set for on_match_found to fire.
		Hooks: &hooks.HookCallbacks{
			OnMatchFound: matchSrv.URL,
		},
		RequestID: "test-match-hooks",
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Len(t, resp.Matches, 3, "should find 3 ERROR matches")

	// Wait for async hook delivery.
	time.Sleep(300 * time.Millisecond)

	// Verify on_match_found was called 3 times (one per match).
	matchCalls := matchCollector.getCalls()
	require.Len(t, matchCalls, 3, "on_match_found should fire once per match")

	// Each call should have the expected params.
	for _, call := range matchCalls {
		assert.Equal(t, "test-match-hooks", call.params["request_id"])
		assert.Equal(t, path, call.params["path"])
		assert.NotEmpty(t, call.params["offset"])
		assert.NotEmpty(t, call.params["line_number"])
		assert.Equal(t, "ERROR", call.params["pattern"])
	}
}

func TestTrace_Hooks_OnMatchFound_Silent_Without_MaxResults(t *testing.T) {
	dir := t.TempDir()
	content := "ERROR: found\n"
	path := filepath.Join(dir, "silent.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	matchCollector := &hookCollector{}
	matchSrv := httptest.NewServer(matchCollector.handler())
	defer matchSrv.Close()

	// MaxResults is 0 (unlimited) — on_match_found should NOT fire.
	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
		Hooks: &hooks.HookCallbacks{
			OnMatchFound: matchSrv.URL,
		},
		RequestID: "test-no-match-hooks",
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)

	// Wait and verify no match hooks were called.
	time.Sleep(300 * time.Millisecond)
	matchCalls := matchCollector.getCalls()
	assert.Empty(t, matchCalls, "on_match_found should not fire when MaxResults is 0")
}

func TestTrace_Hooks_NilHooks_NoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "safe.log")
	require.NoError(t, os.WriteFile(path, []byte("ERROR here\n"), 0644))

	// Hooks is nil — should not panic or error.
	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
		Hooks:    nil,
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)
}

func TestTrace_Hooks_MultiFile_OnFileScanned(t *testing.T) {
	dir := t.TempDir()

	file1 := filepath.Join(dir, "a.log")
	require.NoError(t, os.WriteFile(file1, []byte("ERROR in a\n"), 0644))

	file2 := filepath.Join(dir, "b.log")
	require.NoError(t, os.WriteFile(file2, []byte("ERROR in b\nERROR again\n"), 0644))

	fileCollector := &hookCollector{}
	fileSrv := httptest.NewServer(fileCollector.handler())
	defer fileSrv.Close()

	completeCollector := &hookCollector{}
	completeSrv := httptest.NewServer(completeCollector.handler())
	defer completeSrv.Close()

	req := TraceRequest{
		Paths:    []string{file1, file2},
		Patterns: []string{"ERROR"},
		Hooks: &hooks.HookCallbacks{
			OnFileScanned: fileSrv.URL,
			OnComplete:    completeSrv.URL,
		},
		RequestID: "test-multi-file",
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	assert.Len(t, resp.Matches, 3)

	time.Sleep(300 * time.Millisecond)

	// Should have 2 on_file_scanned calls (one per file).
	fileCalls := fileCollector.getCalls()
	require.Len(t, fileCalls, 2, "on_file_scanned should fire once per file")

	// Collect reported paths.
	paths := make(map[string]bool)
	for _, call := range fileCalls {
		paths[call.params["path"]] = true
		assert.Equal(t, "test-multi-file", call.params["request_id"])
	}
	assert.True(t, paths[file1], "should report file1")
	assert.True(t, paths[file2], "should report file2")

	// on_complete should report totals.
	completeCalls := completeCollector.getCalls()
	require.Len(t, completeCalls, 1)
	assert.Equal(t, "3", completeCalls[0].params["total_matches"])
	assert.Equal(t, "2", completeCalls[0].params["total_files"])
}

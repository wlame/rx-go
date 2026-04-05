package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/config"
)

// newTestServer creates a Server backed by a temporary search root directory.
// The caller gets the server, the temp dir path, and a cleanup function.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	tmpDir := t.TempDir()

	cfg := config.Config{
		SearchRoots:    []string{tmpDir},
		CacheDir:       filepath.Join(tmpDir, ".cache"),
		MaxLineSizeKB:  8,
		MaxSubprocesses: 20,
		MinChunkSizeMB: 20,
		MaxFiles:       1000,
		LargeFileMB:    50,
		LogLevel:       "WARNING",
	}

	srv := NewServer(&cfg)
	return srv, tmpDir
}

// doRequest is a test helper that sends an HTTP request to the server and returns the response.
func doRequest(t *testing.T, srv *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.Router.ServeHTTP(rr, req)
	return rr
}

// --- 6.1: Server + router + middleware tests ---

func TestMiddleware_RequestID(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/health", nil)

	// Every response must have an X-Request-ID header with a valid UUID format.
	reqID := rr.Header().Get("X-Request-ID")
	assert.NotEmpty(t, reqID, "X-Request-ID header should be present")
	assert.Len(t, reqID, 36, "UUID v4 should be 36 characters (with hyphens)")
}

func TestMiddleware_CORS(t *testing.T) {
	srv, _ := newTestServer(t)

	// Send an OPTIONS preflight request.
	req := httptest.NewRequest("OPTIONS", "/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rr := httptest.NewRecorder()
	srv.Router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "*", rr.Header().Get("Access-Control-Allow-Origin"))
}

func TestMiddleware_PanicRecovery(t *testing.T) {
	// Test recovery middleware directly (no chi router needed).
	handler := recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("intentional test panic")
	}))

	req := httptest.NewRequest("GET", "/panic", nil)
	ctx := context.WithValue(req.Context(), requestIDKey, "panic-test")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	assert.NotPanics(t, func() {
		handler.ServeHTTP(rr, req)
	})
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Contains(t, rr.Body.String(), "internal server error")
}

// --- 6.9: Health endpoint tests ---

func TestHealth_ResponseFields(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/health", nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Check required fields are present.
	assert.Equal(t, "ok", resp["status"])
	assert.Contains(t, resp, "app_version")
	assert.Contains(t, resp, "go_version")
	assert.Contains(t, resp, "uptime_seconds")
	assert.Contains(t, resp, "rg_version")
	assert.Contains(t, resp, "ripgrep_available")
	assert.Contains(t, resp, "os_info")
	assert.Contains(t, resp, "constants")

	// Uptime should be a positive number.
	uptime, ok := resp["uptime_seconds"].(float64)
	assert.True(t, ok)
	assert.GreaterOrEqual(t, uptime, 0.0)
}

func TestHealth_SearchRoots(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/health", nil)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	roots, ok := resp["search_roots"].([]interface{})
	require.True(t, ok, "search_roots should be an array")
	assert.Len(t, roots, 1)
	assert.Equal(t, tmpDir, roots[0])
}

// --- 6.2: Trace endpoint tests ---

func TestTrace_MissingPath(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/v1/trace?regexp=error", nil)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "path")
}

func TestTrace_MissingRegexp(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	// Create a test file.
	testFile := filepath.Join(tmpDir, "test.log")
	os.WriteFile(testFile, []byte("hello world\n"), 0o644)

	rr := doRequest(t, srv, "GET", fmt.Sprintf("/v1/trace?path=%s", testFile), nil)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "regexp")
}

func TestTrace_BasicSearch(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	// Create a test file with known content.
	testFile := filepath.Join(tmpDir, "test.log")
	content := "line one\nERROR something failed\nline three\n"
	os.WriteFile(testFile, []byte(content), 0o644)

	url := fmt.Sprintf("/v1/trace?path=%s&regexp=ERROR", testFile)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Should have standard trace response fields.
	assert.Contains(t, resp, "request_id")
	assert.Contains(t, resp, "patterns")
	assert.Contains(t, resp, "files")
	assert.Contains(t, resp, "matches")
	assert.Contains(t, resp, "scanned_files")
	assert.Contains(t, resp, "skipped_files")
	assert.Contains(t, resp, "time")
}

func TestTrace_MultiPattern(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "test.log")
	content := "ERROR first\nWARNING second\nINFO third\n"
	os.WriteFile(testFile, []byte(content), 0o644)

	url := fmt.Sprintf("/v1/trace?path=%s&regexp=ERROR&regexp=WARNING", testFile)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Should have two patterns registered.
	patterns, ok := resp["patterns"].(map[string]interface{})
	require.True(t, ok)
	assert.Len(t, patterns, 2)
}

// --- 6.3: Samples endpoint tests ---

func TestSamples_MissingPath(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/v1/samples?byte_offset=0", nil)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "path")
}

func TestSamples_MissingOffsetAndLine(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "test.log")
	os.WriteFile(testFile, []byte("hello\n"), 0o644)

	rr := doRequest(t, srv, "GET", fmt.Sprintf("/v1/samples?path=%s", testFile), nil)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "offsets")
}

func TestSamples_ByteOffset(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "test.log")
	content := "line one\nline two\nline three\nline four\nline five\n"
	os.WriteFile(testFile, []byte(content), 0o644)

	// Byte offset 9 is the start of "line two"
	url := fmt.Sprintf("/v1/samples?path=%s&byte_offset=9", testFile)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, testFile, resp["path"])
	assert.Contains(t, resp, "samples")
	assert.Contains(t, resp, "before_context")
	assert.Contains(t, resp, "after_context")

	// Verify samples map has the offset key.
	samples, ok := resp["samples"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, samples, "9")
}

func TestSamples_LineNumber(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "test.log")
	content := "first\nsecond\nthird\nfourth\nfifth\n"
	os.WriteFile(testFile, []byte(content), 0o644)

	url := fmt.Sprintf("/v1/samples?path=%s&line=2", testFile)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	samples, ok := resp["samples"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, samples, "2")
}

func TestSamples_FileNotFound(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	url := fmt.Sprintf("/v1/samples?path=%s/nonexistent.log&byte_offset=0", tmpDir)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// --- 6.4: Index endpoint tests ---

func TestGetIndex_NoIndex(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "test.log")
	os.WriteFile(testFile, []byte("hello\n"), 0o644)

	url := fmt.Sprintf("/v1/index?path=%s", testFile)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "no index found")
}

func TestPostIndex_TriggerBackground(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "test.log")
	// Create a file with some content.
	content := strings.Repeat("this is a test line\n", 100)
	os.WriteFile(testFile, []byte(content), 0o644)

	body := fmt.Sprintf(`{"path": "%s"}`, testFile)
	rr := doRequest(t, srv, "POST", "/v1/index", strings.NewReader(body))

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Contains(t, resp, "task_id")
	assert.Contains(t, resp, "status")
	assert.Contains(t, resp, "message")
	assert.NotEmpty(t, resp["task_id"])
}

func TestPostIndex_MissingPath(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"force": true}`
	rr := doRequest(t, srv, "POST", "/v1/index", strings.NewReader(body))

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestPostIndex_FileNotFound(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	body := fmt.Sprintf(`{"path": "%s/nonexistent.log"}`, tmpDir)
	rr := doRequest(t, srv, "POST", "/v1/index", strings.NewReader(body))

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// --- 6.5: Complexity endpoint tests ---

func TestComplexity_StubResponse(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/v1/complexity?regex=test.*pattern", nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Verify the stub has all expected fields with correct types.
	assert.Equal(t, "test.*pattern", resp["regex"])
	assert.Equal(t, float64(0), resp["score"])
	assert.Equal(t, "not_implemented", resp["risk_level"])
	assert.Equal(t, "not_implemented", resp["complexity_class"])
	assert.Equal(t, "unknown", resp["level"])
	assert.Equal(t, "not_implemented", resp["risk"])

	// Arrays should be empty, not null.
	issues, ok := resp["issues"].([]interface{})
	assert.True(t, ok, "issues should be an array")
	assert.Empty(t, issues)

	warnings, ok := resp["warnings"].([]interface{})
	assert.True(t, ok, "warnings should be an array")
	assert.Empty(t, warnings)

	recommendations, ok := resp["recommendations"].([]interface{})
	assert.True(t, ok, "recommendations should be an array")
	assert.Empty(t, recommendations)
}

func TestComplexity_MissingRegex(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/v1/complexity", nil)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "regex")
}

// --- 6.6: Detectors endpoint tests ---

func TestDetectors_EmptyList(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/v1/detectors", nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Detectors should be an empty list.
	detectors, ok := resp["detectors"].([]interface{})
	assert.True(t, ok, "detectors should be an array")
	assert.Empty(t, detectors)

	// Categories should be an empty list.
	categories, ok := resp["categories"].([]interface{})
	assert.True(t, ok, "categories should be an array")
	assert.Empty(t, categories)

	// Severity scale should be populated with 4 levels.
	scale, ok := resp["severity_scale"].([]interface{})
	assert.True(t, ok, "severity_scale should be an array")
	assert.Len(t, scale, 4)
}

// --- 6.7: Tasks endpoint tests ---

func TestGetTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rr := doRequest(t, srv, "GET", "/v1/tasks/nonexistent-id", nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "task not found")
}

func TestGetTask_ExistingTask(t *testing.T) {
	srv, _ := newTestServer(t)

	// Create a task directly in the store.
	taskID := srv.TaskStore.Create("index", "/tmp/test.log")
	srv.TaskStore.SetRunning(taskID)

	rr := doRequest(t, srv, "GET", fmt.Sprintf("/v1/tasks/%s", taskID), nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, taskID, resp["task_id"])
	assert.Equal(t, "running", resp["status"])
	assert.Equal(t, "index", resp["operation"])
	assert.Equal(t, "/tmp/test.log", resp["path"])
	assert.NotNil(t, resp["started_at"])
}

func TestGetTask_CompletedTask(t *testing.T) {
	srv, _ := newTestServer(t)

	taskID := srv.TaskStore.Create("compress", "/tmp/data.zst")
	srv.TaskStore.Complete(taskID, map[string]interface{}{
		"success":     true,
		"output_path": "/tmp/data.zst",
	})

	rr := doRequest(t, srv, "GET", fmt.Sprintf("/v1/tasks/%s", taskID), nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "completed", resp["status"])
	assert.NotNil(t, resp["completed_at"])
	assert.NotNil(t, resp["result"])

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, result["success"])
}

func TestGetTask_FailedTask(t *testing.T) {
	srv, _ := newTestServer(t)

	taskID := srv.TaskStore.Create("index", "/tmp/broken.log")
	srv.TaskStore.Fail(taskID, "file corrupted")

	rr := doRequest(t, srv, "GET", fmt.Sprintf("/v1/tasks/%s", taskID), nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "failed", resp["status"])
	assert.Equal(t, "file corrupted", resp["error"])
}

// --- 6.8: Tree endpoint tests ---

func TestTree_ListDirectory(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	// Create some test files and directories.
	os.MkdirAll(filepath.Join(tmpDir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "file2.log"), []byte("world\n"), 0o644)

	url := fmt.Sprintf("/v1/tree?path=%s", tmpDir)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, tmpDir, resp["path"])

	entries, ok := resp["entries"].([]interface{})
	require.True(t, ok)
	assert.Len(t, entries, 3) // subdir + file1.txt + file2.log

	// First entry should be the directory (sorted: dirs first).
	first := entries[0].(map[string]interface{})
	assert.Equal(t, "subdir", first["name"])
	assert.Equal(t, true, first["is_dir"])
}

func TestTree_DefaultsToSearchRoot(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("data\n"), 0o644)

	rr := doRequest(t, srv, "GET", "/v1/tree", nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, tmpDir, resp["path"])
}

func TestTree_PathTraversalBlocked(t *testing.T) {
	srv, _ := newTestServer(t)

	// Attempt to traverse outside the search root.
	rr := doRequest(t, srv, "GET", "/v1/tree?path=/../../../etc", nil)

	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestTree_NonexistentPath(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	url := fmt.Sprintf("/v1/tree?path=%s/does-not-exist", tmpDir)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestTree_FileNotDirectory(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "notadir.txt")
	os.WriteFile(testFile, []byte("hello\n"), 0o644)

	url := fmt.Sprintf("/v1/tree?path=%s", testFile)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "not a directory")
}

func TestTree_EntriesHaveSizeAndModified(t *testing.T) {
	srv, tmpDir := newTestServer(t)

	testFile := filepath.Join(tmpDir, "sized.txt")
	os.WriteFile(testFile, []byte("twelve chars"), 0o644)

	url := fmt.Sprintf("/v1/tree?path=%s", tmpDir)
	rr := doRequest(t, srv, "GET", url, nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	entries := resp["entries"].([]interface{})
	require.Len(t, entries, 1)

	entry := entries[0].(map[string]interface{})
	assert.Equal(t, "sized.txt", entry["name"])
	assert.Equal(t, false, entry["is_dir"])
	assert.Equal(t, float64(12), entry["size"])
	assert.NotNil(t, entry["modified"])
}

// --- TaskStore tests ---

func TestTaskStore_CreateAndGet(t *testing.T) {
	ts := NewTaskStore()

	id := ts.Create("index", "/tmp/test.log")
	assert.NotEmpty(t, id)

	task := ts.Get(id)
	require.NotNil(t, task)
	assert.Equal(t, id, task.ID)
	assert.Equal(t, "index", task.Operation)
	assert.Equal(t, "/tmp/test.log", task.Path)
	assert.Equal(t, TaskStatePending, task.State)
}

func TestTaskStore_GetNotFound(t *testing.T) {
	ts := NewTaskStore()
	task := ts.Get("nonexistent")
	assert.Nil(t, task)
}

func TestTaskStore_Lifecycle(t *testing.T) {
	ts := NewTaskStore()

	id := ts.Create("compress", "/tmp/data")

	// Pending -> Running
	ts.SetRunning(id)
	task := ts.Get(id)
	assert.Equal(t, TaskStateRunning, task.State)

	// Running -> Completed
	ts.Complete(id, map[string]interface{}{"output": "/tmp/data.zst"})
	task = ts.Get(id)
	assert.Equal(t, TaskStateCompleted, task.State)
	assert.NotNil(t, task.CompletedAt)
	assert.Equal(t, "/tmp/data.zst", task.Result["output"])
}

func TestTaskStore_FailedTask(t *testing.T) {
	ts := NewTaskStore()

	id := ts.Create("index", "/tmp/broken")
	ts.SetRunning(id)
	ts.Fail(id, "disk full")

	task := ts.Get(id)
	assert.Equal(t, TaskStateFailed, task.State)
	assert.NotNil(t, task.Error)
	assert.Equal(t, "disk full", *task.Error)
	assert.NotNil(t, task.CompletedAt)
}

// --- ListenAndServe graceful shutdown test ---

func TestListenAndServe_GracefulShutdown(t *testing.T) {
	cfg := config.Config{
		SearchRoots: []string{t.TempDir()},
		CacheDir:    filepath.Join(t.TempDir(), ".cache"),
		LogLevel:    "WARNING",
	}
	srv := NewServer(&cfg)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		// Use port 0 for random port assignment.
		errCh <- srv.ListenAndServe(ctx, "127.0.0.1:0")
	}()

	// Give server time to start.
	time.Sleep(50 * time.Millisecond)

	// Cancel context to trigger graceful shutdown.
	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within timeout")
	}
}

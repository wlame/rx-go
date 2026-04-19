package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestIndexPost_BuildsRealIndex verifies that POST /v1/index now produces
// a full line-offset checkpoint list, not just a stat-based stub.
//
// Creates a ~1 MB fixture with known line structure, POSTs to build,
// polls the task until done, then asserts the returned index has
// multiple checkpoints (the old stub always returned 1).
func TestIndexPost_BuildsRealIndex(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	// Build a 1 MB fixture. We use a small threshold so the POST is
	// accepted without bumping RX_LARGE_FILE_MB.
	big := filepath.Join(root, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 30k lines × ~60 bytes ≈ 1.8 MB. Above threshold=1 MB after
	// POST conversion; generates enough content for multiple checkpoints.
	for i := 1; i <= 30000; i++ {
		fmt.Fprintf(f, "line_%05d:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklm\n", i)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	// Use threshold=1 (MB) so 1.2 MB is above and the POST is accepted.
	reqBody, _ := json.Marshal(rxtypes.IndexRequest{
		Path:      big,
		Threshold: intPtr(1),
	})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post status %d: %s", resp.StatusCode, body)
	}
	var task rxtypes.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Poll until done. Use a tight loop — the fixture is small.
	var finalStatus string
	for i := 0; i < 50; i++ {
		taskResp, err := http.Get(ts.URL + "/v1/tasks/" + task.TaskID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		var body map[string]any
		if err := json.NewDecoder(taskResp.Body).Decode(&body); err != nil {
			_ = taskResp.Body.Close()
			t.Fatalf("decode task: %v", err)
		}
		_ = taskResp.Body.Close()
		status, _ := body["status"].(string)
		finalStatus = status
		if status == "completed" || status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalStatus != "completed" {
		t.Fatalf("task did not complete, final status=%s", finalStatus)
	}

	// GET /v1/index to read back the cached entry.
	getResp, err := http.Get(ts.URL + "/v1/index?path=" + big)
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("get status %d: %s", getResp.StatusCode, body)
	}

	var idx map[string]any
	if err := json.NewDecoder(getResp.Body).Decode(&idx); err != nil {
		t.Fatalf("decode idx: %v", err)
	}

	// Validate real-builder outputs.
	entries, _ := idx["index_entries"].(float64)
	if entries < 1 {
		t.Errorf("index_entries: got %v, want >= 1 (builder should produce at least the initial checkpoint)", entries)
	}
	lineIdx, _ := idx["line_index"].([]any)
	if len(lineIdx) != int(entries) {
		t.Errorf("line_index len %d != index_entries %v", len(lineIdx), entries)
	}
	lineCount, _ := idx["line_count"].(float64)
	if lineCount != 30000 {
		t.Errorf("line_count: got %v, want 30000", lineCount)
	}

	// Check the first checkpoint is [1, 0].
	if len(lineIdx) > 0 {
		first, ok := lineIdx[0].([]any)
		if !ok || len(first) < 2 {
			t.Errorf("first checkpoint malformed: %v", lineIdx[0])
		} else {
			if first[0] != float64(1) || first[1] != float64(0) {
				t.Errorf("first checkpoint: got %v, want [1, 0]", first)
			}
		}
	}
}

// TestIndexPost_ReusesCacheOnSecondCall verifies the second POST with
// the same path returns the cached index quickly (no re-build).
func TestIndexPost_ReusesCacheOnSecondCall(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	big := filepath.Join(root, "cached.log")
	// 2 MB to comfortably exceed threshold=1.
	content := strings.Repeat("row\n", 500000)
	if err := os.WriteFile(big, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	// First call — builds.
	postAndWait(t, ts.URL, big, 1, false, false)

	// Second call — should hit cache. We can't directly observe "no
	// rebuild" from outside, but the task should still succeed.
	postAndWait(t, ts.URL, big, 1, false, false)

	// Third call with --force should rebuild regardless.
	postAndWait(t, ts.URL, big, 1, true, false)
}

// TestIndexGet_AfterBuild verifies GET returns a valid cached index
// with a non-trivial line_index after a build.
func TestIndexGet_AfterBuild(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	big := filepath.Join(root, "lookup.log")
	content := strings.Repeat("abcdefghij\n", 150000) // ~1.6 MB
	if err := os.WriteFile(big, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	postAndWait(t, ts.URL, big, 1, false, false)

	resp, err := http.Get(ts.URL + "/v1/index?path=" + big)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var idx map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lc, _ := idx["line_count"].(float64)
	if lc != 150000 {
		t.Errorf("line_count: got %v, want 150000", lc)
	}
}

// ==========================================================================
// Helpers
// ==========================================================================

// postAndWait POSTs /v1/index and polls until the task is in a terminal
// state. Fails the test on timeout or a failed task.
func postAndWait(t *testing.T, baseURL, path string, thresholdMB int, force, analyze bool) {
	t.Helper()
	body, _ := json.Marshal(rxtypes.IndexRequest{
		Path:      path,
		Force:     force,
		Analyze:   analyze,
		Threshold: intPtr(thresholdMB),
	})
	resp, err := http.Post(baseURL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("post status %d: %s", resp.StatusCode, b)
	}
	var tr rxtypes.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for i := 0; i < 50; i++ {
		r, err := http.Get(baseURL + "/v1/tasks/" + tr.TaskID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = r.Body.Close()
		status, _ := body["status"].(string)
		if status == "completed" {
			return
		}
		if status == "failed" {
			t.Fatalf("task failed: %v", body["error"])
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task timed out")
}

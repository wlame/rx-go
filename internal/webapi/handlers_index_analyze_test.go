package webapi

// Tests for Task 6: the `analyze_window_lines` request body field on
// POST /v1/index. The field is plumbed through
// analyzer.ResolveWindowLines → BuildOptions.WindowLines; these tests
// verify the wire contract (the field parses, round-trips, and the
// resolver precedence URL > CLI > env holds at the HTTP layer).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestIndexPost_AnalyzeWindowLinesField_RoundTrip verifies the HTTP
// surface accepts the JSON field and completes the task without error.
//
// We can't directly observe the coordinator from HTTP, so this is a
// wiring test: the field must not be rejected by the schema validator,
// and the build must succeed with Analyze=true + a custom window size.
func TestIndexPost_AnalyzeWindowLinesField_RoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	big := filepath.Join(root, "window.log")
	// ~1.1 MB — above threshold=1.
	if err := os.WriteFile(big, []byte(strings.Repeat("hello world\n", 100000)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	body, _ := json.Marshal(rxtypes.IndexRequest{
		Path:               big,
		Threshold:          intPtr(1),
		Analyze:            true,
		AnalyzeWindowLines: 256,
	})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("post status %d: %s", resp.StatusCode, b)
	}

	// Task should complete successfully — use postAndWait's pattern for
	// polling. Since we already POSTed manually above, we just drain the
	// task now.
	var tr rxtypes.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	waitForTaskCompletion(t, ts.URL, tr.TaskID)
}

// TestResolveWindowLines_Precedence_URLOverridesCLIAndEnv directly
// exercises the resolver from the analyzer package so the URL > CLI >
// env > default contract has explicit test coverage at the CLI/HTTP
// boundary (not just inside analyzer package tests). The HTTP handler
// passes URL=req.AnalyzeWindowLines, CLI=0; the CLI passes CLI=flag,
// URL=0 — this test walks the three live combinations.
func TestResolveWindowLines_Precedence_URLOverridesCLIAndEnv(t *testing.T) {
	t.Setenv("RX_ANALYZE_WINDOW_LINES", "64")

	// URL (HTTP path): URL=256, CLI=0 → 256 wins over env 64.
	if got := analyzer.ResolveWindowLines(0, 256); got != 256 {
		t.Errorf("URL=256 env=64: got %d, want 256", got)
	}

	// CLI (command-line path): CLI=128, URL=0 → 128 wins over env 64.
	if got := analyzer.ResolveWindowLines(128, 0); got != 128 {
		t.Errorf("CLI=128 env=64: got %d, want 128", got)
	}

	// Both set (hypothetical — no real caller sets both, but the
	// precedence says URL wins).
	if got := analyzer.ResolveWindowLines(128, 256); got != 256 {
		t.Errorf("URL=256 CLI=128: got %d, want 256 (URL wins)", got)
	}

	// Neither set → falls through to env.
	if got := analyzer.ResolveWindowLines(0, 0); got != 64 {
		t.Errorf("no flags, env=64: got %d, want 64", got)
	}

	// Neither set and no env → default.
	t.Setenv("RX_ANALYZE_WINDOW_LINES", "")
	if got := analyzer.ResolveWindowLines(0, 0); got != 128 { // defaultWindowLines
		t.Errorf("no flags, no env: got %d, want 128 (default)", got)
	}
}

// waitForTaskCompletion is a local poll helper. We can't reuse
// postAndWait because that re-POSTs; we just want to drain an existing
// task ID to "completed" or fail the test on timeout. Sleeps between
// polls to match the pattern used elsewhere in this package.
func waitForTaskCompletion(t *testing.T, baseURL, taskID string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		r, err := http.Get(baseURL + "/v1/tasks/" + taskID)
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
	t.Fatal("task did not complete in time")
}

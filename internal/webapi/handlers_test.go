package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// newTestServer creates a Server suitable for unit tests. Uses an
// in-memory task manager, no hook dispatcher, and an empty frontend
// cache so SPA fallback → redirect to /docs.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := NewServer(Config{
		AppVersion:  "unit-test",
		RipgrepPath: "/usr/bin/rg", // lie: tests that actually need rg provide their own
		TaskManager: tasks.New(tasks.Config{}),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// TestHealth_AllFieldsPresent asserts the /health response contains
// every key rx-viewer's dashboard reads.
func TestHealth_AllFieldsPresent(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	requiredKeys := []string{
		"status", "ripgrep_available", "app_version", "go_version",
		"os_info", "system_resources", "go_packages", "constants",
		"environment", "hooks", "docs_url",
	}
	for _, k := range requiredKeys {
		if _, ok := body[k]; !ok {
			t.Errorf("health missing key %q", k)
		}
	}
	// Specific value checks.
	if body["status"] != "ok" {
		t.Errorf("status: got %v, want ok", body["status"])
	}
	if body["ripgrep_available"] != true {
		t.Errorf("ripgrep_available: got %v", body["ripgrep_available"])
	}
	if body["docs_url"] != "https://github.com/wlame/rx-tool" {
		t.Errorf("docs_url: got %v", body["docs_url"])
	}
}

// TestDetectors_EmptyRegistry — Go port ships with no analyzers at v1
// but the response envelope stays shaped for rx-viewer's consumption.
func TestDetectors_EmptyRegistry(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/detectors")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body rxtypes.DetectorsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Detectors) != 0 {
		t.Errorf("expected empty detectors, got %d", len(body.Detectors))
	}
	if len(body.Categories) != 0 {
		t.Errorf("expected empty categories, got %d", len(body.Categories))
	}
	if len(body.SeverityScale) != 4 {
		t.Errorf("severity_scale: got %d entries, want 4", len(body.SeverityScale))
	}
	labels := []string{"low", "medium", "high", "critical"}
	for i, s := range body.SeverityScale {
		if s.Label != labels[i] {
			t.Errorf("severity[%d] label: got %s, want %s", i, s.Label, labels[i])
		}
	}
}

// TestTree_ListSearchRoots exercises the no-path branch.
func TestTree_ListSearchRoots(t *testing.T) {
	// Configure a search root for this test.
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.log"), []byte("x"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/tree")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body rxtypes.TreeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !body.IsSearchRoot {
		t.Errorf("expected is_search_root=true")
	}
	if len(body.Entries) != 1 {
		t.Errorf("expected 1 root entry, got %d", len(body.Entries))
	}
}

// TestTree_ListDirectory exercises listing a real directory under a
// search root.
func TestTree_ListDirectory(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "subdir")
	_ = os.Mkdir(sub, 0o755)
	_ = os.WriteFile(filepath.Join(root, "a.log"), []byte("a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "b.log"), []byte("bb\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/tree?path=" + root)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body rxtypes.TreeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.TotalEntries != 3 {
		t.Errorf("total_entries: got %d, want 3", body.TotalEntries)
	}
	// First entry should be the directory (dirs sort first).
	if body.Entries[0].Type != "directory" {
		t.Errorf("first entry: got type %q, want directory", body.Entries[0].Type)
	}
	// Rest are files in alpha order.
	if body.Entries[1].Name != "a.log" {
		t.Errorf("second entry: got %q, want a.log", body.Entries[1].Name)
	}
	if body.Entries[2].Name != "b.log" {
		t.Errorf("third entry: got %q, want b.log", body.Entries[2].Name)
	}
}

// TestTree_PathOutsideRoots verifies sandbox rejection emits the
// Go-idiomatic structured error (decision 6.9.3).
func TestTree_PathOutsideRoots(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	// Request a path outside the configured root.
	resp, err := http.Get(ts.URL + "/v1/tree?path=/etc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Must have both the "detail" key (FastAPI shape) and the
	// Go-idiomatic "error" + "message" + "path" + "roots" keys.
	if body["detail"] != "path_outside_search_root" {
		t.Errorf("detail: got %v, want path_outside_search_root", body["detail"])
	}
	if body["error"] != "path_outside_search_root" {
		t.Errorf("error: got %v", body["error"])
	}
	if _, ok := body["path"]; !ok {
		t.Errorf("missing path in sandbox error")
	}
	if _, ok := body["roots"]; !ok {
		t.Errorf("missing roots in sandbox error")
	}
}

// TestTree_NotADirectory returns 400 when path resolves to a file.
func TestTree_NotADirectory(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "a.log")
	_ = os.WriteFile(filePath, []byte("x"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/tree?path=" + filePath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestTasks_NotFound returns 404 for unknown task IDs.
func TestTasks_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/tasks/deadbeef-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestIndex_GetMissing returns 404 when no index is cached.
func TestIndex_GetMissing(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("line1\nline2\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/index?path=" + f)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestCompress_TaskLifecycle runs a full POST /v1/compress → poll →
// /v1/tasks/{id} completion cycle on a small real file.
func TestCompress_TaskLifecycle(t *testing.T) {
	root := t.TempDir()
	// Compression threshold not enforced in webapi (only in index).
	src := filepath.Join(root, "big.log")
	// Make sure content compresses — 64 kB of a repeated line.
	content := strings.Repeat("this is a line of log\n", 3000)
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	output := filepath.Join(root, "big.log.zst")

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	req := rxtypes.CompressRequest{
		InputPath:        src,
		OutputPath:       &output,
		FrameSize:        "4K", // small so we get multiple frames
		CompressionLevel: 1,
		BuildIndex:       false,
	}
	buf, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := json.Marshal(resp.Header)
		body, _ := json.Marshal(resp.Body)
		t.Fatalf("compress status: got %d, body: %s, headers: %s", resp.StatusCode, body, b)
	}

	var tr rxtypes.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode tr: %v", err)
	}
	if tr.TaskID == "" {
		t.Fatalf("empty task_id")
	}

	// Poll for completion.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		statusResp, err := http.Get(ts.URL + "/v1/tasks/" + tr.TaskID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var st rxtypes.TaskStatusResponse
		if err := json.NewDecoder(statusResp.Body).Decode(&st); err != nil {
			_ = statusResp.Body.Close()
			t.Fatalf("decode status: %v", err)
		}
		_ = statusResp.Body.Close()
		if st.Status == "completed" {
			// Verify the output file exists.
			if _, err := os.Stat(output); err != nil {
				t.Errorf("output missing: %v", err)
			}
			// Verify result fields.
			if st.Result["success"] != true {
				t.Errorf("result.success: got %v", st.Result["success"])
			}
			return
		}
		if st.Status == "failed" {
			t.Fatalf("task failed: %v", st.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task did not complete in 10s")
}

// TestSamples_OffsetsAndLinesMutex enforces the 400 mutex check.
func TestSamples_OffsetsAndLinesMutex(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("a\nb\nc\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&offsets=1&lines=1", ts.URL, f))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestSamples_RequiresOffsetsOrLines — neither set is a 400.
func TestSamples_RequiresOffsetsOrLines(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("a\nb\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s", ts.URL, f))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestSamples_LineMode covers the happy-path line lookup with context.
func TestSamples_LineMode(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&lines=3&context=1", ts.URL, f))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	var body rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := body.Samples["3"]
	// line 3 ± 1 should give us lines 2,3,4.
	if len(got) != 3 {
		t.Errorf("expected 3 lines around line 3, got %d: %v", len(got), got)
	}
	if got[0] != "line2" || got[1] != "line3" || got[2] != "line4" {
		t.Errorf("samples wrong: %v", got)
	}
}

// TestSamples_LineRange covers range syntax "2-4".
func TestSamples_LineRange(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&lines=2-4", ts.URL, f))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	var body rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := body.Samples["2-4"]
	if len(got) != 3 {
		t.Errorf("expected 3 lines in range 2-4, got %d", len(got))
	}
	if got[0] != "two" || got[1] != "three" || got[2] != "four" {
		t.Errorf("range content wrong: %v", got)
	}
}

// TestSamples_BadOffsetFormat — malformed range returns 400.
func TestSamples_BadOffsetFormat(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("a\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&lines=not-a-number", ts.URL, f))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestSamples_CompressedFileWithOffsets rejects byte-offsets on .gz.
func TestSamples_CompressedFileWithOffsets(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log.gz")
	// Write a valid gzip header + payload.
	// Simplest: use the gzip package.
	// Just fake a .gz extension; compression.IsCompressed reads headers
	// too, but the extension alone is enough to flag it.
	_ = os.WriteFile(f, []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\x03"+
		"ABCDEFGH"), 0o644) // valid gzip magic + any data
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&offsets=1", ts.URL, f))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestIndex_PostBelowThreshold returns 400 for small files.
func TestIndex_PostBelowThreshold(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "small.log")
	_ = os.WriteFile(f, []byte("tiny"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	body, _ := json.Marshal(rxtypes.IndexRequest{Path: f})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestErrorEnvelope_Format checks that handler-returned errors come out
// as {"detail":"..."} not huma's default shape.
func TestErrorEnvelope_Format(t *testing.T) {
	ts := newTestServer(t)
	// 404 task
	resp, err := http.Get(ts.URL + "/v1/tasks/does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["detail"]; !ok {
		t.Errorf("no detail key in error body: %v", body)
	}
	if _, ok := body["title"]; ok {
		t.Errorf("huma default 'title' key leaked into error body (bad)")
	}
}

// TestRequestID_InResponse verifies the X-Request-ID header is always
// set and echoes a client-supplied value (within 128 chars).
func TestRequestID_InResponse(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/health", nil)
	req.Header.Set("X-Request-ID", "client-supplied-id-123")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := resp.Header.Get("X-Request-ID")
	if got != "client-supplied-id-123" {
		t.Errorf("X-Request-ID: got %q, want client-supplied-id-123", got)
	}

	// Missing header should be auto-generated.
	resp2, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if len(resp2.Header.Get("X-Request-ID")) < 10 {
		t.Errorf("auto-generated X-Request-ID too short: %q", resp2.Header.Get("X-Request-ID"))
	}
}

// TestFrameSizeParser covers the suffix matrix.
func TestFrameSizeParser(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"4M", 4 * 1024 * 1024},
		{"4MB", 4 * 1024 * 1024},
		{"4m", 4 * 1024 * 1024},
		{"1G", 1 << 30},
		{"512", 512},
		{"1K", 1024},
		{"1KB", 1024},
	}
	for _, tc := range cases {
		got, err := parseFrameSizeHTTP(tc.in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestFrameSizeParser_Errors — bad inputs.
func TestFrameSizeParser_Errors(t *testing.T) {
	bad := []string{"", "abc", "4X", "M"}
	for _, s := range bad {
		if _, err := parseFrameSizeHTTP(s); err == nil {
			t.Errorf("%s: expected error, got none", s)
		}
	}
}

// TestParseOffsetOrRange — moved to internal/samples. Stage 9 Round 2
// U rework migrated the parser into the shared package. Equivalent
// coverage lives in internal/samples/parser_test.go.

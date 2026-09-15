package webapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// analyzeOverHTTP posts an analyzing index request for path, waits for
// the task and returns its result.
func analyzeOverHTTP(t *testing.T, baseURL, path string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(rxtypes.IndexRequest{Path: path, Analyze: true})
	resp, err := http.Post(baseURL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("post status %d: %s", resp.StatusCode, b)
	}
	var task rxtypes.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	waitForTaskCompletion(t, baseURL, task.TaskID)

	r, err := http.Get(baseURL + "/v1/tasks/" + task.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	defer func() { _ = r.Body.Close() }()
	var status struct {
		Result map[string]any `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return status.Result
}

// seekableFixture writes a log of a few hundred `LINE <n>` lines, some
// empty and one long, and a seekable .zst copy of it in 1 KiB frames,
// in a fresh search root. It returns both paths.
func seekableFixture(t *testing.T) (plainPath, zstPath string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	var text strings.Builder
	for n := 1; n <= 400; n++ {
		switch {
		case n%50 == 0:
			text.WriteString("\n")
		case n == 222:
			fmt.Fprintf(&text, "LINE %d %s\n", n, strings.Repeat("y", 2500))
		default:
			fmt.Fprintf(&text, "LINE %d request handled in %d ms\n", n, n%13)
		}
	}
	plainPath = filepath.Join(root, "app.log")
	if err := os.WriteFile(plainPath, []byte(text.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	zstPath = plainPath + ".zst"
	dst, err := os.Create(zstPath) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = dst.Close() }()
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 1024, Level: 3, Workers: 1})
	src := strings.NewReader(text.String())
	if _, err := enc.Encode(context.Background(), src, int64(src.Len()), dst); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return plainPath, zstPath
}

// An analysis task over a seekable .zst answers the analysis of the
// decompressed text: every field an analysis computes equals the plain
// file's.
func TestIndexPost_AnalysisOfSeekableZstdEqualsThePlainFile(t *testing.T) {
	plainPath, zstPath := seekableFixture(t)
	ts := newTestServer(t)

	plain := analyzeOverHTTP(t, ts.URL, plainPath)
	fromZst := analyzeOverHTTP(t, ts.URL, zstPath)

	if fromZst["file_type"] != "seekable_zstd" {
		t.Fatalf("file_type = %v, want seekable_zstd", fromZst["file_type"])
	}
	for _, key := range []string{
		"analysis_performed", "line_count", "empty_line_count", "line_ending",
		"line_length", "longest_line", "anomaly_count", "anomaly_summary", "anomalies",
	} {
		if !reflect.DeepEqual(fromZst[key], plain[key]) {
			t.Errorf("%s: seekable %v, plain %v", key, fromZst[key], plain[key])
		}
	}
	if fromZst["analysis_performed"] != true {
		t.Errorf("analysis_performed = %v, want true", fromZst["analysis_performed"])
	}
}

// A compressed tar archive is the one compressed input rx cannot read
// as text, so an analysis request for one is refused with a 400 that
// says why, rather than a task that ends with analysis_performed false.
func TestIndexPost_AnalysisOfACompressedArchiveIsRefused(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	content := []byte("LINE 1 hello\n")
	if err := tw.WriteHeader(&tar.Header{Name: "app.log", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
	path := filepath.Join(root, "logs.tar.gz")
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ts := newTestServer(t)
	body, _ := json.Marshal(rxtypes.IndexRequest{Path: path, Analyze: true})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, answer)
	}
	if !strings.Contains(string(answer), "not a text file") {
		t.Errorf("the 400 does not say why: %s", answer)
	}
}

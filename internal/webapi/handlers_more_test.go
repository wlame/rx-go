package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestIndex_GetAfterManualCache covers the cache-hit branch of
// GET /v1/index, plus the full unifiedIndexToDict projection.
func TestIndex_GetAfterManualCache(t *testing.T) {
	root := t.TempDir()
	srcPath := filepath.Join(root, "payload.log")
	_ = os.WriteFile(srcPath, []byte("one\ntwo\nthree\n"), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	// Write a pre-built index so GET has something to load.
	info, _ := os.Stat(srcPath)
	lc := int64(3)
	elc := int64(0)
	le := "LF"
	llmax := int64(5)
	llavg := 3.33
	llmed := 3.0
	llp95 := 5.0
	llp99 := 5.0
	llstd := 1.2
	llmaxLn := int64(3)
	llmaxOff := int64(8)
	idx := &rxtypes.UnifiedFileIndex{
		Version:                 index.Version,
		SourcePath:              srcPath,
		SourceModifiedAt:        index.FormatMtime(info.ModTime()),
		SourceSizeBytes:         info.Size(),
		CreatedAt:               time.Now().UTC().Format(time.RFC3339Nano),
		FileType:                rxtypes.FileTypeText,
		IsText:                  true,
		LineIndex:               []rxtypes.LineIndexEntry{{LineNumber: 1, ByteOffset: 0}},
		LineCount:               &lc,
		EmptyLineCount:          &elc,
		LineEnding:              &le,
		LineLengthMax:           &llmax,
		LineLengthAvg:           &llavg,
		LineLengthMedian:        &llmed,
		LineLengthP95:           &llp95,
		LineLengthP99:           &llp99,
		LineLengthStddev:        &llstd,
		LineLengthMaxLineNumber: &llmaxLn,
		LineLengthMaxByteOffset: &llmaxOff,
		AnalysisPerformed:       true,
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save idx: %v", err)
	}

	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/index?path=" + srcPath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := json.Marshal(resp.Status)
		t.Fatalf("status: %v", string(b))
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Spot-check projected fields.
	if body["path"] != srcPath {
		t.Errorf("path: got %v, want %s", body["path"], srcPath)
	}
	if body["file_type"] != "text" {
		t.Errorf("file_type: got %v", body["file_type"])
	}
	if body["line_count"] != float64(3) {
		t.Errorf("line_count: got %v, want 3", body["line_count"])
	}
	if body["analysis_performed"] != true {
		t.Errorf("analysis_performed: got %v", body["analysis_performed"])
	}
	// line_length sub-object
	ll, ok := body["line_length"].(map[string]any)
	if !ok {
		t.Fatalf("line_length missing")
	}
	if ll["max"] != float64(5) {
		t.Errorf("line_length.max: got %v", ll["max"])
	}
	longest, ok := body["longest_line"].(map[string]any)
	if !ok {
		t.Fatalf("longest_line missing")
	}
	if longest["line_number"] != float64(3) {
		t.Errorf("longest_line.line_number: got %v", longest["line_number"])
	}
}

// TestIndex_PostSuccessPath runs the POST /v1/index task to completion.
func TestIndex_PostSuccessPath(t *testing.T) {
	root := t.TempDir()
	// File must be >= threshold (default 50MB). That's huge for a test;
	// use a tiny threshold in the request.
	f := filepath.Join(root, "data.log")
	// 2 KB is fine — threshold is in MB and request overrides env.
	content := strings.Repeat("line of data\n", 100)
	_ = os.WriteFile(f, []byte(content), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	tiny := 0 // 0 MB threshold — accept anything > 0 bytes (we have 2 KB).
	body, _ := json.Marshal(rxtypes.IndexRequest{Path: f, Threshold: &tiny})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := json.Marshal(resp.Header)
		t.Fatalf("status: %d, headers: %s", resp.StatusCode, b)
	}

	var tr rxtypes.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Poll for completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sr, err := http.Get(ts.URL + "/v1/tasks/" + tr.TaskID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var st rxtypes.TaskStatusResponse
		_ = json.NewDecoder(sr.Body).Decode(&st)
		_ = sr.Body.Close()
		if st.Status == "completed" {
			if st.Result["success"] != true {
				t.Errorf("result.success: got %v", st.Result["success"])
			}
			return
		}
		if st.Status == "failed" {
			t.Fatalf("failed: %v", st.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("index task did not complete")
}

// TestCompress_Conflict409 — second compress of same path → 409.
func TestCompress_Conflict409(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "big.log")
	// Enough data to take > 50ms to compress so the first job is still
	// running when we fire the second.
	content := strings.Repeat("this is a line\n", 100000)
	_ = os.WriteFile(f, []byte(content), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	out := filepath.Join(root, "big.log.zst")
	req := rxtypes.CompressRequest{
		InputPath:        f,
		OutputPath:       &out,
		FrameSize:        "4K",
		CompressionLevel: 3,
	}
	body, _ := json.Marshal(req)

	// First request — 200.
	resp1, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("1st: %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first status: %d", resp1.StatusCode)
	}

	// Second: should get 409 (or 200 if first already done). Try
	// immediately; race the compression.
	resp2, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("2nd: %v", err)
	}
	_ = resp2.Body.Close()
	// Accept 409 (in-progress) or 400 (output already exists after
	// fast-path completion).
	if resp2.StatusCode != http.StatusConflict && resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("2nd status: got %d, want 409 or 400", resp2.StatusCode)
	}
}

// TestCompress_InvalidLevel covers the level-validation branch.
func TestCompress_InvalidLevel(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	req := rxtypes.CompressRequest{
		InputPath:        f,
		FrameSize:        "4K",
		CompressionLevel: 99,
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestCompress_OutputExists returns 400 when the output is on disk and
// Force is false.
func TestCompress_OutputExists(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("content"), 0o644)
	out := filepath.Join(root, "a.log.zst")
	_ = os.WriteFile(out, []byte("existing"), 0o644) // pre-exists

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	req := rxtypes.CompressRequest{
		InputPath: f, OutputPath: &out, FrameSize: "4K", CompressionLevel: 3,
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestSamples_ByteOffset covers byte-offset mode on an uncompressed file.
func TestSamples_ByteOffset(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("abc\n123\nxyz\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	// Offset 5 is in the middle of "123\n".
	resp, err := http.Get(ts.URL + "/v1/samples?path=" + f + "&offsets=5&context=0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := body.Samples["5"]
	if len(got) != 1 || got[0] != "123" {
		t.Errorf("sample at offset 5: got %v, want [123]", got)
	}
}

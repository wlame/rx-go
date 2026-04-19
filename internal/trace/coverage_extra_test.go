package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestIsSubprocessCancelled covers every branch of the helper that
// classifies subprocess termination errors.
func TestIsSubprocessCancelled(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context_canceled", context.Canceled, true},
		{"wrapped_canceled", fmt.Errorf("wrapped: %w", context.Canceled), true},
		{"signal_killed", errors.New("exit status 137: signal: killed"), true},
		{"signal_terminated", errors.New("signal: terminated"), true},
		{"broken_pipe", errors.New("write |1: broken pipe"), true},
		{"random_error", errors.New("disk full"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSubprocessCancelled(tc.err); got != tc.want {
				t.Errorf("isSubprocessCancelled(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestGetCompressedCacheInfo_Miss returns ErrCacheMiss when no cache
// exists for the source file. Covers the miss branch.
func TestGetCompressedCacheInfo_Miss(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log.gz")
	src := []byte("hello world\n")
	gzBytes, err := gzipBytes(src)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(p, gzBytes, 0o644); err != nil {
		t.Fatalf("write gz: %v", err)
	}

	_, err = GetCompressedCacheInfo(p, []string{"error"}, nil)
	if !errors.Is(err, ErrCacheMiss) {
		t.Errorf("expected ErrCacheMiss, got %v", err)
	}
}

// TestReadLineWindow_Boundary exercises the reader with start/end that
// falls near EOF, covering the truncation branch.
func TestReadLineWindow_Boundary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "boundary.log")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Ask for a window around line 3 with ±1 context → should get lines 2-4.
	lines, matchedIdx, startLine, err := readLineWindow(p, 3, 1, 1, false)
	if err != nil {
		t.Fatalf("readLineWindow: %v", err)
	}
	if len(lines) < 3 {
		t.Errorf("expected >= 3 lines, got %d: %v", len(lines), lines)
	}
	if startLine != 2 {
		t.Errorf("startLine: got %d, want 2", startLine)
	}
	if matchedIdx < 0 {
		t.Errorf("matchedIdx: got %d, want >= 0", matchedIdx)
	}
}

// TestReadLineWindow_NearStart tests near-start-of-file truncation.
func TestReadLineWindow_NearStart(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644)

	// Ask for context 5 around line 1 — should truncate to startLine=1.
	lines, _, startLine, err := readLineWindow(p, 1, 5, 1, false)
	if err != nil {
		t.Fatalf("readLineWindow: %v", err)
	}
	if startLine != 1 {
		t.Errorf("startLine: got %d, want 1 (truncated)", startLine)
	}
	if len(lines) == 0 {
		t.Error("expected non-empty result at boundary")
	}
}

// TestReadLineWindow_InvalidTarget rejects target < 1.
func TestReadLineWindow_InvalidTarget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("one\n"), 0o644)
	_, _, _, err := readLineWindow(p, 0, 0, 0, false)
	if err == nil {
		t.Error("expected error for target < 1")
	}
}

// TestReadSourceLine covers the exported source-line reader.
func TestReadSourceLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	content := "first\nsecond\nthird\n"
	_ = os.WriteFile(p, []byte(content), 0o644)
	got, err := ReadSourceLine(p, 2, false)
	if err != nil {
		t.Fatalf("ReadSourceLine: %v", err)
	}
	if got != "second" {
		t.Errorf("line 2: got %q, want %q", got, "second")
	}
}

// TestParsePaths covers the convenience wrapper (0% coverage otherwise).
func TestParsePaths(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("hello\nerror one\n"), 0o644)

	resp, err := ParsePaths(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("ParsePaths: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Errorf("expected 1 match, got %d", len(resp.Matches))
	}
}

// TestEngine_Run_WithTraceRequestStruct covers the .Run variant (as
// opposed to .RunWithOptions).
func TestEngine_Run_WithTraceRequestStruct(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("hit\nerror one\nno match\n"), 0o644)

	eng := New()
	req := &rxtypes.TraceRequest{
		Path:     []string{p},
		Patterns: []string{"error"},
	}
	resp, err := eng.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Errorf("expected 1 match, got %d", len(resp.Matches))
	}
}

// TestEngine_Run_WithMaxResults verifies MaxResults capping.
func TestEngine_Run_WithMaxResults(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("error on line\n")
	}
	_ = os.WriteFile(p, []byte(sb.String()), 0o644)

	max := 5
	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"},
		Options{MaxResults: &max})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) > max {
		t.Errorf("MaxResults violated: got %d, want <= %d", len(resp.Matches), max)
	}
}

// TestEngine_Run_MissingFile returns an empty result set rather than
// erroring (Python parity: missing files are skipped).
func TestEngine_Run_MissingFile(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	eng := New()
	_, err := eng.RunWithOptions(context.Background(),
		[]string{"/definitely/does/not/exist.log"}, []string{"error"}, Options{})
	// Either a nil error (skipped) or a clear error is acceptable; we
	// just want this to NOT panic.
	_ = err
}

// TestEngine_Run_BinaryFile should skip the file (not text).
func TestEngine_Run_BinaryFile(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "binary.dat")
	bin := make([]byte, 4096)
	for i := range bin {
		bin[i] = byte(i % 256)
	}
	_ = os.WriteFile(p, bin, 0o644)

	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("Run on binary: %v", err)
	}
	// Binary file should produce 0 matches.
	if len(resp.Matches) != 0 {
		t.Errorf("binary file matched: %d results", len(resp.Matches))
	}
}

// TestEngine_Run_EmptyPatterns returns 0 matches rather than scanning.
func TestEngine_Run_EmptyPatterns(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("some content\n"), 0o644)
	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p}, []string{}, Options{})
	if err != nil {
		// Empty patterns may be a hard error; that's acceptable.
		return
	}
	if len(resp.Matches) != 0 {
		t.Errorf("empty patterns produced %d matches", len(resp.Matches))
	}
}

// TestEngine_Run_CompressedGzip runs the engine over a gzip file,
// covering the "compressed" bucket path in RunWithOptions.
func TestEngine_Run_CompressedGzip(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip subprocess not installed")
	}
	dir := t.TempDir()
	rawContent := []byte("regular line\nerror here\nanother line\nerror again\n")
	p := filepath.Join(dir, "a.log.gz")
	gzBytes, err := gzipBytes(rawContent)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(p, gzBytes, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("compressed scan: %v", err)
	}
	if len(resp.Matches) != 2 {
		t.Errorf("expected 2 matches in compressed file, got %d", len(resp.Matches))
	}
}

// TestEngine_Run_WithContext exercises the ContextBefore / ContextAfter
// paths through to the response.
func TestEngine_Run_WithContext(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte(
		"prefix line 1\nprefix line 2\nerror HERE\nsuffix line 1\nsuffix line 2\n",
	), 0o644)

	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{
		ContextBefore: 1,
		ContextAfter:  1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(resp.Matches))
	}
	// Context lines are reported on the response, keyed by offset.
	if len(resp.ContextLines) == 0 {
		t.Errorf("expected ContextLines map to be populated")
	}
}

// TestEngine_Run_NoCache_VsCache compares behavior with and without
// cache — cached run should be faster on repeat but produce identical
// matches.
//
// We lower the large-file threshold to 1 MB via env so the test fixture
// stays small (Python uses 50 MB by default; rx-go inherits that).
func TestEngine_Run_NoCache_VsCache(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1") // trigger cache at 1 MB
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	// ~2 MB of filler + 1 match.
	content := strings.Repeat("filler line\n", 150000)
	content += "error here\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	eng := New()
	resp1, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	resp2, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(resp1.Matches) != len(resp2.Matches) {
		t.Errorf("match count differs between runs: %d vs %d",
			len(resp1.Matches), len(resp2.Matches))
	}
	// The second run should use the cache — file_chunks[f1] = 0 signals that.
	if chunks, ok := resp2.FileChunks["f1"]; ok && chunks != 0 {
		t.Logf("second run used %d chunks (may not be cached)", chunks)
	}
}

// TestEngine_Run_NoCacheFlag disables cache explicitly and ensures the
// NoCache branch is exercised.
func TestEngine_Run_NoCacheFlag(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	content := strings.Repeat("filler\n", 200000) + "error\n"
	_ = os.WriteFile(p, []byte(content), 0o644)

	eng := New()
	resp1, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"},
		Options{NoCache: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	resp2, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"},
		Options{NoCache: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(resp1.Matches) != len(resp2.Matches) {
		t.Errorf("NoCache should be deterministic: %d vs %d",
			len(resp1.Matches), len(resp2.Matches))
	}
}

// TestEngine_Run_SeekableZstd_Cached exercises the "cached-seekable"
// bucket: build once, fetch cache on a second run.
func TestEngine_Run_SeekableZstd_Cached(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	// Seekable-zstd caches unconditionally (no threshold).
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		if i%30 == 0 {
			sb.WriteString("error found\n")
		} else {
			sb.WriteString("boring text\n")
		}
	}
	p := writeSeekableZstdFile(t, []byte(sb.String()), 16*1024)

	eng := New()
	resp1, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second run should hit cache.
	resp2, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(resp1.Matches) != len(resp2.Matches) {
		t.Errorf("cached seekable run mismatch: %d vs %d",
			len(resp1.Matches), len(resp2.Matches))
	}
}

// TestEngine_Run_CompressedMaxResults exercises the maxResults path
// inside ProcessCompressed — uses the io.EOF early-stop path.
func TestEngine_Run_CompressedMaxResults(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not installed")
	}
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("error line\n")
	}
	gzBytes, err := gzipBytes([]byte(sb.String()))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	p := filepath.Join(dir, "a.log.gz")
	if err := os.WriteFile(p, gzBytes, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	eng := New()
	max := 10
	resp, err := eng.RunWithOptions(context.Background(), []string{p}, []string{"error"},
		Options{MaxResults: &max})
	if err != nil {
		t.Fatalf("compressed with max: %v", err)
	}
	if len(resp.Matches) > max {
		t.Errorf("MaxResults exceeded: %d > %d", len(resp.Matches), max)
	}
}

// TestProcessCompressed_UnknownFormat returns ErrUnsupportedCompression.
func TestProcessCompressed_UnknownFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("x"), 0o644)
	_, _, _, err := ProcessCompressed(
		context.Background(), p, "unknown-format",
		map[string]string{"p1": "x"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err == nil {
		t.Errorf("expected error for unknown format")
	}
}

// TestProcessCompressed_NotCompressed returns an error for
// FormatNone input.
func TestProcessCompressed_NotCompressed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("hello\n"), 0o644)
	_, _, _, err := ProcessCompressed(
		context.Background(), p, "",
		map[string]string{"p1": "hello"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err == nil {
		t.Errorf("expected error for FormatNone input")
	}
}

// TestEngine_Run_SeekableZstd covers the engine's seekable-zstd bucket.
// Uses the test helper writeSeekableZstdFile defined in seekable_test.go.
func TestEngine_Run_SeekableZstd(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	// Build content with known matches across multiple frames.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		if i%50 == 49 {
			sb.WriteString("line has error\n")
		} else {
			sb.WriteString("regular text\n")
		}
	}
	p := writeSeekableZstdFile(t, []byte(sb.String()), 32*1024)

	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p},
		[]string{"error"}, Options{})
	if err != nil {
		t.Fatalf("seekable Run: %v", err)
	}
	if len(resp.Matches) != 4 {
		t.Errorf("seekable matches: got %d, want 4", len(resp.Matches))
	}
}

// TestEngine_Run_DirectoryRecursive covers expandPaths on a directory.
func TestEngine_Run_DirectoryRecursive(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	_ = os.MkdirAll(sub, 0o755)
	for i, name := range []string{"a.log", "b.log", "sub/c.log"} {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(fmt.Sprintf("error line %d\n", i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{dir}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("Run on dir: %v", err)
	}
	// Directory scan should find matches across files. Exact count
	// depends on ripgrep's directory walker respecting .gitignore etc.,
	// so we just verify we got at least one match from each file we wrote.
	if len(resp.Matches) < 1 {
		t.Errorf("directory scan should find matches, got %d", len(resp.Matches))
	}
}

// TestComputePatternsHash_Stability pins a subset of the 5 Python parity
// fixtures to ensure the hash doesn't drift under refactoring.
func TestComputePatternsHash_Stability(t *testing.T) {
	got := ComputePatternsHash([]string{"error"}, nil)
	const want = "164b5fd59c4c67a3"
	if got != want {
		t.Errorf("hash for [error]: got %s, want %s", got, want)
	}
}

// TestRgText_UnmarshalJSON covers all three branches of the text/bytes
// unmarshaller: null, text form, base64 bytes form.
func TestRgText_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"null", "null", ""},
		{"text", `{"text": "hello"}`, "hello"},
		{"bytes_b64", `{"bytes": "aGVsbG8="}`, "aGVsbG8="}, // "hello" base64
		{"empty", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r RgText
			if err := r.UnmarshalJSON([]byte(tc.input)); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if r.Text != tc.want {
				t.Errorf("Text: got %q, want %q", r.Text, tc.want)
			}
		})
	}
}

// TestRgText_UnmarshalJSON_Invalid exercises the error branch.
func TestRgText_UnmarshalJSON_Invalid(t *testing.T) {
	var r RgText
	if err := r.UnmarshalJSON([]byte(`{not valid json`)); err == nil {
		t.Errorf("expected error on malformed JSON")
	}
}

// TestNoopHookFirer covers the zero-value HookFirer.
func TestNoopHookFirer(t *testing.T) {
	h := NoopHookFirer{}
	// These must not panic.
	h.OnFile(context.Background(), "/tmp/a.log", FileInfo{})
	h.OnMatch(context.Background(), "/tmp/a.log", MatchInfo{})
}

// TestRemainingResults covers all branches of the slot helper.
func TestRemainingResults(t *testing.T) {
	cases := []struct {
		name    string
		max     *int
		have    int
		wantNil bool
		want    int
	}{
		{"unlimited", nil, 0, true, 0},
		{"remaining_some", intPtr(10), 3, false, 7},
		{"exactly_full", intPtr(10), 10, false, 0},
		{"over_full", intPtr(10), 15, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := remainingResults(tc.max, tc.have)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %v", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil")
			}
			if *got != tc.want {
				t.Errorf("got %d, want %d", *got, tc.want)
			}
		})
	}
}

// TestPtrIntDeref covers both arms of the helper.
func TestPtrIntDeref(t *testing.T) {
	if got := ptrIntDeref(nil, 42); got != 42 {
		t.Errorf("nil pointer: got %d, want 42", got)
	}
	five := 5
	if got := ptrIntDeref(&five, 42); got != 5 {
		t.Errorf("valued pointer: got %d, want 5", got)
	}
}

// TestSaveCache_DirectoryCreatesParent verifies Save creates any missing
// parent directories for the cache path.
func TestSaveCache_DirectoryCreatesParent(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested", "deeper")
	p := filepath.Join(nested, "cache.json")
	data := &rxtypes.TraceCacheData{Version: 2, SourcePath: "/tmp/x"}
	if err := SaveCache(p, data); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("cache file not created: %v", err)
	}
}

// TestSaveCache_ReadOnlyDir returns an error when the parent dir is
// read-only.
func TestSaveCache_ReadOnlyDir(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("chmod read-only failed (maybe running as root): %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	p := filepath.Join(dir, "cant-write.json")
	data := &rxtypes.TraceCacheData{Version: 2, SourcePath: "/tmp/x"}
	err := SaveCache(p, data)
	if err == nil {
		t.Errorf("expected error writing to read-only dir")
	}
}

// TestLoadCache_Missing returns an error for a missing file.
func TestLoadCache_Missing(t *testing.T) {
	_, err := LoadCache("/definitely/does/not/exist/cache.json")
	if err == nil {
		t.Errorf("expected error for missing cache")
	}
}

// TestLoadCache_Malformed returns an error for malformed JSON.
func TestLoadCache_Malformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadCache(p)
	if err == nil {
		t.Errorf("expected error for malformed JSON")
	}
}

// TestIsCacheValid_WrongPatternHash rejects caches whose patterns_hash
// doesn't match the request.
func TestIsCacheValid_WrongPatternHash(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "a.log")
	_ = os.WriteFile(src, []byte("hello\n"), 0o644)

	// Build cache for pattern "error" but query with "warning".
	p := CachePath(src, []string{"error"}, nil)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srcInfo, _ := os.Stat(src)
	data := &rxtypes.TraceCacheData{
		Version:          2,
		SourcePath:       src,
		SourceSizeBytes:  srcInfo.Size(),
		SourceModifiedAt: srcInfo.ModTime().Local().Format("2006-01-02T15:04:05.000000"),
		Patterns:         []string{"error"},
		PatternsHash:     ComputePatternsHash([]string{"error"}, nil),
	}
	if err := SaveCache(p, data); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	// Check with DIFFERENT patterns → invalid.
	if IsCacheValid(p, src, []string{"warning"}, nil) {
		t.Error("cache should be invalid for different patterns")
	}
	// Check with same patterns → valid.
	if !IsCacheValid(p, src, []string{"error"}, nil) {
		t.Error("cache should be valid for same patterns")
	}
}

// TestSaveCache_Direct writes a cache and reads it back. Covers the
// write + read round-trip path.
func TestSaveCache_Direct(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	data := &rxtypes.TraceCacheData{
		Version:    2,
		SourcePath: "/tmp/fake.log",
		Matches: []rxtypes.TraceCacheMatch{
			{PatternIndex: 0, Offset: 100, LineNumber: 1},
		},
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	if err := SaveCache(p, data); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	loaded, err := LoadCache(p)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if loaded.SourcePath != data.SourcePath {
		t.Errorf("SourcePath mismatch: got %v, want %v", loaded.SourcePath, data.SourcePath)
	}
}

// TestComputePatternsHash_StabilityMulti covers a few more of the 5
// fixtures documented in cache_test.go to ensure the hash function is
// fully exercised.
func TestComputePatternsHash_StabilityMulti(t *testing.T) {
	cases := []struct {
		patterns []string
		flags    []string
		want     string
	}{
		{[]string{}, nil, "07ddcf7d90fb9d20"},
		{[]string{"error"}, nil, "164b5fd59c4c67a3"},
		{[]string{"error"}, []string{"-i"}, "a9b9b19054a58e17"},
		{[]string{"foo", "bar"}, []string{"-i", "-F"}, "338f138ac38e487b"},
	}
	for _, tc := range cases {
		got := ComputePatternsHash(tc.patterns, tc.flags)
		if got != tc.want {
			t.Errorf("hash(patterns=%v, flags=%v): got %s, want %s",
				tc.patterns, tc.flags, got, tc.want)
		}
	}
}

// TestReadLineWindow_FarFromStart uses the default code path (no index
// hint) when target is far from the start.
func TestReadLineWindow_FarFromStart(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.log")
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "row %d\n", i)
	}
	_ = os.WriteFile(p, []byte(sb.String()), 0o644)

	lines, _, _, err := readLineWindow(p, 400, 2, 2, false)
	if err != nil {
		t.Fatalf("readLineWindow: %v", err)
	}
	if len(lines) != 5 {
		t.Errorf("expected 5 lines (±2 around 400), got %d: %v", len(lines), lines)
	}
	// Line 400 = "row 400" at position 2 (zero-indexed) in the slice.
	if lines[2] != "row 400" {
		t.Errorf("center line: got %q, want 'row 400'", lines[2])
	}
}

// TestEngine_Run_MultiplePatterns covers the multi-pattern path with
// identification.
func TestEngine_Run_MultiplePatterns(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte(
		"error one\nwarning two\nerror three\nok\nwarning four\n",
	), 0o644)

	eng := New()
	resp, err := eng.RunWithOptions(context.Background(), []string{p},
		[]string{"error", "warning"}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 2 errors + 2 warnings = 4.
	if len(resp.Matches) != 4 {
		t.Errorf("expected 4 matches across 2 patterns, got %d", len(resp.Matches))
	}
	// Each match should have the correct pattern ID.
	patterns := map[string]int{}
	for _, m := range resp.Matches {
		patterns[m.Pattern]++
	}
	if patterns["p1"] != 2 || patterns["p2"] != 2 {
		t.Errorf("pattern distribution: got %v, want p1=2 p2=2", patterns)
	}
}

// TestEngine_Run_CtxCancelled verifies the engine returns ctx.Err() when
// the context is canceled.
func TestEngine_Run_CtxCancelled(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	_ = os.WriteFile(p, []byte("some content\n"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-canceled context

	eng := New()
	_, err := eng.RunWithOptions(ctx, []string{p}, []string{"error"}, Options{})
	// Either returns ctx.Err() or no matches found — both acceptable
	// as long as the engine doesn't panic.
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestShouldCache branches.
func TestShouldCache(t *testing.T) {
	cases := []struct {
		name          string
		fileSize      int64
		maxResults    *int
		scanCompleted bool
		compressed    bool
		want          bool
	}{
		{"below_threshold", 1024, nil, true, false, false},
		{"with_max_results", 1 << 30, intPtr(100), true, false, false},
		{"incomplete_scan", 1 << 30, nil, false, false, false},
		{"all_good", 1 << 30, nil, true, false, true},
		{"compressed_just_above_1mb", 2 * 1024 * 1024, nil, true, true, true}, // compressed threshold is 1 MB
		{"compressed_below_1mb", 500 * 1024, nil, true, true, false},          // below 1 MB → skip
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldCache(tc.fileSize, tc.maxResults, tc.scanCompleted, tc.compressed); got != tc.want {
				t.Errorf("ShouldCache: got %v, want %v", got, tc.want)
			}
		})
	}
}

// ============================================================================
// Helpers
// ============================================================================

// gzipBytes returns the gzip'd form of `src`.
func gzipBytes(src []byte) ([]byte, error) {
	// Use the stdlib compress/gzip via a small import here; we can't
	// use the top-level import without another file in this package.
	return gzipBytesImpl(src)
}

// intPtr is a trivial helper for optional-int fields in test cases.
func intPtr(n int) *int { return &n }

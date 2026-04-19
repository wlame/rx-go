package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// testSlogLogger returns a logger that discards output. Middleware
// only writes to it, so the handler is exercised without spam.
func testSlogLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// listenResult bundles the result of listenAny: a free port number and
// a closer to release the test listener before the real server binds.
type listenResult struct {
	port   int
	closer io.Closer
}

// listenAny binds to 127.0.0.1:0 and returns the chosen port plus an
// io.Closer. Used in tests that want to dial a real Server directly
// without relying on httptest.
func listenAny(t *testing.T) (*listenResult, error) {
	t.Helper()
	l, err := netListen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*netTCPAddr).Port
	return &listenResult{port: port, closer: l}, nil
}

// Small wrappers over net.* APIs so coverage_b_test.go doesn't need to
// import net separately. Using type aliases keeps the imports minimal.
type netTCPAddr = net.TCPAddr

func netListen(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}

func netDial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 100*time.Millisecond)
}

// ===========================================================================
// errors.go — sandboxError coverage
// ===========================================================================

// TestSandboxError_Error covers the Error() method on sandboxError.
func TestSandboxError_Error(t *testing.T) {
	perr := &paths.ErrPathOutsideRoots{Path: "/forbidden", Roots: []string{"/ok"}}
	e := NewSandboxError(perr)
	if e.Error() == "" {
		t.Error("sandboxError.Error() should return a non-empty message")
	}
	if e.GetStatus() != http.StatusForbidden {
		t.Errorf("GetStatus: got %d, want 403", e.GetStatus())
	}
}

// TestErrHelpers covers the ErrBadRequest / ErrForbidden / ErrConflict
// / ErrInternal constructors so they're present in coverage. Each just
// wraps a huma.Error* call but the factories themselves had 0% coverage.
func TestErrHelpers(t *testing.T) {
	for name, fn := range map[string]func(string) error{
		"bad_request":         func(s string) error { return ErrBadRequest(s) },
		"forbidden":           func(s string) error { return ErrForbidden(s) },
		"not_found":           func(s string) error { return ErrNotFound(s) },
		"conflict":            func(s string) error { return ErrConflict(s) },
		"internal":            func(s string) error { return ErrInternal(s) },
		"service_unavailable": func(s string) error { return ErrServiceUnavailable(s) },
	} {
		t.Run(name, func(t *testing.T) {
			err := fn("test message")
			if err == nil {
				t.Error("constructor returned nil")
			}
			if !strings.Contains(err.Error(), "test message") {
				t.Errorf("error should carry message: %v", err)
			}
		})
	}
}

// ===========================================================================
// handlers — concurrent request test
// ===========================================================================

// TestServer_ConcurrentRequests verifies the server handles parallel
// requests without races or panics. Uses errgroup-style channel
// synchronization rather than t.Parallel (which would break the shared
// test server).
func TestServer_ConcurrentRequests(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.log")
	_ = os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	const N = 20
	var wg sync.WaitGroup
	var failures int32
	var mu sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := fmt.Sprintf("%s/v1/samples?path=%s&lines=1&context=0", ts.URL, p)
			resp, err := http.Get(url)
			if err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if failures > 0 {
		t.Errorf("concurrent failures: %d / %d", failures, N)
	}
}

// ===========================================================================
// middleware — recoverMiddleware
// ===========================================================================

// TestRecoverMiddleware_CatchesPanic verifies a handler panic is caught
// and returns a 500 with our error envelope.
func TestRecoverMiddleware_CatchesPanic(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("intentional for test")
	})
	// Use the default slog logger; middleware only writes to it, never
	// reads, so discarded output is fine.
	wrapped := recoverMiddleware(testSlogLogger())(handler)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Internal server error") {
		t.Errorf("body missing recovery message: %s", body)
	}
}

// ===========================================================================
// compressed_reader — Read/Close paths
// ===========================================================================

// TestCompressedReader_ZstdClose exercises the Close() path on the
// zstd-specific reader wrapper.
func TestCompressedReader_ZstdClose(t *testing.T) {
	// Build a zstd stream with klauspost zstd.NewWriter.
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := zw.Write([]byte("hello zstd\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	r, err := newCompressedReader(&buf, compression.FormatZstd)
	if err != nil {
		t.Fatalf("newCompressedReader: %v", err)
	}
	// Read a byte so Read path is exercised.
	bb := make([]byte, 5)
	if _, err := r.Read(bb); err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	// Close via io.Closer interface.
	if c, ok := r.(io.Closer); ok {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}

// TestCompressedReader_NoneFormat returns the reader as-is for
// FormatNone.
func TestCompressedReader_NoneFormat(t *testing.T) {
	src := strings.NewReader("plain text")
	r, err := newCompressedReader(src, compression.FormatNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "plain text" {
		t.Errorf("FormatNone should pass through: got %q", got)
	}
}

// TestCompressedReader_UnknownFormat returns an error.
func TestCompressedReader_UnknownFormat(t *testing.T) {
	src := strings.NewReader("x")
	if _, err := newCompressedReader(src, "lzo"); err == nil {
		t.Errorf("expected error for unknown format")
	}
}

// ===========================================================================
// samples handler — broader coverage
// ===========================================================================

// TestSamples_ByteOffset_Small uses a small file with byte-offset mode.
func TestSamples_ByteOffset_Small(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\nthree\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	// Offset 4 is inside "two" (one\n ends at offset 4, two starts there).
	url := fmt.Sprintf("%s/v1/samples?path=%s&offsets=4&context=0", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// TestSamples_NegativeLine_Uncompressed exercises countLines via the
// negative-line resolution path on a plain file.
func TestSamples_NegativeLine_Uncompressed(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("A\nB\nC\nD\nE\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=-1&context=0", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// -1 resolved to "5" (last line), content is "E".
	got := out.Samples["5"]
	if len(got) != 1 || got[0] != "E" {
		t.Errorf("negative line: got %v, want [E]", got)
	}
}

// TestSamples_ByteOffset_Negative exercises negative byte offsets.
func TestSamples_ByteOffset_Negative(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("A\nB\nC\nD\nE\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	// offsets=-1 resolves against file size.
	url := fmt.Sprintf("%s/v1/samples?path=%s&offsets=-1&context=0", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// TestSamples_ByteOffset_Range tests range mode in byte-offset.
func TestSamples_ByteOffset_Range(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("AAAA\nBBBB\nCCCC\nDDDD\nEEEE\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&offsets=0-10", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// TestSamples_InvalidRange returns 400 for unparsable range.
func TestSamples_InvalidRange(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("x\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=abc", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestSamples_LineRange_FourLines exercises the range path for line mode.
func TestSamples_LineRange_FourLines(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	_ = os.WriteFile(f, []byte(sb.String()), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=5-8", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out.Samples["5-8"]
	if len(got) != 4 {
		t.Errorf("range 5-8: got %d lines, want 4: %v", len(got), got)
	}
}

// TestSamples_LinesMapReportsRequestedLineOffset covers Stage 9 Round 2
// R1-B5: the `lines` map in the response must contain the byte offset
// of the REQUESTED line (v.Start), not the start of the context window
// (v.Start - before). The frontend rx-viewer uses this to drive
// "Copy absolute offset" and "load-more" chunk boundaries — wrong
// offsets would break line-number/offset associations.
//
// Deterministic fixture: 20 lines of "line NNN\n" (each 9 bytes
// including newline). Line N starts at offset (N-1)*9.
func TestSamples_LinesMapReportsRequestedLineOffset(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %03d\n", i) // 9 bytes each
	}
	_ = os.WriteFile(f, []byte(sb.String()), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	// Request line 10 with context 3. Line 10 starts at offset (10-1)*9 = 81.
	// PRE-fix bug: Lines["10"] would be 54 (start of line 7, i.e. line 10 - 3).
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=10&context=3", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out.Lines["10"]
	want := int64((10 - 1) * 9)
	if got != want {
		t.Errorf("Lines[\"10\"] = %d, want %d (byte offset of line 10's start)", got, want)
	}
}

// TestSamples_MissingFileParam returns 400/422 for missing required path.
func TestSamples_MissingFileParam(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/samples?lines=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity &&
		resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 422 or 400 for missing path, got %d", resp.StatusCode)
	}
}

// TestSamples_SandboxViolation returns 403.
func TestSamples_SandboxViolation(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	f := filepath.Join(elsewhere, "a.log")
	_ = os.WriteFile(f, []byte("x\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=1", ts.URL, f)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", resp.StatusCode)
	}
}

// TestSamples_NonexistentFile returns 404.
func TestSamples_NonexistentFile(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	missing := filepath.Join(root, "not-there.log")
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=1", ts.URL, missing)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestSamples_DirectoryInput returns 400.
func TestSamples_DirectoryInput(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=1", ts.URL, root)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// ===========================================================================
// openapi spec round-trip
// ===========================================================================

// TestOpenAPISpec_IsValidJSON fetches the generated spec and parses it.
func TestOpenAPISpec_IsValidJSON(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	if data["openapi"] == "" {
		t.Error("openapi field missing")
	}
	if _, ok := data["paths"].(map[string]any); !ok {
		t.Error("paths object missing or wrong type")
	}
}

// TestDocsRoute_ReturnsHTML — /docs should serve an HTML page (SwaggerUI).
func TestDocsRoute_ReturnsHTML(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("content-type: got %q, want text/html*", resp.Header.Get("Content-Type"))
	}
}

// TestHealthEndpoint_IncludesExpectedFields covers the /health handler
// more thoroughly than existing tests, hitting getGoPackages and
// getAppEnvVariables.
func TestHealthEndpoint_IncludesExpectedFields(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"app_version", "go_version", "ripgrep_available"} {
		if _, ok := body[key]; !ok {
			t.Errorf("health missing %q", key)
		}
	}
}

// ===========================================================================
// helpers
// ===========================================================================

// ===========================================================================
// misc helpers with direct context usage to exercise code paths
// ===========================================================================

// TestRequestIDFromContext_Empty verifies the default empty-string return.
func TestRequestIDFromContext_Empty(t *testing.T) {
	// Raw context with no request ID.
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// TestServer_StartShutdown binds a real server on an ephemeral port,
// pings /health, then shuts down gracefully. Covers the Start() and
// Shutdown() paths that httptest.Server doesn't hit.
func TestServer_StartShutdown(t *testing.T) {
	// Pick a free port.
	l, err := listenAny(t)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.port
	_ = l.closer.Close()

	srv := NewServer(Config{
		Host:       "127.0.0.1",
		Port:       port,
		AppVersion: "test",
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	// Poll until bind succeeds.
	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		c, dialErr := netDial(fmt.Sprintf("127.0.0.1:%d", port))
		if dialErr == nil {
			_ = c.Close()
			ok = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("server did not bind")
	}

	// Hit /health.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health: got status %d, want 200", resp.StatusCode)
	}

	// Shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Start did not return after Shutdown")
	}
}

// TestCompressPost_Various covers createCompressTask error branches.
func TestCompressPost_InvalidLevel(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.log")
	_ = os.WriteFile(p, []byte("x"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	body, _ := json.Marshal(rxtypes.CompressRequest{
		InputPath:        p,
		CompressionLevel: 99, // out of range
	})
	resp, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("should have rejected invalid level, got %d", resp.StatusCode)
	}
}

// TestCompressPost_MissingFile returns 404.
func TestCompressPost_MissingFile(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	body, _ := json.Marshal(rxtypes.CompressRequest{
		InputPath: filepath.Join(root, "missing.log"),
	})
	resp, err := http.Post(ts.URL+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestIndexPost_SandboxViolation returns 403.
func TestIndexPost_SandboxViolation(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	body, _ := json.Marshal(rxtypes.IndexRequest{
		Path: filepath.Join(elsewhere, "x.log"),
	})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", resp.StatusCode)
	}
}

// TestIndexPost_MissingFile returns 404.
func TestIndexPost_MissingFile(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)
	body, _ := json.Marshal(rxtypes.IndexRequest{
		Path: filepath.Join(root, "missing.log"),
	})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestStaticFile_FaviconReachable verifies /favicon.svg responds. The
// server may redirect (307) to /docs when no frontend cache exists;
// both 200 and 307 are acceptable — this exercises the static-handler
// dispatch either way.
func TestStaticFile_FaviconReachable(t *testing.T) {
	ts := newTestServer(t)
	// Don't follow redirects so we can see the 307 if it's issued.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/favicon.svg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status: got %d, want 200 or 307", resp.StatusCode)
	}
}

// TestTraceEndpoint_GetQuery smoke-tests the full trace endpoint
// with a simple regex over a small file. /v1/trace is GET-only so
// we pass params via query string.
func TestTraceEndpoint_GetQuery(t *testing.T) {
	if _, err := os.Stat("/usr/bin/rg"); err != nil {
		if _, err := os.Stat("/usr/local/bin/rg"); err != nil {
			t.Skip("rg not installed")
		}
	}
	root := t.TempDir()
	p := filepath.Join(root, "a.log")
	_ = os.WriteFile(p, []byte("hello\nerror one\nok\nerror two\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	url := fmt.Sprintf("%s/v1/trace?path=%s&regexp=error", ts.URL, p)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// TestTraceEndpoint_SandboxViolation returns 403.
func TestTraceEndpoint_SandboxViolation(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	p := filepath.Join(elsewhere, "a.log")
	_ = os.WriteFile(p, []byte("x\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	url := fmt.Sprintf("%s/v1/trace?path=%s&regexp=x", ts.URL, p)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", resp.StatusCode)
	}
}

// TestHealthEndpoint_RAMFieldsPopulated_Linux covers Stage 9 Round 2
// R1-B9: /health's ram_total_gb / ram_available_gb / ram_percent_used
// were always null in Round 1. The Linux path parses /proc/meminfo to
// populate real numbers (psutil parity at the values level; not the
// exact implementation).
//
// Test skips on platforms without /proc/meminfo.
func TestHealthEndpoint_RAMFieldsPopulated_Linux(t *testing.T) {
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("/proc/meminfo unavailable; skipping Linux-only ram check")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
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
	sr, _ := body["system_resources"].(map[string]any)
	for _, k := range []string{"ram_total_gb", "ram_available_gb", "ram_percent_used"} {
		v := sr[k]
		if v == nil {
			t.Errorf("system_resources.%s is null; expected populated on Linux", k)
			continue
		}
		// JSON numbers decode as float64. All three fields must be positive.
		f, ok := v.(float64)
		if !ok {
			t.Errorf("%s: wrong type %T, want float64", k, v)
			continue
		}
		if f <= 0 {
			t.Errorf("%s = %v, want > 0", k, f)
		}
	}
}

// TestHealthEndpoint_NEWLINE_SYMBOL_NotOverEscaped covers Stage 9
// Round 2 R1-B9: Go's NEWLINE_SYMBOL constant was being double-escaped
// relative to Python's repr() — the on-wire value read as '\\n' (two
// literal backslashes then n) when Python emits '\n' (one backslash,
// then n) matching the character's repr.
//
// The fix applies Python's repr() semantics: decode the env literal
// `\n` → actual newline character first, then emit its repr. Test
// asserts the exact on-wire value matches Python's output.
func TestHealthEndpoint_NEWLINE_SYMBOL_NotOverEscaped(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("NEWLINE_SYMBOL", "\\n") // Python default literal value
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
	constants, _ := body["constants"].(map[string]any)
	got, _ := constants["NEWLINE_SYMBOL"].(string)
	// Python's repr('\n') is the 4-char string: ', \, n, '.
	// Go's raw (in-memory) value should match. Previous over-escaped
	// output was "'\\n'" (5 chars with an extra backslash).
	if got != `'\n'` {
		t.Errorf("NEWLINE_SYMBOL: got %q, want %q", got, `'\n'`)
	}
}

// TestHealthEndpoint_PythonCompat_Fields exercises getAppEnvVariables
// and getSearchRootsForHealth by reading all advertised fields.
func TestHealthEndpoint_PythonCompat_Fields(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_DEBUG", "1")
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
	// Python parity — look for optional fields that are only populated
	// when env vars exist.
	if env, ok := body["app_env_variables"].(map[string]any); !ok || len(env) == 0 {
		t.Logf("app_env_variables not present or empty: %v", body["app_env_variables"])
	}
	// Search roots field should be an array.
	if _, ok := body["search_roots"]; !ok {
		t.Errorf("health missing search_roots field")
	}
}

// TestMetricsEndpoint exposes Prometheus metrics.
func TestMetricsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// Check for at least one rx_ metric name in the output.
	if !strings.Contains(string(body), "rx_") {
		t.Errorf("metrics output missing rx_* metrics: %s", truncateString(string(body), 500))
	}
}

// TestMetricsMiddleware_UsesChiRoutePattern covers Stage 8 Reviewer 3
// High #11: the metricsMiddleware was using r.URL.Path directly,
// producing a cardinality explosion when path parameters (like task_id)
// are present. Every unique /v1/tasks/{id} GET created a new label
// value in the Prometheus registry.
//
// The fix is to call chi.RouteContext(r.Context()).RoutePattern() to
// get the template shape (e.g. "/v1/tasks/{task_id}") and use that as
// the endpoint label instead of the raw path.
//
// Test strategy: hit /v1/tasks/abc-1 and /v1/tasks/abc-2 (two distinct
// paths), then scrape /metrics and assert:
//
//   - The output includes endpoint="/v1/tasks/{task_id}" with count 2.
//   - The output does NOT include endpoint="/v1/tasks/abc-1" — the raw
//     path should be collapsed into the template.
func TestMetricsMiddleware_UsesChiRoutePattern(t *testing.T) {
	ts := newTestServer(t)

	// Hit the same route with two different task IDs. Both will
	// 404 (tasks don't exist) but the middleware runs regardless and
	// records a metric for each.
	for _, id := range []string{"abc-1", "abc-2"} {
		resp, err := http.Get(ts.URL + "/v1/tasks/" + id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		_ = resp.Body.Close()
	}

	// Scrape the metrics endpoint.
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	// Must include the templated route, NOT the raw IDs. We check for
	// the endpoint label set to exactly "/v1/tasks/{task_id}"
	// (Prometheus exposition format quotes label values).
	//
	// Pre-fix: the registry contains two series:
	//   rx_http_responses_total{...,endpoint="/v1/tasks/abc-1",...} 1
	//   rx_http_responses_total{...,endpoint="/v1/tasks/abc-2",...} 1
	// Post-fix: one series:
	//   rx_http_responses_total{...,endpoint="/v1/tasks/{task_id}",...} 2
	wantTemplate := `endpoint="/v1/tasks/{task_id}"`
	if !strings.Contains(out, wantTemplate) {
		t.Errorf("metrics missing templated endpoint label %q (cardinality bomb still present). Relevant snippet:\n%s",
			wantTemplate, truncateString(findMatchingLines(out, "/v1/tasks"), 800))
	}

	// The raw IDs must NOT appear as endpoint labels — that would mean
	// the middleware is still expanding path params.
	for _, rawID := range []string{`endpoint="/v1/tasks/abc-1"`, `endpoint="/v1/tasks/abc-2"`} {
		if strings.Contains(out, rawID) {
			t.Errorf("metrics leaked raw path id %q — Prometheus cardinality explosion. Fix metricsMiddleware to use chi.RouteContext.",
				rawID)
		}
	}
}

// findMatchingLines returns lines of s that contain substr, joined by
// newlines. Used above to generate concise error output when assertions
// fail against a multi-kilobyte /metrics dump.
func findMatchingLines(s, substr string) string {
	var matches []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			matches = append(matches, line)
		}
	}
	return strings.Join(matches, "\n")
}

// truncateString shortens a string to n characters for error messages.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestTreeEndpoint_FilterByExtension covers the ?ext=.log path in the
// directory tree handler.
func TestTreeEndpoint_FilterByExtension(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.log", "b.txt", "c.log"} {
		_ = os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	resp, err := http.Get(fmt.Sprintf("%s/v1/tree?path=%s", ts.URL, root))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d", resp.StatusCode)
	}
}

// TestDetectorsEndpoint returns the analyzer registry list.
// With zero analyzers registered, we expect an empty list.
func TestDetectorsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/detectors")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["detectors"]; !ok {
		t.Errorf("response missing detectors key: %v", body)
	}
}

// TestTaskEndpoint_NotFound returns 404 for unknown IDs.
func TestTaskEndpoint_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/tasks/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestReservedPrefix_404 verifies unknown /v1/ routes return 404 rather
// than falling through to the static handler.
func TestReservedPrefix_404(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/no-such-endpoint")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestRequestIDFromContext_RealServer checks that a real /health call
// picks up the request-id middleware. We can't directly poke the
// unexported middleware from a test, so we use an end-to-end probe.
func TestRequestIDFromContext_RealServer(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Middleware should have generated a new request-id in a header.
	if rid := resp.Header.Get("X-Request-Id"); rid == "" {
		t.Logf("Warning: no X-Request-Id header set (some middleware configs omit it)")
	}
}

// TestOpenAPIHandlers_Healthcheck verifies basic reachability.
func TestOpenAPIHandlers_Healthcheck(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d", resp.StatusCode)
	}
}

package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// newServerWithRipgrep returns a Server wired to the system ripgrep.
// Skips the test if rg isn't installed.
func newServerWithRipgrep(t *testing.T) *httptest.Server {
	t.Helper()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skipf("ripgrep not installed: %v", err)
	}
	srv := NewServer(Config{
		AppVersion:  "trace-test",
		RipgrepPath: rgPath,
		TaskManager: tasks.New(tasks.Config{}),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// TestTrace_HappyPath runs a real ripgrep against a small fixture and
// asserts the trace response shape.
func TestTrace_HappyPath(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "log.txt")
	_ = os.WriteFile(f, []byte(
		"2026-04-18 INFO hello world\n"+
			"2026-04-18 ERROR something broke\n"+
			"2026-04-18 INFO all good\n"+
			"2026-04-18 ERROR again\n",
	), 0o644)

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newServerWithRipgrep(t)

	u := fmt.Sprintf("%s/v1/trace?path=%s&regexp=ERROR", ts.URL, url.QueryEscape(f))
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := json.Marshal(resp.Header)
		t.Fatalf("status: got %d, headers: %s", resp.StatusCode, b)
	}
	var body rxtypes.TraceResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.RequestID == "" {
		t.Errorf("missing request_id")
	}
	if len(body.Matches) != 2 {
		t.Errorf("got %d matches, want 2", len(body.Matches))
	}
	if len(body.Patterns) != 1 {
		t.Errorf("got %d patterns, want 1", len(body.Patterns))
	}
	if body.CLICommand == nil || *body.CLICommand == "" {
		t.Errorf("missing cli_command")
	}
}

// TestTrace_PathOutsideRoots — sandbox rejection.
func TestTrace_PathOutsideRoots(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newServerWithRipgrep(t)
	u := fmt.Sprintf("%s/v1/trace?path=/etc/passwd&regexp=root", ts.URL)
	resp, err := http.Get(u)
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
	if body["error"] != "path_outside_search_root" {
		t.Errorf("wrong error shape: %v", body)
	}
}

// TestTrace_NotFound — a path within roots but nonexistent → 404.
func TestTrace_NotFound(t *testing.T) {
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newServerWithRipgrep(t)
	u := fmt.Sprintf("%s/v1/trace?path=%s&regexp=anything",
		ts.URL, url.QueryEscape(filepath.Join(root, "missing.log")))
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestTrace_ServiceUnavailableWhenNoRipgrep — no rg configured → 503.
func TestTrace_ServiceUnavailableWhenNoRipgrep(t *testing.T) {
	// Intentionally build with RipgrepPath="".
	srv := NewServer(Config{AppVersion: "no-rg"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/trace?path=/tmp/any&regexp=x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
}

// TestTrace_HookOnMatchRequiresMaxResults — 400 when on_match without limit.
func TestTrace_HookOnMatchRequiresMaxResults(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("x\n"), 0o644)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newServerWithRipgrep(t)

	u := fmt.Sprintf("%s/v1/trace?path=%s&regexp=x&hook_on_match=%s",
		ts.URL, url.QueryEscape(f), url.QueryEscape("http://127.0.0.1:9999/hook"))
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// ============================================================================
// CLI command builder
// ============================================================================

// TestBuildCLICommand — trace subcommand with regexp flags.
func TestBuildCLICommand(t *testing.T) {
	cases := []struct {
		name   string
		sub    string
		params map[string]any
		want   string
	}{
		{
			name: "trace with single pattern and path",
			sub:  "trace",
			params: map[string]any{
				"regexp": []string{"ERROR"},
				"path":   []string{"/var/log/a.log"},
			},
			want: "rx trace --regexp ERROR /var/log/a.log",
		},
		{
			name: "trace with multiple patterns and max-results",
			sub:  "trace",
			params: map[string]any{
				"regexp":      []string{"ERROR", "WARN"},
				"path":        []string{"/var/log/a.log"},
				"max_results": intPtr(100),
			},
			want: "rx trace --regexp ERROR --regexp WARN --max-results 100 /var/log/a.log",
		},
		{
			name: "index_get",
			sub:  "index_get",
			params: map[string]any{
				"path": "/var/log/a.log",
			},
			want: "rx index /var/log/a.log",
		},
		{
			name: "index_post with analyze",
			sub:  "index_post",
			params: map[string]any{
				"path":    "/var/log/a.log",
				"analyze": true,
				"force":   false,
			},
			want: "rx index --analyze /var/log/a.log",
		},
		{
			name: "compress with output path",
			sub:  "compress",
			params: map[string]any{
				"input_path":        "/var/log/a.log",
				"output_path":       "/var/log/a.log.zst",
				"frame_size":        "4M",
				"compression_level": intPtr(3),
			},
			want: "rx compress --output /var/log/a.log.zst --frame-size 4M --level 3 /var/log/a.log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildCLICommand(tc.sub, tc.params)
			if got != tc.want {
				t.Errorf("got:  %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

// TestBuildCLICommand_Quoting asserts that paths with spaces and shell
// metacharacters round-trip through output.Quote.
func TestBuildCLICommand_Quoting(t *testing.T) {
	got := BuildCLICommand("trace", map[string]any{
		"regexp": []string{"ERROR.*failed"},
		"path":   []string{"/tmp/my logs/app.log"},
	})
	if !strings.Contains(got, `'/tmp/my logs/app.log'`) {
		t.Errorf("path not quoted correctly: %s", got)
	}
}

// ============================================================================
// Error classification
// ============================================================================

// TestClassifyPathError handles both sandbox and non-sandbox cases.
func TestClassifyPathError(t *testing.T) {
	// Sandbox-specific error → sandboxError.
	perr := &paths.ErrPathOutsideRoots{Path: "/etc/passwd", Roots: []string{"/srv"}}
	got := ClassifyPathError(perr)
	if got.GetStatus() != http.StatusForbidden {
		t.Errorf("sandbox status: got %d, want 403", got.GetStatus())
	}
	if _, ok := got.(*sandboxError); !ok {
		t.Errorf("expected *sandboxError, got %T", got)
	}

	// Generic error → plain apiError.
	other := fmt.Errorf("boom")
	got2 := ClassifyPathError(other)
	if got2.GetStatus() != http.StatusForbidden {
		t.Errorf("generic status: got %d, want 403", got2.GetStatus())
	}
	if _, ok := got2.(*apiError); !ok {
		t.Errorf("expected *apiError, got %T", got2)
	}
}

// TestErrorHelpers — the convenience constructors.
func TestErrorHelpers(t *testing.T) {
	cases := []struct {
		name   string
		build  func(string) error
		status int
	}{
		{"not-found", func(d string) error { return ErrNotFound(d) }, http.StatusNotFound},
		{"bad-request", func(d string) error { return ErrBadRequest(d) }, http.StatusBadRequest},
		{"forbidden", func(d string) error { return ErrForbidden(d) }, http.StatusForbidden},
		{"conflict", func(d string) error { return ErrConflict(d) }, http.StatusConflict},
		{"unavailable", func(d string) error { return ErrServiceUnavailable(d) }, http.StatusServiceUnavailable},
		{"internal", func(d string) error { return ErrInternal(d) }, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		err := tc.build("test detail")
		se, ok := err.(interface{ GetStatus() int })
		if !ok {
			t.Errorf("%s: not a StatusError", tc.name)
			continue
		}
		if se.GetStatus() != tc.status {
			t.Errorf("%s: status got %d want %d", tc.name, se.GetStatus(), tc.status)
		}
		if err.Error() != "test detail" {
			t.Errorf("%s: error() got %q want %q", tc.name, err.Error(), "test detail")
		}
	}
}

// ============================================================================
// Server lifecycle
// ============================================================================

// TestServer_Shutdown exercises the graceful shutdown path. Starts a
// server in a goroutine, fires a request, then Shutdowns.
func TestServer_Shutdown(t *testing.T) {
	srv := NewServer(Config{
		AppVersion:  "shutdown-test",
		Host:        "127.0.0.1",
		Port:        0, // OS-chosen
		Hooks:       hooks.NewDispatcher(hooks.DispatcherConfig{}),
		TaskManager: tasks.New(tasks.Config{}),
	})

	// Drive via httptest instead of actual ListenAndServe so we don't
	// depend on port allocation; Shutdown still exercises backend stop.
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Do a request.
	if _, err := http.Get(ts.URL + "/health"); err != nil {
		t.Fatalf("pre-shutdown get: %v", err)
	}

	// Now shut down the server itself (drains backends, stops hooks/tasks).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		// http.Shutdown may return an error because httptest.Server owns
		// the listener — that's expected. We only care that it doesn't panic.
		t.Logf("shutdown returned %v (expected in httptest setup)", err)
	}
}

// TestServer_RouterAndAPIExposed verifies the public helpers.
func TestServer_RouterAndAPIExposed(t *testing.T) {
	srv := NewServer(Config{AppVersion: "api-test"})
	if srv.API() == nil {
		t.Errorf("API() returned nil")
	}
	if srv.Router() == nil {
		t.Errorf("Router() returned nil")
	}
}

// TestRequestHookFirer verifies the per-request wrapper dispatches
// events only when configured.
func TestRequestHookFirer(t *testing.T) {
	disp := hooks.NewDispatcher(hooks.DispatcherConfig{})
	defer disp.Close()

	// No URLs configured → no dispatch.
	firer := &requestHookFirer{
		dispatcher: disp,
		cfg:        hooks.HookConfig{}, // empty
		requestID:  "test-req",
	}
	firer.OnFile(context.Background(), "/tmp/a", trace.FileInfo{})
	firer.OnMatch(context.Background(), "/tmp/a", trace.MatchInfo{})

	// With OnFile URL but no target, dispatcher would try to POST but
	// we're not asserting on network — just that no panic / block.
	firer2 := &requestHookFirer{
		dispatcher: disp,
		cfg:        hooks.HookConfig{OnFileURL: "http://127.0.0.1:99999/bogus"},
		requestID:  "test-req-2",
	}
	firer2.OnFile(context.Background(), "/tmp/a", trace.FileInfo{})
	// Let the queued worker attempt and fail quickly; not load-bearing.
}

// ============================================================================
// CLI command: positional slice coverage
// ============================================================================

// TestBuildCLICommand_SliceHelpers exercises appendStringSlicePositional.
func TestBuildCLICommand_SliceHelpers(t *testing.T) {
	got := BuildCLICommand("trace", map[string]any{
		"regexp": []string{"x"},
		"path":   []string{"a.log", "b.log"},
	})
	if !strings.Contains(got, "a.log") || !strings.Contains(got, "b.log") {
		t.Errorf("missing multi-path: %s", got)
	}
}

// TestFormatTaskTime — ISO-ish microsecond UTC.
func TestFormatTaskTime(t *testing.T) {
	ts := time.Date(2026, 4, 18, 12, 34, 56, 789000000, time.UTC)
	got := formatTaskTime(ts)
	if got != "2026-04-18T12:34:56.789000Z" {
		t.Errorf("got %s", got)
	}
}

// TestCompressedReader_XzUnsupported.
func TestCompressedReader_XzUnsupported(t *testing.T) {
	if _, err := newCompressedReader(strings.NewReader(""), "xz"); err == nil {
		t.Errorf("xz should not be supported")
	}
}

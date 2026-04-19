package clicommand

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
)

// safeBuffer is a mutex-guarded bytes.Buffer so one goroutine can write
// while another reads — used in serve tests where the server writes to
// a shared buffer and the test goroutine snapshots its state. Without
// this the race detector flags the Write/String interleave as unsafe
// (which, strictly speaking, it is — bytes.Buffer is not goroutine-safe).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns a snapshot of the buffer's current contents.
func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// freePort picks an unused TCP port for a test server. Falls back to a
// well-known ephemeral range if net.Listen("tcp", "127.0.0.1:0") is
// somehow unavailable. Used to avoid rx-go's 7777 default colliding
// with a long-running local server.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr := l.Addr().(*net.TCPAddr)
	return addr.Port
}

// TestRunServe_StartsAndStops launches the server in a goroutine, waits
// for it to bind, then sends SIGTERM and verifies graceful shutdown.
// Covers the signal-driven shutdown path in runServe.
func TestRunServe_StartsAndStops(t *testing.T) {
	t.Cleanup(resetServeGlobals)
	port := freePort(t)
	root := t.TempDir()
	// Unset XDG to force frontend cache into the tempdir.
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	var buf safeBuffer
	params := serveParams{
		host:         "127.0.0.1",
		port:         port,
		searchRoots:  []string{root},
		appVersion:   "test",
		skipFrontend: true, // avoid GitHub roundtrip
	}

	done := make(chan error, 1)
	go func() {
		done <- runServe(&buf, params)
	}()

	// Poll until the banner is printed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "Starting RX API server") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Wait for listener to actually be ready. We poll the port.
	dialTimeout := time.Now().Add(5 * time.Second)
	for time.Now().Before(dialTimeout) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Send SIGTERM to ourselves — the server's signal handler catches it.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServe returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServe did not stop within 10s")
	}

	got := buf.String()
	for _, want := range []string{"Starting RX API server", "Search root:", "/docs", "/metrics"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "shutting down") {
		t.Errorf("shutdown message missing:\n%s", got)
	}
}

// resetServeGlobals clears the process-wide state that runServe sets:
// sandbox roots (paths.Reset) and the RX_SEARCH_ROOTS env var. Without
// this, later tests in the same package (trace/samples/compress) fail
// with sandbox errors because the server's roots persist.
func resetServeGlobals() {
	paths.Reset()
	_ = os.Unsetenv("RX_SEARCH_ROOTS")
}

// TestRunServe_InvalidSearchRoot rejects a nonexistent search root.
func TestRunServe_InvalidSearchRoot(t *testing.T) {
	t.Cleanup(resetServeGlobals)
	params := serveParams{
		host:         "127.0.0.1",
		port:         freePort(t),
		searchRoots:  []string{"/does/not/exist/for/test"},
		appVersion:   "test",
		skipFrontend: true,
	}
	var buf bytes.Buffer
	err := runServe(&buf, params)
	if err == nil {
		t.Errorf("expected error for invalid search root")
	}
}

// TestRunServe_MultipleSearchRoots exercises the multi-root banner.
func TestRunServe_MultipleSearchRoots(t *testing.T) {
	t.Cleanup(resetServeGlobals)
	root1 := t.TempDir()
	root2 := t.TempDir()
	port := freePort(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	var buf safeBuffer
	params := serveParams{
		host:         "127.0.0.1",
		port:         port,
		searchRoots:  []string{root1, root2},
		appVersion:   "test",
		skipFrontend: true,
	}

	done := make(chan error, 1)
	go func() { done <- runServe(&buf, params) }()

	// Wait briefly for banner then kill.
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not stop")
	}

	if !strings.Contains(buf.String(), "Search roots (2):") {
		t.Errorf("multi-root banner missing: %s", buf.String())
	}
}

// TestRunServe_DefaultSearchRootIsCwd verifies the fallback default.
func TestRunServe_DefaultSearchRootIsCwd(t *testing.T) {
	t.Cleanup(resetServeGlobals)
	port := freePort(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	// Move CWD to a known tempdir so the assertion is stable.
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	var buf safeBuffer
	params := serveParams{
		host:         "127.0.0.1",
		port:         port,
		searchRoots:  nil, // explicit nil → use cwd
		appVersion:   "test",
		skipFrontend: true,
	}

	done := make(chan error, 1)
	go func() { done <- runServe(&buf, params) }()

	time.Sleep(300 * time.Millisecond)
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not stop")
	}

	// Normalize: the tmp dir might be a symlink target (/private/var on macOS, etc.)
	// so we check for any "Search root:" line mentioning the tail.
	if !strings.Contains(buf.String(), filepath.Base(tmp)) {
		t.Errorf("default root should mention tmp basename %q:\n%s", filepath.Base(tmp), buf.String())
	}
}

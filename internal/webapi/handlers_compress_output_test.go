package webapi

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// compressOutputServer starts a test server whose task manager the test
// can reach, inside a search root of its own, and returns the server,
// the manager and the root.
func compressOutputServer(t *testing.T) (*httptest.Server, *tasks.Manager, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", filepath.Join(root, ".cache"))
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	manager := tasks.New(tasks.Config{})
	ts := httptest.NewServer(NewServer(Config{AppVersion: "unit-test", TaskManager: manager}))
	t.Cleanup(ts.Close)
	return ts, manager, root
}

// waitForTaskEnd waits for the task to complete or fail and returns it.
func waitForTaskEnd(t *testing.T, manager *tasks.Manager, taskID string) *tasks.Task {
	t.Helper()
	done, ok := manager.Done(taskID)
	if !ok {
		t.Fatalf("no task %s", taskID)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("task %s did not end", taskID)
	}
	task, _ := manager.Get(taskID)
	return task
}

// gzipLines is a gzip stream of n lines reading "LINE <n> ...".
func gzipLines(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	for i := 1; i <= n; i++ {
		_, _ = fmt.Fprintf(w, "LINE %d level=info took %d ms\n", i, i%97)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return buf.Bytes()
}

// A forced compression that fails leaves the existing output as it was:
// the task writes a temporary file and never removes the output first.
func TestCompress_FailedForcedTaskKeepsTheExistingOutput(t *testing.T) {
	ts, manager, root := compressOutputServer(t)
	body := gzipLines(t, 20000)
	input := filepath.Join(root, "app.log.gz")
	if err := os.WriteFile(input, body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	output := filepath.Join(root, "app.log.zst")
	if err := os.WriteFile(output, []byte("an older file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	status, answer := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath: input, FrameSize: "4K", CompressionLevel: 1, Force: true,
	})
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body: %v)", status, answer)
	}
	task := waitForTaskEnd(t, manager, answer["task_id"].(string))

	if task.Status != tasks.StatusFailed {
		t.Errorf("task status %s, want failed", task.Status)
	}
	if got, _ := os.ReadFile(output); string(got) != "an older file\n" { //nolint:gosec // test path
		t.Errorf("the output holds %q, want the older file", got)
	}
}

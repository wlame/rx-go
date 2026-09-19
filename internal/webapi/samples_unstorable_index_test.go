package webapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// When the line index cannot be stored, GET /v1/samples starts no index
// build: a build per request would read the whole file and throw the
// index away. Each request answers the lines by reading the file.
func TestSamples_NoIndexTaskWhenTheIndexCannotBeStored(t *testing.T) {
	cases := []struct {
		name   string
		breaks func(t *testing.T)
	}{
		{"read-only index directory", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a directory whatever its permissions")
			}
			dir := config.GetIndexCacheDir()
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		}},
		{"cache directory is a regular file", func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			t.Setenv("RX_CACHE_DIR", file)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSamplesBuildFixture(t, time.Minute)
			tc.breaks(t)
			// A build that should not happen must not hold the request
			// at the gate either: the test then fails on the count.
			f.gate.open()

			for _, headers := range []http.Header{{"Prefer": {"respond-async"}}, {}, {}} {
				resp, body := f.askSamples(t, headers)
				requireLines(t, resp.StatusCode, body, "LINE")
			}
			if calls := f.gate.calls.Load(); calls != 0 {
				t.Errorf("index builds started: %d, want 0", calls)
			}
			if tasks := f.manager.List(); len(tasks) != 0 {
				t.Errorf("tasks created: %d, want 0", len(tasks))
			}
		})
	}
}

// A POST /v1/index task whose index cannot be stored fails before it
// reads the file, with an error that names the cause, instead of
// building the whole index and failing at the save.
func TestIndexTask_FailsBeforeBuildingWhenTheIndexCannotBeStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", file)
	manager := tasks.New(tasks.Config{})
	task, _ := manager.Create(path, indexOperation)

	runIndexTask(manager, task.TaskID, path, rxtypes.IndexRequest{Path: path})

	ended, _ := manager.Get(task.TaskID)
	if ended.Status != tasks.StatusFailed {
		t.Fatalf("task status %q, want failed", ended.Status)
	}
	for _, part := range []string{"cannot store the line index", "not a directory"} {
		if !strings.Contains(ended.Error, part) {
			t.Errorf("task error %q does not say %q", ended.Error, part)
		}
	}
}

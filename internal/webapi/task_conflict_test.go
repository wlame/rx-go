package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
)

// TestTaskConflict_BodyNamesTheRunningTask asserts that a 409 for a path
// whose task is still running carries that task's ID as a top-level
// task_id member, so a client can follow the running task without
// reading it out of the sentence in detail. The sentence stays as it is.
//
// The running task is created in the manager directly and never
// finishes, so the 409 does not depend on how fast a real task runs.
func TestTaskConflict_BodyNamesTheRunningTask(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	input := filepath.Join(root, "app.log")
	if err := os.WriteFile(input, []byte("a log line\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	// The manager keys a task by the validated path, symlinks resolved.
	validated, err := paths.ValidatePathWithinRoots(input)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	cases := []struct {
		operation  string
		route      string
		body       string
		wantDetail string
	}{
		{"index", "/v1/index", `{"path": "` + input + `", "analyze": true}`, "Indexing already in progress"},
		{"compress", "/v1/compress", `{"input_path": "` + input + `"}`, "Compression already in progress"},
	}
	for _, tc := range cases {
		t.Run(tc.operation, func(t *testing.T) {
			manager := tasks.New(tasks.Config{})
			running, _ := manager.Create(validated, tc.operation)
			ts := httptest.NewServer(NewServer(Config{AppVersion: "unit-test", TaskManager: manager}))
			t.Cleanup(ts.Close)

			resp, err := http.Post(ts.URL+tc.route, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", resp.StatusCode)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["task_id"] != running.TaskID {
				t.Errorf("task_id = %v, want %s", body["task_id"], running.TaskID)
			}
			detail, _ := body["detail"].(string)
			if !strings.HasPrefix(detail, tc.wantDetail) || !strings.Contains(detail, "(task: "+running.TaskID+")") {
				t.Errorf("detail = %q, want the sentence naming task %s", detail, running.TaskID)
			}
		})
	}
}

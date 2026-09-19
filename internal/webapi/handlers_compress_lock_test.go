package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// postIndex POSTs req to /v1/index and returns the status and body.
func postIndex(t *testing.T, url string, req rxtypes.IndexRequest) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// app.log and app.log.gz both compress to app.log.zst by default. While
// the compression of one of them runs, it holds app.log.zst: a
// compression of the other is refused with 409 naming that output and
// the running task, and so is an index build of the output. Once the
// running task ends, the output is free again.
func TestCompress_SecondTaskForTheSameOutputIsRefusedWhileTheFirstRuns(t *testing.T) {
	ts, manager, root := compressOutputServer(t)
	plain := filepath.Join(root, "app.log")
	if err := os.WriteFile(plain, []byte("LINE 1 level=info\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gz := filepath.Join(root, "app.log.gz")
	if err := os.WriteFile(gz, gzipLines(t, 1000), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	output := filepath.Join(root, "app.log.zst")
	// An older output is there, so the index request below has a file
	// to look at; force lets the compression replace it.
	if err := os.WriteFile(output, []byte("LINE 1 an older output\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The running compression of app.log, held as the handler holds it.
	running, _, _ := manager.CreateHolding("compress", plain, output)

	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath: gz, FrameSize: "4K", CompressionLevel: 1, Force: true,
	})
	if status != http.StatusConflict || body["task_id"] != running.TaskID {
		t.Fatalf("compress: status %d, body %v; want 409 with task %s", status, body, running.TaskID)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "Compression already in progress for "+output) {
		t.Errorf("detail %q does not name the output", detail)
	}
	status, body = postIndex(t, ts.URL, rxtypes.IndexRequest{Path: output, Analyze: true})
	if status != http.StatusConflict || body["task_id"] != running.TaskID {
		t.Errorf("index of the output: status %d, body %v; want 409 with task %s", status, body, running.TaskID)
	}

	manager.Complete(running.TaskID, rxtypes.CompressTaskResult{})
	status, body = postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath: gz, FrameSize: "4K", CompressionLevel: 1, Force: true,
	})
	if status != http.StatusOK {
		t.Fatalf("compress after the first ended: status %d, body %v; want 200", status, body)
	}
	if task := waitForTaskEnd(t, manager, body["task_id"].(string)); task.Status != "completed" {
		t.Errorf("task %s: %s", task.Status, task.Error)
	}
}

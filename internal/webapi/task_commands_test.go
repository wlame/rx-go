package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// POST /v1/compress and POST /v1/logs/index answer the command that
// does what their task does, at once, and GET /v1/tasks/{id} carries it
// while the task runs and when it is done, as it does for an index
// task.
func TestTasks_CompressAndChainIndexAnswerTheirCommand(t *testing.T) {
	base, root, _ := compressInputServer(t)
	output := filepath.Join(root, "app.log.zst")
	status, body := postCompress(t, base, rxtypes.CompressRequest{InputPath: filepath.Join(root, "app.log"), OutputPath: &output, FrameSize: "16K", CompressionLevel: 3})
	if status != 200 {
		t.Fatalf("POST /v1/compress: %d %v", status, body)
	}
	cli, _ := body["cli_command"].(string)
	if !strings.HasPrefix(cli, "rx compress ") || !strings.Contains(cli, filepath.Join(root, "app.log")) {
		t.Errorf("POST /v1/compress answered cli_command %q, want an rx compress command", cli)
	}
	taskID, _ := body["task_id"].(string)
	done := awaitTaskOverHTTP(t, base, taskID)
	if done.CLICommand != cli {
		t.Errorf("GET /v1/tasks/%s: cli_command %q, want the one the answer gave, %q", taskID, done.CLICommand, cli)
	}

	f, gate := newGatedChainFixture(t, 2)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")
	posted := postTask(t, f.base, url.Values{"path": {handle}})
	if !strings.HasPrefix(posted.CLICommand, "rx logs index ") || !strings.Contains(posted.CLICommand, handle) {
		t.Errorf("POST /v1/logs/index answered cli_command %q, want an rx logs index command", posted.CLICommand)
	}
	gate.awaitStart(t)
	if running := taskStatusOnce(t, f.base, posted.TaskID); running.Status != "running" || running.CLICommand != posted.CLICommand {
		t.Errorf("GET /v1/tasks/%s while running: %s, cli_command %q, want running with %q", posted.TaskID, running.Status, running.CLICommand, posted.CLICommand)
	}
	gate.open()
	if task := awaitTaskEnd(t, f.manager, posted.TaskID); task.Status != tasks.StatusCompleted {
		t.Fatalf("task %s: %s", task.Status, task.Error)
	}
	if ended := awaitTaskOverHTTP(t, f.base, posted.TaskID); ended.CLICommand != posted.CLICommand {
		t.Errorf("GET /v1/tasks/%s when done: cli_command %q, want %q", posted.TaskID, ended.CLICommand, posted.CLICommand)
	}
}

// taskStatusOnce reads GET /v1/tasks/{id} once, as it is now.
func taskStatusOnce(t *testing.T, base, taskID string) rxtypes.TaskStatusResponse {
	t.Helper()
	resp, err := http.Get(base + "/v1/tasks/" + taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var status rxtypes.TaskStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return status
}

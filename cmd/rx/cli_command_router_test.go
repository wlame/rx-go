package main

// The commands an rx server actually renders, from real requests through
// its router, are held to two promises: the real command tree accepts
// them, and running them gives the answer the request got.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/webapi"
)

// routerFixture is an rx server running in this process over a fresh
// search root, cache directory and log file.
type routerFixture struct {
	root     string
	logPath  string
	cacheDir string
	baseURL  string
	hookURL  string
}

// startRouterFixture writes a 300-line log whose every line names its
// own number, and serves it from an in-process rx server. The webhook
// target is a local test server, which RX_ALLOW_INTERNAL_HOOKS permits.
func startRouterFixture(t *testing.T) routerFixture {
	t.Helper()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	// The sandbox answers canonical paths; resolving the temp dir's
	// symlinks (/var is /private/var on macOS) makes the expected
	// command strings agree with them.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	logPath := filepath.Join(root, "app.log")
	var content strings.Builder
	for i := 1; i <= 300; i++ {
		level := "INFO"
		if i%7 == 0 {
			level = "ERROR"
		}
		fmt.Fprintf(&content, "LINE %d %s request handled\n", i, level)
	}
	if err := os.WriteFile(logPath, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set search roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)

	server := httptest.NewServer(webapi.NewServer(webapi.Config{
		AppVersion:  "cli-command-test",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RipgrepPath: rgPath,
		TaskManager: tasks.New(tasks.Config{}),
	}))
	t.Cleanup(server.Close)

	return routerFixture{root: root, logPath: logPath, cacheDir: cacheDir, baseURL: server.URL, hookURL: hook.URL}
}

// get sends a GET and decodes the JSON answer, failing on any status
// but 200.
func (f routerFixture) get(t *testing.T, path string, query url.Values) map[string]any {
	t.Helper()
	resp, err := http.Get(f.baseURL + path + "?" + query.Encode())
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return decodeOK(t, resp)
}

// runTask posts body to path, polls the task it starts until it ends,
// and returns the task's result.
func (f routerFixture) runTask(t *testing.T, path string, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(f.baseURL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	taskID, _ := decodeOK(t, resp)["task_id"].(string)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		task := f.get(t, "/v1/tasks/"+taskID, url.Values{})
		switch task["status"] {
		case "completed":
			return task["result"].(map[string]any)
		case "failed":
			t.Fatalf("task %s failed: %v", taskID, task["error"])
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", taskID)
	return nil
}

func decodeOK(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%s %s: status %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return decoded
}

// runRenderedJSON runs a rendered command (already split into words,
// without "rx") with --json against the fixture's cache and decodes
// what it prints.
func (f routerFixture) runRenderedJSON(t *testing.T, words []string) map[string]any {
	t.Helper()
	args := append(append([]string{}, words...), "--json")
	code, stdout, stderr := runRxIn(t, f.root, []string{"RX_CACHE_DIR=" + f.cacheDir}, args...)
	if code != 0 {
		t.Fatalf("rx %v exited %d: %s", args, code, stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("rx %v: decode: %v\n%s", args, err, stdout)
	}
	return decoded
}

// withoutPerRunKeys drops the members that differ between two runs of
// the same request: when it ran, its ID, and the command string itself.
func withoutPerRunKeys(answer map[string]any) map[string]any {
	out := make(map[string]any, len(answer))
	for key, value := range answer {
		switch key {
		case "time", "request_id", "cli_command":
			continue
		}
		out[key] = value
	}
	return out
}

func requireSameAnswer(t *testing.T, httpAnswer, cliAnswer map[string]any) {
	t.Helper()
	left, right := withoutPerRunKeys(httpAnswer), withoutPerRunKeys(cliAnswer)
	if !reflect.DeepEqual(left, right) {
		leftJSON, _ := json.MarshalIndent(left, "", " ")
		rightJSON, _ := json.MarshalIndent(right, "", " ")
		t.Errorf("the rendered command answers differently\nHTTP: %s\nCLI:  %s", leftJSON, rightJSON)
	}
}

func TestCLICommand_RealRequestsRenderCommandsThatDoTheSame(t *testing.T) {
	f := startRouterFixture(t)
	requestID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}

	t.Run("trace", func(t *testing.T) {
		answer := f.get(t, "/v1/trace", url.Values{
			"path":             {f.logPath},
			"regexp":           {"error", `LINE 7\d `},
			"ignore_case":      {"true"},
			"max_results":      {"1000"},
			"request_id":       {requestID.String()},
			"hook_on_complete": {f.hookURL},
		})
		rendered, _ := answer["cli_command"].(string)
		want := "rx trace " + f.logPath + ` --regexp=error --regexp='LINE 7\d ' --ignore-case` +
			" --max-results=1000 --request-id=" + requestID.String() + " --hook-on-complete=" + f.hookURL
		if rendered != want {
			t.Fatalf("cli_command\n got  %q\n want %q", rendered, want)
		}
		requireSameAnswer(t, answer, f.runRenderedJSON(t, requireRunnableCommand(t, rendered)))
	})

	t.Run("samples", func(t *testing.T) {
		answer := f.get(t, "/v1/samples", url.Values{
			"path":           {f.logPath},
			"lines":          {"10,20"},
			"context":        {"2"},
			"before_context": {"0"},
		})
		rendered, _ := answer["cli_command"].(string)
		want := "rx samples " + f.logPath + " --lines=10,20 --context=2 --before=0"
		if rendered != want {
			t.Fatalf("cli_command\n got  %q\n want %q", rendered, want)
		}
		requireSameAnswer(t, answer, f.runRenderedJSON(t, requireRunnableCommand(t, rendered)))
	})

	t.Run("index_post", func(t *testing.T) {
		// A 300-line file is far below RX_LARGE_FILE_MB; threshold 0
		// indexes it anyway, and so must the rendered command.
		result := f.runTask(t, "/v1/index", map[string]any{
			"path": f.logPath, "force": true, "threshold": 0, "analyze_window_lines": 50,
		})
		rendered, _ := result["cli_command"].(string)
		want := "rx index " + f.logPath + " --force --threshold=0 --analyze-window-lines=50"
		if rendered != want {
			t.Fatalf("cli_command\n got  %q\n want %q", rendered, want)
		}
		cli := f.runRenderedJSON(t, requireRunnableCommand(t, rendered))
		if indexed, _ := cli["indexed"].([]any); len(indexed) != 1 {
			t.Errorf("rx index did not index the file: %v", cli)
		}
	})

	t.Run("index_get", func(t *testing.T) {
		answer := f.get(t, "/v1/index", url.Values{"path": {f.logPath}})
		rendered, _ := answer["cli_command"].(string)
		want := "rx index " + f.logPath + " --info --json"
		if rendered != want {
			t.Fatalf("cli_command\n got  %q\n want %q", rendered, want)
		}
		// The CLI prints the stored index; the HTTP answer is a
		// projection of it, with two members renamed.
		words := requireRunnableCommand(t, rendered)
		code, stdout, stderr := runRxIn(t, f.root, []string{"RX_CACHE_DIR=" + f.cacheDir}, words...)
		if code != 0 {
			t.Fatalf("rx %v exited %d: %s", words, code, stderr)
		}
		var stored map[string]any
		if err := json.Unmarshal([]byte(stdout), &stored); err != nil {
			t.Fatalf("decode: %v\n%s", err, stdout)
		}
		renamed := map[string]string{"path": "source_path", "size_bytes": "source_size_bytes"}
		for _, key := range []string{"path", "size_bytes", "file_type", "created_at", "line_index", "analysis_performed", "line_count"} {
			storedKey := key
			if name, ok := renamed[key]; ok {
				storedKey = name
			}
			if !reflect.DeepEqual(answer[key], stored[storedKey]) {
				t.Errorf("%s: HTTP %v, CLI %s %v", key, answer[key], storedKey, stored[storedKey])
			}
		}
	})

	t.Run("compress", func(t *testing.T) {
		output := filepath.Join(f.root, "archive.zst")
		result := f.runTask(t, "/v1/compress", map[string]any{
			"input_path": f.logPath, "output_path": output, "frame_size": "4M",
			"compression_level": 3, "build_index": false, "force": true,
		})
		rendered, _ := result["cli_command"].(string)
		want := "rx compress " + f.logPath + " --output=" + output + " --build-index=false --force"
		if rendered != want {
			t.Fatalf("cli_command\n got  %q\n want %q", rendered, want)
		}
		// The task has written the output, so only --force lets the
		// command run again; without an index, it builds none.
		cli := f.runRenderedJSON(t, requireRunnableCommand(t, rendered))
		files, _ := cli["files"].([]any)
		if len(files) != 1 {
			t.Fatalf("rx compress answered %v", cli)
		}
		file := files[0].(map[string]any)
		if file["success"] != true || file["index"] != nil {
			t.Errorf("rx compress: success=%v index=%v, want true and none", file["success"], file["index"])
		}
	})
}

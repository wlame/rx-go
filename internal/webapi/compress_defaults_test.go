package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// compressDocPath is the user page that documents POST /v1/compress. Its
// curl examples are part of what the endpoint promises.
const compressDocPath = "../../docs/api/endpoints/compress.md"

// compressDocBody matches the JSON body of a `curl -d '...'` example.
var compressDocBody = regexp.MustCompile(`(?s)-d '(\{.*?\})'`)

// compressDocLogDir is the directory the documented examples use; each
// test run substitutes a temporary search root for it.
const compressDocLogDir = "/var/log"

// postCompressRaw POSTs body unchanged to /v1/compress and returns the
// status and the decoded answer. Unlike postCompress it does not go
// through rxtypes.CompressRequest, so a field the caller leaves out is
// really absent from the request.
func postCompressRaw(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/v1/compress", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// getTaskResult fetches a completed task's result object.
func getTaskResult(t *testing.T, url, taskID string) map[string]any {
	t.Helper()
	resp, err := http.Get(url + "/v1/tasks/" + taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var task struct {
		Result map[string]any `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return task.Result
}

// TestCompress_BodyWithOnlyInputPathUsesCLIDefaults asserts that the
// smallest documented request is accepted and compresses the way
// `rx compress PATH` does: output beside the input, 4M frames, level 3,
// and a line index built.
func TestCompress_BodyWithOnlyInputPathUsesCLIDefaults(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	input := filepath.Join(root, "app.log")
	if err := os.WriteFile(input, []byte(strings.Repeat("a log line\n", 500)), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"input_path": input})
	status, created := postCompressRaw(t, ts.URL, string(body))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %v", status, created)
	}
	taskID, _ := created["task_id"].(string)
	waitForTaskCompletion(t, ts.URL, taskID)

	result := getTaskResult(t, ts.URL, taskID)
	if got, want := result["output_path"], input+".zst"; got != want {
		t.Errorf("output_path = %v, want %v", got, want)
	}
	if result["index_built"] != true {
		t.Errorf("index_built = %v, want true (rx compress builds the index by default)", result["index_built"])
	}
	// The defaults the request took are those of rx compress, so the
	// equivalent command names none of them.
	if got, want := result["cli_command"], "rx compress "+input; got != want {
		t.Errorf("cli_command = %v, want %v", got, want)
	}
}

// TestCompress_DocumentedExampleBodiesAreAccepted posts every request
// body shown in the compress page of the user docs. Each one runs
// against its own search root, so one example's output file cannot make
// the next one fail.
func TestCompress_DocumentedExampleBodiesAreAccepted(t *testing.T) {
	doc, err := os.ReadFile(compressDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", compressDocPath, err)
	}
	examples := compressDocBody.FindAllStringSubmatch(string(doc), -1)
	if len(examples) == 0 {
		t.Fatalf("no -d '{...}' example bodies found in %s", compressDocPath)
	}

	for i, match := range examples {
		t.Run("example", func(t *testing.T) {
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "archive"), 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, "audit-2026-03.log"),
				[]byte(strings.Repeat("audit line\n", 200)), 0o600); err != nil {
				t.Fatalf("write input: %v", err)
			}
			if err := paths.SetSearchRoots([]string{root}); err != nil {
				t.Fatalf("set roots: %v", err)
			}
			t.Cleanup(paths.Reset)
			ts := newTestServer(t)

			body := strings.ReplaceAll(match[1], compressDocLogDir, root)
			status, answer := postCompressRaw(t, ts.URL, body)
			if status != http.StatusOK {
				t.Fatalf("example %d: status = %d, want 200; body %s; answer %v", i, status, body, answer)
			}
			taskID, _ := answer["task_id"].(string)
			waitForTaskCompletion(t, ts.URL, taskID)
		})
	}
}

// TestCompress_BuildIndexFalseBuildsNoIndex asserts that an explicit
// "build_index": false is honored. Its default is true, and a default
// filled over every zero value would turn the false into true.
func TestCompress_BuildIndexFalseBuildsNoIndex(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	input := filepath.Join(root, "app.log")
	if err := os.WriteFile(input, []byte(strings.Repeat("a log line\n", 500)), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	status, created := postCompressRaw(t, ts.URL, `{"input_path": "`+input+`", "build_index": false}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %v", status, created)
	}
	taskID, _ := created["task_id"].(string)
	waitForTaskCompletion(t, ts.URL, taskID)

	result := getTaskResult(t, ts.URL, taskID)
	if result["index_built"] != false || result["total_lines"] != nil {
		t.Errorf("index_built = %v, total_lines = %v; want false and null",
			result["index_built"], result["total_lines"])
	}
}

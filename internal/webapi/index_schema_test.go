package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
)

// schemaRef is how the OpenAPI document refers to a named schema.
func schemaRef(name string) string { return "#/components/schemas/" + name }

// TestIndexSchemas_AreNamedInTheDocument asserts that the index answers
// have named schemas a client can generate types from, and that the
// operations refer to them rather than to a free-form object.
func TestIndexSchemas_AreNamedInTheDocument(t *testing.T) {
	doc := NewServer(Config{AppVersion: "index-schema-test"}).API().OpenAPI()
	schemas := doc.Components.Schemas.Map()
	for _, name := range []string{"IndexResponse", "IndexTaskResult", "CompressTaskResult", "LineIndexEntry"} {
		if _, ok := schemas[name]; !ok {
			t.Errorf("components.schemas has no %s", name)
		}
	}

	getIndex := doc.Paths["/v1/index"].Get.Responses["200"].Content["application/json"].Schema
	if getIndex.Ref != schemaRef("IndexResponse") {
		t.Errorf("GET /v1/index 200 schema = %+v, want a $ref to IndexResponse", getIndex)
	}

	result, err := json.Marshal(schemas["TaskStatusResponse"].Properties["result"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{schemaRef("IndexTaskResult"), schemaRef("CompressTaskResult"), `"null"`} {
		if !strings.Contains(string(result), want) {
			t.Errorf("TaskStatusResponse.result schema %s lacks %s", result, want)
		}
	}
}

// TestIndexAnswers_ValidateAgainstTheirSchemas checks real answers
// against the schemas the document declares for them: a compress task,
// which builds a seekable-zstd index whose line_index entries have three
// elements; GET /v1/index on that .zst; an analyze task on the plain
// file, whose entries have two; and GET /v1/index on the plain file.
func TestIndexAnswers_ValidateAgainstTheirSchemas(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	plain := filepath.Join(root, "app.log")
	var content strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&content, "LINE %d 2026-10-03 INFO request handled in %d ms\n", i, i%97)
	}
	if err := os.WriteFile(plain, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	srv := NewServer(Config{AppVersion: "unit-test", TaskManager: tasks.New(tasks.Config{})})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	registry := srv.API().OpenAPI().Components.Schemas

	// 1 MiB frames give the ~1.3 MB file two frames, so the index names
	// more than one frame.
	compress := postJSON(t, ts.URL+"/v1/compress", `{"input_path": "`+plain+`", "frame_size": "1M"}`)
	compressTask := waitForTaskBody(t, ts.URL, compress["task_id"].(string))
	validateBody(t, registry, "TaskStatusResponse", compressTask)
	validateBody(t, registry, "CompressTaskResult", compressTask["result"])

	seekable := getJSON(t, ts.URL+"/v1/index?path="+url.QueryEscape(plain+".zst"))
	validateBody(t, registry, "IndexResponse", seekable)
	if got := entryLengths(seekable); !got[3] || got[2] {
		t.Errorf("seekable-zstd line_index entry lengths = %v, want only 3", got)
	}

	index := postJSON(t, ts.URL+"/v1/index", `{"path": "`+plain+`", "analyze": true}`)
	indexTask := waitForTaskBody(t, ts.URL, index["task_id"].(string))
	validateBody(t, registry, "TaskStatusResponse", indexTask)
	validateBody(t, registry, "IndexTaskResult", indexTask["result"])
	if got := entryLengths(indexTask["result"]); !got[2] || got[3] {
		t.Errorf("plain line_index entry lengths = %v, want only 2", got)
	}

	validateBody(t, registry, "IndexResponse", getJSON(t, ts.URL+"/v1/index?path="+url.QueryEscape(plain)))
}

// validateBody validates a decoded JSON value against the named schema
// of the document, with huma's own validator.
func validateBody(t *testing.T, registry huma.Registry, name string, value any) {
	t.Helper()
	if _, ok := registry.Map()[name]; !ok {
		t.Fatalf("components.schemas has no %s", name)
	}
	result := &huma.ValidateResult{}
	huma.Validate(registry, &huma.Schema{Ref: schemaRef(name)}, huma.NewPathBuffer([]byte{}, 0),
		huma.ModeReadFromServer, value, result)
	for _, problem := range result.Errors {
		t.Errorf("%s: %v", name, problem)
	}
}

// entryLengths reports which entry lengths an index answer's line_index
// holds.
func entryLengths(answer any) map[int]bool {
	out := map[int]bool{}
	body, _ := answer.(map[string]any)
	entries, _ := body["line_index"].([]any)
	for _, entry := range entries {
		elements, _ := entry.([]any)
		out[len(elements)] = true
	}
	return out
}

// postJSON POSTs body and decodes a 200 answer.
func postJSON(t *testing.T, target, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(target, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("post %s: %v", target, err)
	}
	return decodeOK(t, resp)
}

// getJSON GETs target and decodes a 200 answer.
func getJSON(t *testing.T, target string) map[string]any {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("get %s: %v", target, err)
	}
	return decodeOK(t, resp)
}

func decodeOK(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d, body %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// waitForTaskBody polls a task until it completes and returns its last
// answer.
func waitForTaskBody(t *testing.T, baseURL, taskID string) map[string]any {
	t.Helper()
	waitForTaskCompletion(t, baseURL, taskID)
	return getJSON(t, baseURL+"/v1/tasks/"+taskID)
}

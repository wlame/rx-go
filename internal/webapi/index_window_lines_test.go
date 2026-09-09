package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// analyze_window_lines is the sliding-window size the analyzer's
// detectors see. rx-python had no equivalent, so a client that set it
// got the window it asked for from one backend and silently the default
// from the other — a difference in results, not just in the API surface.
//
// It is optional: absent, null and 0 all mean "use the default". A
// negative value is a mistake the caller made rather than a way to spell
// that, so it is refused before a task is created — failing a task the
// client is already polling would report the same mistake later and less
// clearly.

// windowFixture installs a sandbox root and returns it. Callers write
// the files they need inside it.
func windowFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := paths.SetSearchRoots([]string{dir}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return dir
}

func postIndexBody(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/v1/index", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func TestIndexPost_WindowLinesIsOptional(t *testing.T) {
	root := windowFixture(t)
	ts := newServerWithRipgrep(t)

	// Each case gets its own file: two index tasks for one path collide
	// with 409, which is correct behaviour and not what this is testing.
	cases := map[string]string{
		"given":   `,"analyze_window_lines":50`,
		"omitted": ``,
		"null":    `,"analyze_window_lines":null`,
		"zero":    `,"analyze_window_lines":0`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".log")
			if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			body := `{"path":"` + path + `","analyze":true` + field + `}`
			status, decoded := postIndexBody(t, ts.URL, body)
			if status != http.StatusOK {
				t.Errorf("status: got %d, want 200 (body: %v)", status, decoded)
			}
		})
	}
}

func TestIndexPost_NegativeWindowLinesIsRefused(t *testing.T) {
	root := windowFixture(t)
	path := filepath.Join(root, "app.log")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ts := newServerWithRipgrep(t)

	status, decoded := postIndexBody(t, ts.URL,
		`{"path":"`+path+`","analyze":true,"analyze_window_lines":-5}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body: %v)", status, decoded)
	}
	detail, _ := decoded["detail"].(string)
	if detail != "analyze_window_lines must be positive, or 0 to use the default" {
		t.Errorf("detail: got %q", detail)
	}
	// Refused before a task exists, so the client has nothing to poll.
	if _, ok := decoded["task_id"]; ok {
		t.Errorf("a task was created for a refused request: %v", decoded)
	}
}

// The field is optional in the published schema, so a client that omits
// it is not rejected by a generated validator either.
func TestIndexPost_WindowLinesIsOptionalInTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "openapi.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	schema := doc.Components.Schemas["IndexRequest"]
	if _, ok := schema.Properties["analyze_window_lines"]; !ok {
		t.Error("analyze_window_lines is not in the IndexRequest schema")
	}
	for _, name := range schema.Required {
		if name == "analyze_window_lines" {
			t.Error("analyze_window_lines is marked required; it is optional")
		}
	}
}

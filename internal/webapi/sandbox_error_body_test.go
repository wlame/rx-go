package webapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// A path outside the search roots produces one body, whatever route it
// was asked of, and rx-python produces the same one. The shape is the
// SandboxError schema in the OpenAPI document; a route that answers with
// a bare {"detail": ...} instead gives the viewer nothing to build a
// "this path is outside your sandbox" panel from.

// twoRootSandbox installs two roots, deliberately in non-sorted order,
// and returns a path that is inside neither.
func twoRootSandbox(t *testing.T) (outside string, wantRoots []string) {
	t.Helper()
	base := t.TempDir()
	zeta := filepath.Join(base, "zeta")
	alpha := filepath.Join(base, "alpha")
	out := filepath.Join(base, "outside")
	for _, d := range []string{zeta, alpha, out} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := paths.SetSearchRoots([]string{zeta, alpha}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	wantRoots = paths.GetSearchRoots()
	sort.Strings(wantRoots)
	return filepath.Join(out, "f.log"), wantRoots
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return decoded
}

func TestSandboxError_EveryRouteReturnsTheSameBody(t *testing.T) {
	outside, wantRoots := twoRootSandbox(t)
	ts := newServerWithRipgrep(t)

	roots := make([]any, len(wantRoots))
	for i, r := range wantRoots {
		roots[i] = r
	}
	want := map[string]any{
		"detail":  "path_outside_search_root",
		"error":   "path_outside_search_root",
		"message": "path \"" + outside + "\" is not within any configured --search-root",
		"path":    outside,
		"roots":   roots,
	}

	cases := []struct {
		name   string
		method string
		url    string
		body   any
	}{
		{"trace", http.MethodGet, ts.URL + "/v1/trace?path=" + outside + "&regexp=error", nil},
		{"samples", http.MethodGet, ts.URL + "/v1/samples?path=" + outside + "&lines=1", nil},
		{"index-get", http.MethodGet, ts.URL + "/v1/index?path=" + outside, nil},
		{"tree", http.MethodGet, ts.URL + "/v1/tree?path=" + outside, nil},
		{"index-post", http.MethodPost, ts.URL + "/v1/index", map[string]any{"path": outside}},
		{"compress", http.MethodPost, ts.URL + "/v1/compress", map[string]any{
			"input_path":        outside,
			"output_path":       nil,
			"frame_size":        "4K",
			"compression_level": 3,
			"build_index":       false,
			"force":             false,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			var err error
			if tc.body == nil {
				req, err = http.NewRequest(tc.method, tc.url, nil)
			} else {
				encoded, mErr := json.Marshal(tc.body)
				if mErr != nil {
					t.Fatalf("marshal: %v", mErr)
				}
				req, err = http.NewRequest(tc.method, tc.url, bytes.NewReader(encoded))
				req.Header.Set("Content-Type", "application/json")
			}
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status: got %d, want 403", resp.StatusCode)
			}
			got := decodeBody(t, resp)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("body mismatch\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

// The roots are sorted, so a client that compares two backends' bodies
// does not have to care which order the operator wrote the flags in.
func TestSandboxError_RootsAreSorted(t *testing.T) {
	outside, wantRoots := twoRootSandbox(t)
	ts := newServerWithRipgrep(t)

	resp, err := http.Get(ts.URL + "/v1/tree?path=" + outside)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := decodeBody(t, resp)

	rawRoots, ok := got["roots"].([]any)
	if !ok {
		t.Fatalf("roots is not a list: %#v", got["roots"])
	}
	for i, want := range wantRoots {
		if rawRoots[i] != want {
			t.Errorf("roots[%d]: got %v, want %s", i, rawRoots[i], want)
		}
	}
}

// The shape is published, so a client can generate a type for it.
func TestSandboxError_IsInTheOpenAPIDocument(t *testing.T) {
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
		Paths map[string]map[string]struct {
			Responses map[string]any `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	schema, ok := doc.Components.Schemas["SandboxError"]
	if !ok {
		t.Fatalf("SandboxError is not in components.schemas")
	}
	for _, field := range []string{"detail", "error", "message", "path", "roots"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Errorf("SandboxError schema has no %q property", field)
		}
	}

	// Only the routes that take a filesystem path can refuse one.
	pathAccepting := map[string]bool{
		"/v1/trace": true, "/v1/samples": true, "/v1/index": true,
		"/v1/tree": true, "/v1/compress": true,
	}
	for path, ops := range doc.Paths {
		if !pathAccepting[path] {
			continue
		}
		for method, op := range ops {
			if _, ok := op.Responses["403"]; !ok {
				t.Errorf("%s %s declares no 403 response", method, path)
			}
		}
	}
}

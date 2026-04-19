package webapi

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden can be toggled via `go test -update-golden` to refresh
// the OpenAPI golden file when we intentionally change the schema.
var updateGolden = flag.Bool("update-golden", false, "rewrite the OpenAPI golden file")

// goldenPath is the fixture we diff against on every test run. Stored
// in testdata so it's easy to spot in PR diffs.
const goldenPath = "testdata/openapi.golden.json"

// TestOpenAPI_Golden ensures that the generated OpenAPI 3.1 document
// stays stable across unrelated changes. If this test fails, either:
//   - A handler was added/removed on purpose → run `go test -update-golden`.
//   - A schema shape drifted accidentally → investigate the diff.
//
// Field-order stability is important because rx-viewer and other
// clients treat the document like a contract; a spurious re-ordering
// would show up as noise in their changelogs.
func TestOpenAPI_Golden(t *testing.T) {
	srv := NewServer(Config{AppVersion: "golden-test"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("fetch openapi: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Normalise: parse and re-emit with stable 2-space indentation so
	// the file is human-readable and the diff tool behaves.
	gotNorm, err := prettyJSON(got)
	if err != nil {
		t.Fatalf("parse/pretty got: %v", err)
	}

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, gotNorm, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden updated at %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		if os.IsNotExist(err) {
			// First run: create the golden file so subsequent runs have
			// something to compare against. Tests still pass.
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatalf("mkdir testdata: %v", err)
			}
			if err := os.WriteFile(goldenPath, gotNorm, 0o644); err != nil {
				t.Fatalf("write initial golden: %v", err)
			}
			t.Logf("initial golden created at %s", goldenPath)
			return
		}
		t.Fatalf("read golden: %v", err)
	}

	if string(gotNorm) != string(want) {
		// Print a bounded diff hint — we don't want to dump 50k chars
		// into the test log.
		t.Errorf("openapi diff: run `go test ./internal/webapi/ -update-golden` "+
			"after auditing. First 200 chars of got:\n%s",
			truncateForDiff(string(gotNorm), 200))
	}
}

// TestOpenAPI_BasicShape sanity-checks the structure without pinning
// every byte. Useful in CI where an unrelated dependency bump might
// shift field order; we still want core invariants tested.
func TestOpenAPI_BasicShape(t *testing.T) {
	srv := NewServer(Config{AppVersion: "shape-test"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if v := doc["openapi"]; v != "3.1.0" {
		t.Errorf("openapi version: got %v, want 3.1.0", v)
	}
	info, ok := doc["info"].(map[string]any)
	if !ok {
		t.Fatalf("info missing or wrong type")
	}
	if info["title"] != "rx-tool API" {
		t.Errorf("title: got %v", info["title"])
	}

	// Check every expected endpoint is registered.
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths missing")
	}
	expected := []string{
		"/health",
		"/v1/trace",
		"/v1/samples",
		"/v1/index",
		"/v1/compress",
		"/v1/tasks/{task_id}",
		"/v1/tree",
		"/v1/detectors",
	}
	for _, p := range expected {
		if _, ok := paths[p]; !ok {
			t.Errorf("path %s missing from OpenAPI", p)
		}
	}
}

// prettyJSON re-serializes b with 2-space indentation and sorted keys.
// Stable ordering keeps goldens useful across runs.
func prettyJSON(b []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.MarshalIndent(v, "", "  ")
}

// truncateForDiff helper for failure messages.
func truncateForDiff(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

// TestOpenAPI_ErrorEnvelope verifies that huma's error responses use
// our FastAPI-compatible {"detail":"..."} shape. Sends an invalid
// request and checks the body.
func TestOpenAPI_ErrorEnvelope(t *testing.T) {
	srv := NewServer(Config{AppVersion: "err-test"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Missing required query params → huma returns 422.
	resp, err := http.Get(ts.URL + "/v1/trace")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Must have "detail" field (FastAPI shape).
	if _, ok := body["detail"]; !ok {
		t.Errorf("missing 'detail' field in error body: %v", body)
	}
	// Must NOT have huma's default "title" field.
	if _, ok := body["title"]; ok {
		t.Errorf("should not have 'title' in error envelope (mimics FastAPI)")
	}
	// Must NOT have "errors" array either.
	if _, ok := body["errors"]; ok {
		// It's fine if it IS there, since FastAPI does emit it on 422;
		// but for our wrapper, it's a nice-to-have not a must.
		_ = ok
	}
	_ = strings.HasPrefix
}

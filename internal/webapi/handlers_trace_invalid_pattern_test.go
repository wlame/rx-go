package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// TestTrace_InvalidPatternReturns400 asserts a pattern ripgrep cannot
// compile is reported as a client error carrying rg's own message,
// rather than a 200 with an empty result and the file listed as
// skipped — which reads to a caller as "searched, found nothing".
func TestTrace_InvalidPatternReturns400(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.log")
	if err := os.WriteFile(file, []byte("error happened\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newServerWithRipgrep(t)

	for _, pattern := range []string{"a(", "[", "(?P<x"} {
		t.Run(pattern, func(t *testing.T) {
			query := url.Values{"path": {file}, "regexp": {pattern}}
			resp, err := http.Get(ts.URL + "/v1/trace?" + query.Encode())
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400 (body: %v)", resp.StatusCode, body)
			}
			detail, _ := body["detail"].(string)
			if detail == "" {
				t.Errorf("no detail in body: %v", body)
			}
		})
	}
}

// TestTrace_ValidPatternStillSucceeds is the positive control.
func TestTrace_ValidPatternStillSucceeds(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.log")
	if err := os.WriteFile(file, []byte("error happened\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newServerWithRipgrep(t)
	query := url.Values{"path": {file}, "regexp": {"error"}}
	resp, err := http.Get(ts.URL + "/v1/trace?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	matches, _ := body["matches"].([]any)
	if len(matches) != 1 {
		t.Errorf("matches: got %d, want 1 (body: %v)", len(matches), body)
	}
}

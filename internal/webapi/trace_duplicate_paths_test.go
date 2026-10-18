package webapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// GET /v1/trace with one path twice searches the file once, under one
// id, and reports each match once; cli_command still renders the
// request as it was given.
func TestTraceEndpoint_APathGivenTwiceIsSearchedOnce(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	if err := os.WriteFile(f, []byte("LINE 1 NEEDLE\nLINE 2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	q := url.Values{"path": {f, f}, "regexp": {"NEEDLE"}}
	resp, err := http.Get(fmt.Sprintf("%s/v1/trace?%s", ts.URL, q.Encode()))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.TraceResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Files) != 1 || out.Files["f1"] != f {
		t.Errorf("files = %v, want {f1: %s}", out.Files, f)
	}
	if len(out.Matches) != 1 || out.Matches[0].File != "f1" {
		t.Errorf("matches = %v, want one match in f1", out.Matches)
	}
	if out.CLICommand == nil || strings.Count(*out.CLICommand, f) != 2 {
		t.Errorf("cli_command = %v, want the path twice, as given", out.CLICommand)
	}
}

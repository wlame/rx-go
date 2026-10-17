package webapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// lines=-3--1 answers the last three lines under the range they name;
// a reversed negative range is a 400 whose detail names a line range.
func TestSamples_NegativeLineRange(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	if err := os.WriteFile(f, []byte("A\nB\nC\nD\nE\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	get := func(lines string) (int, []byte) {
		resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&lines=%s&context=0", ts.URL, f, lines))
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	status, body := get("-3--1")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := out.Samples["3-5"]; len(got) != 3 || got[0] != "C" || got[2] != "E" {
		t.Errorf("samples[3-5] = %v, want [C D E]: %s", got, body)
	}
	if out.Lines["3-5"] != -1 {
		t.Errorf("lines[3-5] = %d, want -1 for a range", out.Lines["3-5"])
	}

	status, body = get("-1--3")
	if status != http.StatusBadRequest || !strings.Contains(string(body), "invalid line range: -1--3") {
		t.Errorf("reversed range: status %d body %s, want 400 naming a line range", status, body)
	}
}

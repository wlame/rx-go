package clicommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// `rx trace` over twelve files given in order lists their matches in
// that order, app2.log before app10.log, in --json and in the human
// output; the human context section lists the files in the same order.
func TestTraceCommand_ListsTwelveFilesInTheOrderGiven(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	var paths []string
	for k := 1; k <= 12; k++ {
		p := filepath.Join(dir, fmt.Sprintf("app%d.log", k))
		body := fmt.Sprintf("LINE 1 file=%d\nLINE 2 file=%d error\nLINE 3 file=%d\n", k, k, k)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	resp := runTraceJSON(t, append([]string{"error"}, paths...))
	var jsonOrder []string
	for _, m := range resp.Matches {
		jsonOrder = append(jsonOrder, resp.Files[m.File])
	}
	if !slices.Equal(jsonOrder, paths) {
		t.Errorf("--json matches in file order:\n%v\nwant:\n%v", jsonOrder, paths)
	}

	human := runTraceHuman(t, append([]string{"error", "--context=1", "--no-color"}, paths...))
	matchSection, contextSection, found := strings.Cut(human, "\nContext (1 before, 1 after):\n")
	if !found {
		t.Fatalf("no context section in:\n%s", human)
	}
	if got := pathsInOrder(matchSection, paths, ":2:"); !slices.Equal(got, paths) {
		t.Errorf("match list in file order:\n%v\nwant:\n%v", got, paths)
	}
	if got := pathsInOrder(contextSection, paths, ""); !slices.Equal(got, paths) {
		t.Errorf("context section in file order:\n%v\nwant:\n%v", got, paths)
	}
}

// runTraceJSON runs `rx trace ARGS --json` and parses the answer.
func runTraceJSON(t *testing.T, args []string) rxtypes.TraceResponse {
	t.Helper()
	out := runTraceHuman(t, append(args, "--json"))
	var resp rxtypes.TraceResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("parse --json: %v\n%s", err, out)
	}
	return resp
}

// runTraceHuman runs `rx trace ARGS` and returns what it printed.
func runTraceHuman(t *testing.T, args []string) string {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trace %v: %v", args, err)
	}
	return buf.String()
}

// pathsInOrder returns, in the order text prints them, the paths that
// begin a line of text (after its indent) and are followed right away
// by suffix, or end the line when suffix is empty.
func pathsInOrder(text string, paths []string, suffix string) []string {
	var got []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for _, p := range paths {
			if (suffix == "" && line == p) || (suffix != "" && strings.HasPrefix(line, p+suffix)) {
				got = append(got, p)
			}
		}
	}
	return got
}

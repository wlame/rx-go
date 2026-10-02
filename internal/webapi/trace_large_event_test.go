package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A line whose ripgrep event is larger than 16 MiB is answered over
// HTTP like any other, with every match after it, and the request
// returns: it neither hangs nor holds an rg process. The response
// arrives only after the engine has waited on every rg it started.
//
// The client's timeout is what keeps this test from hanging when the
// request would: it gives up, the server's context ends, and rg is
// killed.
func TestTrace_LineWhoseRipgrepEventExceeds16MiBIsAnswered(t *testing.T) {
	const lineCount = 3000
	var text strings.Builder
	text.WriteString("LINE 1 NEEDLE " + strings.Repeat("x", 17_000_000) + "\n")
	for i := 2; i <= lineCount; i++ {
		text.WriteString("LINE " + strconv.Itoa(i) + " NEEDLE short\n")
	}
	root := t.TempDir()
	file := filepath.Join(root, "long-line.log")
	if err := os.WriteFile(file, []byte(text.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newServerWithRipgrep(t)

	client := &http.Client{Timeout: 60 * time.Second}
	query := url.Values{"path": {file}, "regexp": {"NEEDLE"}, "no_cache": {"true"}}
	resp, err := client.Get(ts.URL + "/v1/trace?" + query.Encode())
	if err != nil {
		t.Fatalf("the request did not return: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var body rxtypes.TraceResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.SkippedFiles) != 0 {
		t.Errorf("skipped_files = %v, want none", body.SkippedFiles)
	}
	if len(body.Matches) != lineCount {
		t.Fatalf("got %d matches, want %d", len(body.Matches), lineCount)
	}
	if first := body.Matches[0]; first.AbsoluteLineNumber != 1 {
		t.Errorf("first match on line %d, want 1", first.AbsoluteLineNumber)
	}
}

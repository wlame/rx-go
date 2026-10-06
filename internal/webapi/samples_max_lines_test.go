package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// A GET /v1/samples answer may hold at most RX_SAMPLES_MAX_LINES lines:
// one that would hold more is refused with 400 naming the setting and
// the count reached, and one within it is answered.
func TestSamples_AnswerOverTheLineLimitIsRefused(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_NO_INDEX", "true")
	t.Setenv("RX_SAMPLES_MAX_LINES", "1000")
	root := t.TempDir()
	var text strings.Builder
	for n := 1; n <= 3000; n++ {
		fmt.Fprintf(&text, "LINE %d payload\n", n)
	}
	file := filepath.Join(root, "big.log")
	if err := os.WriteFile(file, []byte(text.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	validated, err := paths.ValidatePathWithinRoots(file)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	ts := newTestServer(t)

	status, body := getSamples(t, ts.URL, url.Values{"path": {validated}, "lines": {"1-1000"}})
	if status != http.StatusOK {
		t.Fatalf("a range of the limit: status %d, body %.200s", status, body)
	}
	for _, query := range []url.Values{
		{"path": {validated}, "lines": {"1-1001"}},
		{"path": {validated}, "lines": {"1-600,2-600"}},
		{"path": {validated}, "offsets": {"0-99999"}},
	} {
		status, body := getSamples(t, ts.URL, query)
		var problem struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &problem)
		if status != http.StatusBadRequest || !strings.Contains(problem.Detail, "RX_SAMPLES_MAX_LINES") ||
			!strings.Contains(problem.Detail, "1001") {
			t.Errorf("%s: status %d, body %.200s; want 400 naming RX_SAMPLES_MAX_LINES and 1001", query.Encode(), status, body)
		}
	}
}

package webapi

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
)

// GET /v1/index and the result of an index task carry time_summary:
// the time section of the index without its per-checkpoint array, or
// null for a file with no timestamp format. The key is always there.
func TestIndexAnswers_CarryTheTimeSummary(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	first := time.Date(2025, 12, 10, 7, 0, 4, 574e6, time.UTC)
	var timed strings.Builder
	for i := 0; i < 400; i++ {
		at := first.Add(time.Duration(i) * time.Second)
		if i == 200 {
			at = at.Add(-3 * time.Second) // one line steps back 3 s
		}
		fmt.Fprintf(&timed, "%s INFO request %d served\n", at.Format("2006-01-02 15:04:05.000"), i)
	}
	files := map[string]string{
		"timed.log": timed.String(),
		"plain.log": strings.Repeat("no timestamp on this line\n", 400),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	srv := NewServer(Config{AppVersion: "unit-test", TaskManager: tasks.New(tasks.Config{})})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	registry := srv.API().OpenAPI().Components.Schemas

	wantTimed := map[string]any{
		"format":            "iso",
		"has_zone":          false,
		"first_ms":          float64(first.UnixMilli()),
		"last_ms":           float64(first.Add(399 * time.Second).UnixMilli()),
		"timestamped_lines": float64(400),
		"backward_steps":    float64(1),
		"max_backward_ms":   float64(2000),
	}
	for name, want := range map[string]any{"timed.log": wantTimed, "plain.log": nil} {
		path := filepath.Join(root, name)
		task := postJSON(t, ts.URL+"/v1/index", `{"path": "`+path+`", "threshold": 0}`)
		result := waitForTaskBody(t, ts.URL, task["task_id"].(string))["result"].(map[string]any)
		validateBody(t, registry, "IndexTaskResult", result)
		cached := getJSON(t, ts.URL+"/v1/index?path="+url.QueryEscape(path))
		validateBody(t, registry, "IndexResponse", cached)
		for label, answer := range map[string]map[string]any{"task result": result, "GET /v1/index": cached} {
			got, present := answer["time_summary"]
			if !present {
				t.Fatalf("%s %s: no time_summary key", name, label)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: time_summary = %v; want %v", name, label, got, want)
			}
		}
	}
}

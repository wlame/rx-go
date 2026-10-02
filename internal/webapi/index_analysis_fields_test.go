package webapi

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// removedIndexFields are the analysis fields contract 2.0 removed from
// the index answers.
var removedIndexFields = []string{
	"analysis_performed",
	"analysis_window_lines",
	"analysis_detector_set",
	"anomalies",
	"anomaly_count",
	"anomaly_summary",
}

// indexFixture writes a small log under a fresh search root and returns
// its path.
func indexFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "app.log")
	text := "LINE 1 started\nTraceback (most recent call last):\n" +
		"  File \"/app/x.py\", line 5, in foo\nValueError: nope\nLINE 5 done\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return path
}

// The result of a POST /v1/index task and GET /v1/index carry none of
// the analysis fields.
func TestIndexAnswers_CarryNoAnalysisField(t *testing.T) {
	path := indexFixture(t)
	ts := newTestServer(t)

	created := postJSON(t, ts.URL+"/v1/index", `{"path": "`+path+`", "threshold": 0}`)
	task := waitForTaskBody(t, ts.URL, created["task_id"].(string))
	result, _ := task["result"].(map[string]any)
	if result == nil {
		t.Fatalf("task has no result: %v", task)
	}
	got := getJSON(t, ts.URL+"/v1/index?path="+url.QueryEscape(path))

	for _, field := range removedIndexFields {
		if _, ok := result[field]; ok {
			t.Errorf("the POST /v1/index task result carries %s", field)
		}
		if _, ok := got[field]; ok {
			t.Errorf("GET /v1/index carries %s", field)
		}
	}
}

// POST /v1/index answers the removed request fields as it answers any
// property its schema does not declare: 422, naming the property.
func TestIndexPost_AnalysisFieldsAreUnknownProperties(t *testing.T) {
	path := indexFixture(t)
	ts := newTestServer(t)

	cases := []struct{ field, body string }{
		{"analyze", `{"path": "` + path + `", "analyze": true}`},
		{"analyze", `{"path": "` + path + `", "analyze": false}`},
		{"analyze_window_lines", `{"path": "` + path + `", "analyze_window_lines": 64}`},
		{"undeclared", `{"path": "` + path + `", "undeclared": 1}`},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			answer, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", resp.StatusCode, answer)
			}
			if want := "unexpected property (body." + tc.field + ":"; !strings.Contains(string(answer), want) {
				t.Errorf("answer %s does not name the property (want %q)", answer, want)
			}
		})
	}
}

// GET /v1/detectors answers as a route the server does not have.
func TestDetectorsRoute_AnswersAsAnUnknownRoute(t *testing.T) {
	ts := newTestServer(t)

	wantStatus, wantType, wantBody := getRaw(t, ts.URL+"/v1/no-such-route")
	status, contentType, body := getRaw(t, ts.URL+"/v1/detectors")

	if status != http.StatusNotFound || status != wantStatus {
		t.Errorf("status %d, want %d as for an unknown route", status, wantStatus)
	}
	if contentType != wantType || body != wantBody {
		t.Errorf("answer %q (%s), want %q (%s) as for an unknown route", body, contentType, wantBody, wantType)
	}
}

// getRaw GETs target and returns the status, the content type and the
// body as text.
func getRaw(t *testing.T, target string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("get %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

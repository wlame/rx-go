package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
)

// timedRoot writes, into a fresh search root, a one-day log with
// Python-logging timestamps, a log that spans midnight and a log
// without timestamps, and returns their validated paths by name.
func timedRoot(t *testing.T) map[string]string {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_NO_INDEX", "")
	root := t.TempDir()
	files := map[string]string{
		"app.log": "2025-12-10 12:34:55,000 INFO LINE 1\n2025-12-10 12:34:56,123 ERROR LINE 2\n" +
			"Traceback LINE 3\n2025-12-10 12:34:57,000 INFO LINE 4\n2025-12-10 12:34:58,000 INFO LINE 5\n",
		"midnight.log": "2025-12-10 23:59:59,000 LINE 1\n2025-12-11 00:00:01,000 LINE 2\n" +
			"2025-12-11 00:00:02,000 LINE 3\n",
		"plain.log": "alpha\nbeta\ngamma\n",
	}
	out := map[string]string{}
	for name, text := range files {
		file := filepath.Join(root, name)
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		out[name] = file
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	for name, file := range out {
		validated, err := paths.ValidatePathWithinRoots(file)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		out[name] = validated
	}
	return out
}

// getSamples sends GET /v1/samples with query and returns the status
// and the body.
func getSamples(t testing.TB, base string, query url.Values) (int, []byte) {
	t.Helper()
	resp, err := http.Get(base + "/v1/samples?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// The timestamps parameter repeats, keeps a comma inside its value,
// answers the same before and after an index build, and renders as one
// --timestamps flag per value in cli_command.
func TestSamples_TimestampsOverHTTP(t *testing.T) {
	files := timedRoot(t)
	ts := newTestServer(t)
	values := []string{"2025-12-10 12:34:56,123", "12:34:56..12:34:57"}
	query := url.Values{"path": {files["app.log"]}, "timestamps": values, "context": {"1"}}

	got := samplesanswer.ColdAndIndexed(t, files["app.log"], 0, func(t testing.TB) any {
		status, body := getSamples(t, ts.URL, query)
		if status != http.StatusOK {
			t.Fatalf("status %d: %s", status, body)
		}
		return json.RawMessage(body)
	})
	var answer struct {
		Timestamps map[string]int64    `json:"timestamps"`
		Samples    map[string][]string `json:"samples"`
		CLICommand string              `json:"cli_command"`
		TimeFormat map[string]any      `json:"time_format"`
	}
	if err := json.Unmarshal(got.(json.RawMessage), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer.Timestamps[values[0]] != 2 || answer.Timestamps[values[1]] != 2 {
		t.Errorf("timestamps %v, want both at line 2", answer.Timestamps)
	}
	if s := answer.Samples[values[1]]; len(s) != 3 || !strings.HasSuffix(s[2], "LINE 4") {
		t.Errorf("range sample %q, want lines 2 to 4", s)
	}
	wantCLI := "rx samples " + files["app.log"] + " --timestamps='2025-12-10 12:34:56,123' " +
		"--timestamps=12:34:56..12:34:57 --context=1"
	if answer.CLICommand != wantCLI {
		t.Errorf("cli_command %q, want %q", answer.CLICommand, wantCLI)
	}
	if answer.TimeFormat["format"] != "iso" {
		t.Errorf("time_format %v", answer.TimeFormat)
	}
}

// A time query the server cannot answer is the request's fault: 400,
// with a message that says why.
func TestSamples_TimestampsRefusedAsBadRequest(t *testing.T) {
	files := timedRoot(t)
	ts := newTestServer(t)
	cases := []struct {
		name  string
		query url.Values
		says  string
	}{
		{"with lines", url.Values{"path": {files["app.log"]}, "timestamps": {"12:34:56"}, "lines": {"1"}}, "Cannot use 'timestamps'"},
		{"with offsets", url.Values{"path": {files["app.log"]}, "timestamps": {"12:34:56"}, "offsets": {"0"}}, "Cannot use 'timestamps'"},
		{"not a time", url.Values{"path": {files["app.log"]}, "timestamps": {"soon"}}, "soon"},
		{"no timestamps in the file", url.Values{"path": {files["plain.log"]}, "timestamps": {"12:34:56"}}, "no timestamp format recognized"},
		{"a time of day on two dates", url.Values{"path": {files["midnight.log"]}, "timestamps": {"00:00:01"}}, "2025-12-11"},
		{"no address at all", url.Values{"path": {files["app.log"]}}, "'timestamps'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := getSamples(t, ts.URL, tc.query)
			if status != http.StatusBadRequest || !strings.Contains(string(body), tc.says) {
				t.Errorf("status %d, body %s; want 400 saying %s", status, body, tc.says)
			}
		})
	}
}

package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
)

// zonedFileInRoot writes, into the search root timedRoot set, a log
// whose lines write +02:00, and returns its validated path.
func zonedFileInRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(files["app.log"]), "zoned.log")
	text := "2025-12-10T07:00:00.000+02:00 INFO LINE 1\n2025-12-10T07:30:00.000+02:00 INFO LINE 2\n" +
		"2025-12-10T08:00:00.000+02:00 INFO LINE 3\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	validated, err := paths.ValidatePathWithinRoots(path)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return validated
}

// file_tz reads a zoned file's written wall clocks in the zone: the
// same answer before and after an index build, assumed_zone the zone,
// and --file-tz in cli_command.
func TestSamples_FileTZOverHTTP(t *testing.T) {
	files := timedRoot(t)
	path := zonedFileInRoot(t, files)
	ts := newTestServer(t)
	query := url.Values{"path": {path}, "timestamps": {"2025-12-10T07:30:00Z"}, "file_tz": {"UTC"}, "context": {"0"}}
	got := samplesanswer.ColdAndIndexed(t, path, 0, func(t testing.TB) any {
		status, body := getSamples(t, ts.URL, query)
		if status != http.StatusOK {
			t.Fatalf("status %d: %s", status, body)
		}
		return json.RawMessage(body)
	})
	var answer struct {
		Timestamps     map[string]int64    `json:"timestamps"`
		LineTimestamps map[string][]*int64 `json:"line_timestamps"`
		TimeFormat     map[string]any      `json:"time_format"`
		CLICommand     string              `json:"cli_command"`
	}
	if err := json.Unmarshal(got.(json.RawMessage), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stamps := answer.LineTimestamps["2025-12-10T07:30:00Z"]
	if answer.Timestamps["2025-12-10T07:30:00Z"] != 2 || len(stamps) != 1 || stamps[0] == nil || *stamps[0] != 1765351800000 {
		t.Errorf("timestamps %v, line_timestamps %v; want line 2 at 07:30:00 UTC", answer.Timestamps, answer.LineTimestamps)
	}
	if answer.TimeFormat["assumed_zone"] != "UTC" || answer.TimeFormat["has_zone"] != true {
		t.Errorf("time_format %v", answer.TimeFormat)
	}
	wantCLI := "rx samples " + path + " --timestamps=2025-12-10T07:30:00Z --file-tz=UTC --context=0"
	if answer.CLICommand != wantCLI {
		t.Errorf("cli_command %q, want %q", answer.CLICommand, wantCLI)
	}
}

// file_tz on GET /v1/time-range: display_zone is the zone, the instants
// are read in it, and cli_command carries --file-tz.
func TestTimeRange_FileTZOverHTTP(t *testing.T) {
	base, files, _ := timeRangeServer(t)
	query := url.Values{"path": {files["app.log"]}, "file_tz": {"Asia/Tokyo"}}
	resp, err := http.Get(base + "/v1/time-range?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&answer) != nil {
		t.Fatalf("status %d", resp.StatusCode)
	}
	const nine = 9 * 3600 * 1000
	if answer["display_zone"] != "Asia/Tokyo" || answer["first_ms"] != float64(1765350004574-nine) ||
		answer["last_ms"] != float64(1765353599390-nine) ||
		answer["cli_command"] != "rx time-range "+files["app.log"]+" --file-tz=Asia/Tokyo" {
		t.Fatalf("answer %v", answer)
	}
}

// A file_tz that names no zone is the request's fault on both routes:
// 400, naming the value and what is accepted.
func TestFileTZ_InvalidZoneIsABadRequest(t *testing.T) {
	files := timedRoot(t)
	ts := newTestServer(t)
	for _, zone := range []string{"Mars/Base", "+25:00", "+-1:00", "local"} {
		for _, route := range []string{"/v1/samples?lines=1&", "/v1/time-range?"} {
			query := url.Values{"path": {files["app.log"]}, "file_tz": {zone}}
			resp, err := http.Get(ts.URL + route + query.Encode())
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			var problem struct {
				Detail string `json:"detail"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&problem)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(problem.Detail, "file_tz") ||
				!strings.Contains(problem.Detail, zone) || !strings.Contains(problem.Detail, "IANA") {
				t.Errorf("%s file_tz=%s: status %d, detail %q; want 400 naming the value", route, zone, resp.StatusCode, problem.Detail)
			}
		}
	}
}

// /health lists file_tz, and the contract is 1.6.
func TestHealth_ListsFileTZ(t *testing.T) {
	if !slices.Contains(Features(), "file_tz") {
		t.Errorf("features %v lack file_tz", Features())
	}
	if ContractVersion != "1.6" {
		t.Errorf("contract %s, want 1.6", ContractVersion)
	}
}

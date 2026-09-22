package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/clicommand"
)

// utcLogs reads zone-less timestamps as UTC whatever the developer's
// environment says, since the tests name the expected instants in UTC.
var utcLogs = []string{"RX_LOG_TZ="}

// timeRangeFixture writes a timestamped log, its gzip copy, a file
// without timestamps and a binary file into a fresh directory.
func timeRangeFixture(t *testing.T) (dir, timed, gz, plain, binary string) {
	t.Helper()
	dir = t.TempDir()
	text := []byte("starting\n2025-12-10 07:00:04.574 INFO LINE 2\n2025-12-10 07:00:05.000 INFO LINE 3\n" +
		"2025-12-10 07:59:59.390 INFO LINE 4\n\tat frame LINE 5\n")
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write(text)
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	files := map[string][]byte{
		"app.log":    text,
		"app.log.gz": compressed.Bytes(),
		"notes.txt":  []byte("LINE 1 alpha\nLINE 2 beta\n"),
		"blob.bin":   {0x00, 0x01, 0x02, 0x00, 'x', '\n'},
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir, filepath.Join(dir, "app.log"), filepath.Join(dir, "app.log.gz"),
		filepath.Join(dir, "notes.txt"), filepath.Join(dir, "blob.bin")
}

// One line per file: the path, the format, the first and last
// timestamp as the file writes them, the zone and the source.
func TestTimeRange_HumanOutputIsOneLinePerFile(t *testing.T) {
	dir, timed, gz, plain, _ := timeRangeFixture(t)
	code, stdout, stderr := runRxIn(t, dir, utcLogs, "time-range", timed, gz, plain)
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := []string{
		timed + "  iso  2025-12-10 07:00:04.574 .. 2025-12-10 07:59:59.390  UTC  scan",
		gz + "  iso  2025-12-10 07:00:04.574 .. 2025-12-10 07:59:59.390  UTC  index",
		plain + "  no timestamps  scan",
	}
	if got := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stdout\n%s\nwant\n%s", stdout, strings.Join(want, "\n"))
	}
}

// --json gives one object for one path and an array for several; a
// gzip file gets its index built first, unless RX_NO_INDEX is set.
func TestTimeRange_JSONShapes(t *testing.T) {
	dir, timed, gz, _, _ := timeRangeFixture(t)

	code, stdout, stderr := runRxIn(t, dir, utcLogs, "time-range", timed, "--json")
	var one map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &one) != nil {
		t.Fatalf("one path: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	if one["first_ms"] != float64(1765350004574) || one["last_ms"] != float64(1765353599390) ||
		one["example"] != "2025-12-10 07:00:04.574" || one["display_zone"] != "UTC" ||
		one["cli_command"] != "rx time-range "+timed {
		t.Fatalf("answer %v", one)
	}

	code, stdout, stderr = runRxIn(t, dir, utcLogs, "time-range", timed, gz, "--json")
	var several []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &several) != nil || len(several) != 2 {
		t.Fatalf("two paths: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	if several[1]["source"] != "index" || several[1]["last_ms"] != one["last_ms"] {
		t.Fatalf("gzip answer %v; want it from the index it built", several[1])
	}

	code, stdout, _ = runRxIn(t, dir, append([]string{"RX_NO_INDEX=true"}, utcLogs...), "time-range", gz, "--json")
	if code != 0 || json.Unmarshal([]byte(stdout), &one) != nil || one["source"] != "none" || one["last_ms"] != nil {
		t.Fatalf("gzip under RX_NO_INDEX: exit %d, %v", code, one)
	}
}

// Exit codes as `rx samples` gives them, and for several paths the
// code of the failures while the other paths are still answered.
func TestTimeRange_ExitCodes(t *testing.T) {
	dir, timed, _, _, binary := timeRangeFixture(t)
	root, inside, _, outsideFile := sandboxFixture(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"a timestamped file", []string{"time-range", timed}, clicommand.ExitSuccess},
		{"a missing file", []string{"time-range", filepath.Join(dir, "nope.log")}, clicommand.ExitFileNotFound},
		{"a directory", []string{"time-range", dir}, clicommand.ExitUsageError},
		{"a binary file", []string{"time-range", binary}, clicommand.ExitUsageError},
		{"no path", []string{"time-range"}, clicommand.ExitUsageError},
		{"outside the search root", []string{"time-range", outsideFile, "--search-root=" + root}, clicommand.ExitAccessDenied},
		{"inside the search root", []string{"time-range", inside, "--search-root=" + root}, clicommand.ExitSuccess},
		{"a missing file beside a good one", []string{"time-range", timed, filepath.Join(dir, "nope.log")}, clicommand.ExitFileNotFound},
		{"two different failures", []string{"time-range", binary, filepath.Join(dir, "nope.log")}, clicommand.ExitGenericError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runRxIn(t, dir, utcLogs, tc.args...)
			if code != tc.want {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
		})
	}

	code, stdout, _ := runRxIn(t, dir, utcLogs, "time-range", timed, filepath.Join(dir, "nope.log"), "--json")
	var answers []map[string]any
	if code != clicommand.ExitFileNotFound || json.Unmarshal([]byte(stdout), &answers) != nil || len(answers) != 1 {
		t.Fatalf("exit %d, stdout %q; want 3 and the good file's answer", code, stdout)
	}
}

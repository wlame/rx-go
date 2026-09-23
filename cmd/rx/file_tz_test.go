package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/clicommand"
)

// zonedLogFixture writes a log whose lines write +02:00 into a fresh
// directory and returns the directory and the file.
func zonedLogFixture(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "zoned.log")
	text := "2025-12-10T07:00:04.574+02:00 INFO LINE 1\n2025-12-10T07:30:00.000+02:00 INFO LINE 2\n" +
		"2025-12-10T07:59:59.390+02:00 INFO LINE 3\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir, path
}

// --file-tz reads a zone-less file's wall clock in the zone: the
// instants move nine hours for Tokyo, display_zone names it, and
// cli_command carries the flag. The human line shows the times as the
// file writes them.
func TestFileTZ_TimeRangeOfAZonelessFile(t *testing.T) {
	dir, timed, _, _, _ := timeRangeFixture(t)
	code, stdout, stderr := runRxIn(t, dir, utcLogs, "time-range", timed, "--file-tz=Asia/Tokyo", "--json")
	var answer map[string]any
	if code != clicommand.ExitSuccess || json.Unmarshal([]byte(stdout), &answer) != nil {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	const nine = 9 * 3600 * 1000
	if answer["first_ms"] != float64(1765350004574-nine) || answer["last_ms"] != float64(1765353599390-nine) ||
		answer["display_zone"] != "Asia/Tokyo" || answer["has_zone"] != false ||
		answer["cli_command"] != "rx time-range "+timed+" --file-tz=Asia/Tokyo" {
		t.Fatalf("answer %v", answer)
	}

	code, stdout, stderr = runRxIn(t, dir, utcLogs, "time-range", timed, "--file-tz=Asia/Tokyo")
	want := timed + "  iso  2025-12-10 07:00:04.574 .. 2025-12-10 07:59:59.390  Asia/Tokyo  scan\n"
	if code != clicommand.ExitSuccess || stdout != want {
		t.Fatalf("exit %d, stdout %q, want %q; stderr %s", code, stdout, want, stderr)
	}
}

// --file-tz on a file whose lines write +02:00 ignores the written zone:
// the wall clocks are read in the chosen zone, and the human line shows
// them as written, in that zone, rather than in UTC.
func TestFileTZ_TimeRangeOfAZonedFile(t *testing.T) {
	dir, path := zonedLogFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "time-range", path, "--file-tz=Asia/Tokyo", "--json")
	var answer map[string]any
	if code != clicommand.ExitSuccess || json.Unmarshal([]byte(stdout), &answer) != nil {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	// 2025-12-10 07:00:04.574 in Tokyo.
	if answer["first_ms"] != float64(1765317604574) || answer["display_zone"] != "Asia/Tokyo" || answer["has_zone"] != true {
		t.Fatalf("answer %v", answer)
	}
	code, stdout, stderr = runRxIn(t, dir, nil, "time-range", path, "--file-tz=Asia/Tokyo")
	if code != clicommand.ExitSuccess || !strings.Contains(stdout, "2025-12-10T07:00:04.574") ||
		!strings.Contains(stdout, "2025-12-10T07:59:59.390") || !strings.Contains(stdout, "  Asia/Tokyo  scan") {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
}

// rx samples --file-tz finds a line by the wall clock read in the zone.
func TestFileTZ_SamplesFindsTheLineInTheZone(t *testing.T) {
	dir, path := zonedLogFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "samples", path, "--timestamps=2025-12-10T07:30:00Z",
		"--file-tz=UTC", "--context=0", "--json")
	var answer struct {
		Timestamps map[string]int64 `json:"timestamps"`
		TimeFormat map[string]any   `json:"time_format"`
	}
	if code != clicommand.ExitSuccess || json.Unmarshal([]byte(stdout), &answer) != nil {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	if answer.Timestamps["2025-12-10T07:30:00Z"] != 2 || answer.TimeFormat["assumed_zone"] != "UTC" ||
		answer.TimeFormat["has_zone"] != true {
		t.Fatalf("answer %+v", answer)
	}
}

// A zone rx does not accept is a usage error naming the value, in every
// command that takes --file-tz, before any file is read.
func TestFileTZ_InvalidZoneExitsTwo(t *testing.T) {
	dir, path := zonedLogFixture(t)
	for _, zone := range []string{"Mars/Base", "+25:00", "+-1:00", "local"} {
		for _, args := range [][]string{
			{"samples", path, "--lines=1", "--file-tz=" + zone},
			{"time-range", path, "--file-tz=" + zone},
		} {
			code, _, stderr := runRxIn(t, dir, nil, args...)
			if code != clicommand.ExitUsageError || !strings.Contains(stderr, zone) || !strings.Contains(stderr, "--file-tz") {
				t.Errorf("%v: exit %d, stderr %q; want 2 naming %s", args, code, stderr, zone)
			}
		}
	}
}

package clicommand

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
)

// writeTimedLog writes a log whose lines carry the given timestamps,
// with one continuation line after the second, and returns its path.
func writeTimedLog(t *testing.T, stamps ...string) string {
	t.Helper()
	var buf bytes.Buffer
	for i, stamp := range stamps {
		buf.WriteString(stamp + " INFO request served\n")
		if i == 1 {
			buf.WriteString("\tat com.example.Service.run(Service.java:10)\n")
		}
	}
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// runIndexInfoHuman builds the index of path, then returns what
// `rx index PATH --info` prints.
func runIndexInfoHuman(t *testing.T, path string) string {
	t.Helper()
	zero := 0
	runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})
	var buf bytes.Buffer
	if err := runIndex(&buf, indexParams{paths: []string{path}, showInfo: true}); err != nil {
		t.Fatalf("rx index --info: %v", err)
	}
	return buf.String()
}

// `rx index --info` shows the time section: the format and how a
// timestamp without a zone is read, the first and last timestamps with
// their lines, how many lines carry one, and the backward steps.
func TestIndexInfo_ShowsTheTimeSection(t *testing.T) {
	cases := []struct {
		name   string
		stamps []string
		want   []string
	}{
		{
			name: "no zone",
			stamps: []string{
				"2025-12-10 07:00:04.574", "2025-12-10 07:00:09.000",
				"2025-12-10 07:00:06.500", "2025-12-10 07:00:10.250",
			},
			want: []string{
				"  time_format: iso, at the start of each line, no zone, read as UTC\n",
				"  first_timestamp: 2025-12-10 07:00:04.574 (line 1)\n",
				"  last_timestamp: 2025-12-10 07:00:10.250 (line 5)\n",
				"  timestamped_lines: 4\n",
				"  backward_steps: 1 (largest 2500 ms)\n",
			},
		},
		{
			name: "numeric zone",
			stamps: []string{
				"2026-10-06T12:34:56.123+02:00", "2026-10-06T12:35:00.000+02:00",
				"2026-10-06T10:35:01.000Z",
			},
			want: []string{
				"  time_format: iso, at the start of each line, with zones (first +02:00)\n",
				"  first_timestamp: 2026-10-06T10:34:56.123Z (line 1)\n",
				"  last_timestamp: 2026-10-06T10:35:01.000Z (line 4)\n",
				"  timestamped_lines: 3\n",
				"  backward_steps: 0\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			got := runIndexInfoHuman(t, writeTimedLog(t, tc.stamps...))
			for _, line := range tc.want {
				if !strings.Contains(got, line) {
					t.Errorf("--info lacks %q:\n%s", line, got)
				}
			}
		})
	}
}

// A file with no timestamp format says so in --info.
func TestIndexInfo_SaysWhenNoTimestampFormatIsRecognized(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	got := runIndexInfoHuman(t, tinyLogFile(t))
	if !strings.Contains(got, "  time_format: none recognized\n") {
		t.Errorf("--info lacks the time_format line:\n%s", got)
	}
	if strings.Contains(got, "first_timestamp") {
		t.Errorf("--info shows timestamps for a file without any:\n%s", got)
	}
}

// `rx index --json` gives the whole time section of each indexed file,
// max_before included, and null for a file without timestamps.
func TestIndexJSON_CarriesTheTimeSection(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	timed := writeTimedLog(t, "2025-12-10 07:00:04.574", "2025-12-10 07:00:05.000", "2025-12-10 07:00:06.000")
	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{timed, tinyLogFile(t)}, threshold: &zero})
	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 2 {
		t.Fatalf("indexed %d files, want 2: %v", len(indexed), result)
	}
	section, ok := indexed[0].(map[string]any)["time_index"].(map[string]any)
	if !ok {
		t.Fatalf("time_index of the timed log is %v; want an object", indexed[0].(map[string]any)["time_index"])
	}
	first := section["first"].(map[string]any)
	wantMs := float64(time.Date(2025, 12, 10, 7, 0, 4, 574e6, time.UTC).UnixMilli())
	if section["format"] != "iso" || section["timestamped_lines"] != float64(3) || first["ms"] != wantMs {
		t.Errorf("time_index = %v; want iso, 3 lines, first ms %.0f", section, wantMs)
	}
	if _, ok := section["max_before"].([]any); !ok {
		t.Errorf("time_index has no max_before list: %v", section)
	}
	entry := indexed[1].(map[string]any)
	if value, present := entry["time_index"]; !present || value != nil {
		t.Errorf("time_index of a log without timestamps = %v (present %v); want null", value, present)
	}
}

// An index stored by the previous format version is not used: `rx
// index` builds a new one, with its time section, in its place.
func TestIndex_RebuildsAnIndexOfThePreviousVersion(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := writeTimedLog(t, "2025-12-10 07:00:04.574", "2025-12-10 07:00:05.000", "2025-12-10 07:00:06.000")
	zero := 0
	runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})

	cachePath := index.GetCachePath(path)
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	stored["version"] = index.Version - 1
	delete(stored, "time_index")
	if body, err = json.Marshal(stored); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatalf("write old index: %v", err)
	}

	runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})
	rebuilt, err := index.LoadFromPath(cachePath)
	if err != nil {
		t.Fatalf("load rebuilt index: %v", err)
	}
	if rebuilt.Version != index.Version || rebuilt.TimeIndex == nil || rebuilt.TimeIndex.Max == nil {
		t.Errorf("stored index has version %d, time section %+v; want %d with max",
			rebuilt.Version, rebuilt.TimeIndex, index.Version)
	}
}

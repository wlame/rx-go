package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// offsetSeconds is a zone's offset from UTC at a fixed winter instant.
func offsetSeconds(loc *time.Location) int {
	_, seconds := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone()
	return seconds
}

// RX_LOG_TZ takes UTC, a zone name or a fixed offset; anything else,
// `local` included, keeps the default UTC.
func TestLogTZ_AcceptsAZoneAndFallsBackToUTC(t *testing.T) {
	cases := []struct {
		value      string
		wantName   string
		wantOffset int
	}{
		{"", "UTC", 0},
		{"UTC", "UTC", 0},
		{"Europe/Berlin", "Europe/Berlin", 3600},
		{"+05:30", "+05:30", 5*3600 + 30*60},
		{"-07:00", "-07:00", -7 * 3600},
		{"+18:00", "+18:00", 18 * 3600},
		{"+18:01", "UTC", 0},
		{"+5:30", "UTC", 0},
		{"+05:60", "UTC", 0},
		// A sign inside a field is not a digit: each field is two digits.
		{"+-1:00", "UTC", 0},
		{"--1:00", "UTC", 0},
		{"+00:-5", "UTC", 0},
		{"+00:+5", "UTC", 0},
		{"+1:5x", "UTC", 0},
		{"+0x:00", "UTC", 0},
		{"Mars/Olympus", "UTC", 0},
		{"local", "UTC", 0},
		{"Local", "UTC", 0},
		{"../../etc/passwd", "UTC", 0},
		// A zone name only in the case the zone database writes it.
		{"utc", "UTC", 0},
		{"europe/berlin", "UTC", 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.value), func(t *testing.T) {
			captureWarnings(t)
			t.Setenv("RX_LOG_TZ", tc.value)
			got := LogTZ()
			if got.Name != tc.wantName || got.Location == nil || offsetSeconds(got.Location) != tc.wantOffset {
				t.Fatalf("LogTZ() = %q at %+v, want %q at offset %d", got.Name, got.Location, tc.wantName, tc.wantOffset)
			}
		})
	}
}

// RX_QUERY_TZ unset names no zone (the file's own frame); `local` names
// the process's zone; a bad value keeps it unset.
func TestQueryTZ_UnsetLocalAndInvalid(t *testing.T) {
	captureWarnings(t)
	t.Setenv("RX_QUERY_TZ", "")
	if got := QueryTZ(); got.Location != nil || got.Name != "" {
		t.Errorf("unset: %+v, want no zone", got)
	}
	t.Setenv("RX_QUERY_TZ", "local")
	if got := QueryTZ(); got.Location != time.Local || got.Name != "local" {
		t.Errorf("local: %+v, want time.Local", got)
	}
	t.Setenv("RX_QUERY_TZ", "Asia/Tokyo")
	if got := QueryTZ(); got.Location == nil || offsetSeconds(got.Location) != 9*3600 {
		t.Errorf("Asia/Tokyo: %+v", got)
	}
	t.Setenv("RX_QUERY_TZ", "nowhere")
	if got := QueryTZ(); got.Location != nil {
		t.Errorf("invalid: %+v, want no zone", got)
	}
}

// A value not accepted warns once per process, naming what is used.
func TestZoneSettings_WarnOnceForAValueNotAccepted(t *testing.T) {
	log := captureWarnings(t)
	t.Setenv("RX_LOG_TZ", "Atlantis/Capital")
	LogTZ()
	LogTZ()
	if got := strings.Count(log.String(), "invalid_setting"); got != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", got, log.String())
	}
	for _, part := range []string{"name=RX_LOG_TZ", "value=Atlantis/Capital", "using=UTC"} {
		if !strings.Contains(log.String(), part) {
			t.Errorf("warning lacks %s:\n%s", part, log.String())
		}
	}
}

// docs/configuration.md lists every zone setting with its default.
func TestZoneSettings_Documented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	for _, s := range ZoneSettings {
		def := "unset"
		if s.Default != "" {
			def = "`" + s.Default + "`"
		}
		row := fmt.Sprintf("| `%s` | %s |", s.Name, def)
		if !bytes.Contains(doc, []byte(row)) {
			t.Errorf("docs/configuration.md lacks the row %s", row)
		}
	}
}

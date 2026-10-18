package config

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"
)

// captureLog sends the log package's output, which slog's default
// handler writes to, into a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

// RX_LOG_LEVEL sets the level of the default logger: a record below it
// is not written. The value is read in any case, WARNING is WARN,
// unset is INFO, and a value that is not a level is reported once and
// read as INFO.
func TestApplyLogLevel(t *testing.T) {
	t.Cleanup(func() { SetLogLevel(slog.LevelInfo) })
	cases := []struct {
		value   string
		level   slog.Level
		name    string
		warning bool
	}{
		{"", slog.LevelInfo, "INFO", false},
		{"debug", slog.LevelDebug, "DEBUG", false},
		{"INFO", slog.LevelInfo, "INFO", false},
		{"warning", slog.LevelWarn, "WARN", false},
		{"Warn", slog.LevelWarn, "WARN", false},
		{"ERROR", slog.LevelError, "ERROR", false},
		{"verbose", slog.LevelInfo, "INFO", true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(LogLevelName, tc.value)
			SetLogLevel(slog.LevelInfo)
			buf := captureLog(t)
			if got := ApplyLogLevel(); got != tc.level {
				t.Errorf("ApplyLogLevel() = %v, want %v", got, tc.level)
			}
			if got := LogLevelInForce(); got != tc.name {
				t.Errorf("LogLevelInForce() = %q, want %q", got, tc.name)
			}
			if warned := strings.Contains(buf.String(), "invalid_setting"); warned != tc.warning {
				t.Errorf("warning written: %v, want %v (%q)", warned, tc.warning, buf.String())
			}
			buf.Reset()
			slog.Default().Info("a_record_at_info")
			slog.Default().Debug("a_record_at_debug")
			if wantInfo := tc.level <= slog.LevelInfo; strings.Contains(buf.String(), "a_record_at_info") != wantInfo {
				t.Errorf("an INFO record written: %v, want %v under %q", !wantInfo, wantInfo, tc.value)
			}
			if wantDebug := tc.level <= slog.LevelDebug; strings.Contains(buf.String(), "a_record_at_debug") != wantDebug {
				t.Errorf("a DEBUG record written: %v, want %v under %q", !wantDebug, wantDebug, tc.value)
			}
		})
	}
}

// The invalid_setting warning for a bad RX_LOG_LEVEL is written once
// per process and value, however often the level is applied.
func TestApplyLogLevel_WarnsOnce(t *testing.T) {
	t.Cleanup(func() { SetLogLevel(slog.LevelInfo) })
	t.Setenv(LogLevelName, "chatty")
	buf := captureLog(t)
	ApplyLogLevel()
	ApplyLogLevel()
	if n := strings.Count(buf.String(), "invalid_setting"); n != 1 {
		t.Errorf("the warning was written %d times, want once: %q", n, buf.String())
	}
}

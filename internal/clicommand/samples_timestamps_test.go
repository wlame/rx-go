package clicommand

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
)

// timedLogFixture writes a log whose lines carry Python-logging
// timestamps (a comma before the milliseconds), with a traceback line.
func timedLogFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	text := "2025-12-10 12:34:55,000 INFO LINE 1\n" +
		"2025-12-10 12:34:56,123 ERROR LINE 2\n" +
		"Traceback LINE 3\n" +
		"2025-12-10 12:34:57,000 INFO LINE 4\n" +
		"2025-12-10 12:34:58,000 INFO LINE 5\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// A value with a comma is one value, the flag repeats, and the answer
// maps each query to its line, the same before and after an index
// build.
func TestSamples_TimestampsByValue(t *testing.T) {
	path := timedLogFixture(t)
	values := []string{"2025-12-10 12:34:56,123", "2025-12-10T12:34:56..2025-12-10T12:34:57"}
	got := samplesanswer.ColdAndIndexed(t, path, 0, func(t testing.TB) any {
		var buf bytes.Buffer
		err := runSamples(&buf, samplesParams{path: path, timestamps: values, ctxLines: 1, jsonOutput: true})
		if err != nil {
			t.Fatalf("runSamples: %v", err)
		}
		return json.RawMessage(buf.Bytes())
	})
	var answer struct {
		Timestamps map[string]int64    `json:"timestamps"`
		Samples    map[string][]string `json:"samples"`
		TimeFormat map[string]any      `json:"time_format"`
	}
	if err := json.Unmarshal(got.(json.RawMessage), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer.Timestamps[values[0]] != 2 || answer.Timestamps[values[1]] != 2 || len(answer.Timestamps) != 2 {
		t.Fatalf("timestamps %v, want both at line 2", answer.Timestamps)
	}
	if s := answer.Samples[values[0]]; len(s) != 3 || !strings.HasSuffix(s[1], "LINE 2") {
		t.Errorf("sample of %s: %q, want lines 1 to 3", values[0], s)
	}
	if s := answer.Samples[values[1]]; len(s) != 3 || !strings.HasSuffix(s[2], "LINE 4") {
		t.Errorf("sample of the range: %q, want lines 2 to 4, the traceback line included", s)
	}
	if answer.TimeFormat["format"] != "iso" || answer.TimeFormat["assumed_zone"] != "UTC" {
		t.Errorf("time_format %v", answer.TimeFormat)
	}
}

// Human output heads each query with the line it found, in line order;
// a query with no line says so on stderr.
func TestSamples_TimestampsHumanOutput(t *testing.T) {
	path := timedLogFixture(t)
	var buf bytes.Buffer
	stderr := captureStderr(t)
	err := runSamples(&buf, samplesParams{
		path: path, timestamps: []string{"2025-12-10 12:34:57,000", "2025-12-10 12:34:55,500", "2026-01-01"},
		ctxLines: 0, colorFlag: "never",
	})
	if err != nil {
		t.Fatalf("runSamples: %v", err)
	}
	out := buf.String()
	first := strings.Index(out, "=== "+path+":2 @ 2025-12-10 12:34:55,500 ===")
	second := strings.Index(out, "=== "+path+":4 @ 2025-12-10 12:34:57,000 ===")
	missing := strings.Index(out, "=== "+path+":-1 @ 2026-01-01 ===")
	if first < 0 || second < first || missing < second {
		t.Fatalf("headings missing or out of line order:\n%s", out)
	}
	if !strings.Contains(stderr(), "Warning: no line at or after 2026-01-01 in the file.") {
		t.Errorf("stderr lacks the warning:\n%s", stderr())
	}
}

// captureStderr sends os.Stderr to a pipe for the rest of the test and
// returns a function that reads what was written so far.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	previous := os.Stderr
	os.Stderr = w
	var once bool
	var captured string
	t.Cleanup(func() { os.Stderr = previous })
	return func() string {
		if !once {
			once = true
			os.Stderr = previous
			_ = w.Close()
			var b bytes.Buffer
			_, _ = b.ReadFrom(r)
			captured = b.String()
		}
		return captured
	}
}

// --json gives every sample line its effective timestamp in every mode:
// the traceback line carries the timestamp of the line before it, and a
// window that starts on it reads back for that line.
func TestSamples_LineTimestampsInJSON(t *testing.T) {
	path := timedLogFixture(t)
	got := samplesanswer.ColdAndIndexed(t, path, 0, func(t testing.TB) any {
		var buf bytes.Buffer
		err := runSamples(&buf, samplesParams{path: path, lines: []string{"3", "2-4"}, ctxLines: 0, jsonOutput: true})
		if err != nil {
			t.Fatalf("runSamples: %v", err)
		}
		return json.RawMessage(buf.Bytes())
	})
	var answer struct {
		LineTimestamps map[string][]*int64 `json:"line_timestamps"`
	}
	if err := json.Unmarshal(got.(json.RawMessage), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	line2, line4 := int64(1765370096123), int64(1765370097000) // 2025-12-10 12:34:56.123 and 12:34:57 UTC
	want := map[string][]int64{"3": {line2}, "2-4": {line2, line2, line4}}
	for key, values := range want {
		got := answer.LineTimestamps[key]
		if len(got) != len(values) {
			t.Fatalf("line_timestamps[%s] = %v, want %v", key, got, values)
		}
		for i, v := range values {
			if got[i] == nil || *got[i] != v {
				t.Errorf("line_timestamps[%s][%d] = %v, want %d", key, i, got[i], v)
			}
		}
	}
}

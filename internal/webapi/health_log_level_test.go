package webapi

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
)

// /health reports the level the default logger really uses, and a
// level of ERROR quiets the http_request line of every request, as an
// operator who sets RX_LOG_LEVEL=ERROR on a busy server expects. Unset,
// the level is INFO and the line is written.
func TestHealth_ReportsTheLogLevelInForce(t *testing.T) {
	t.Cleanup(func() { config.SetLogLevel(slog.LevelInfo) })
	ts := newTestServer(t)
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	for _, tc := range []struct {
		value, want string
		requestLine bool
	}{
		{"ERROR", "ERROR", false},
		{"", "INFO", true},
		{"debug", "DEBUG", true},
	} {
		t.Setenv(config.LogLevelName, tc.value)
		config.ApplyLogLevel()
		buf.Reset()
		_, _, body := get(t, ts.URL+"/health", "")
		var health struct {
			Constants map[string]any `json:"constants"`
		}
		if err := json.Unmarshal([]byte(body), &health); err != nil {
			t.Fatalf("decode /health: %v", err)
		}
		if got := health.Constants["LOG_LEVEL"]; got != tc.want {
			t.Errorf("RX_LOG_LEVEL=%q: /health LOG_LEVEL = %v, want %q", tc.value, got, tc.want)
		}
		if logged := strings.Contains(buf.String(), "http_request"); logged != tc.requestLine {
			t.Errorf("RX_LOG_LEVEL=%q: http_request line written: %v, want %v", tc.value, logged, tc.requestLine)
		}
	}
}

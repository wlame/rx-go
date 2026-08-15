package webapi

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/prometheus"
)

// scrapeMetrics returns the /metrics body as a string.
func scrapeMetrics(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	return string(body)
}

// A metric family that is declared but never incremented is invisible in
// the scrape: the Prometheus client only emits a labeled family once a
// label combination has been used. rx-python reports request counts,
// trace latency and error counts, so an operator switching backends must
// not lose a dashboard panel.
//
// These tests serve a real request and then assert the families appear.
func TestMetrics_TraceRequestIsCounted(t *testing.T) {
	if _, err := os.Stat("/usr/bin/rg"); err != nil {
		t.Skip("ripgrep not available; skipping")
	}
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logPath, []byte("ERROR one\nINFO two\nERROR three\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/trace?path=%s&regexp=ERROR", ts.URL, logPath))
	if err != nil {
		t.Fatalf("get /v1/trace: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/trace status = %d, want 200", resp.StatusCode)
	}

	body := scrapeMetrics(t, ts.URL)
	for _, family := range []string{
		"rx_trace_requests_total",
		"rx_trace_duration_seconds",
	} {
		if !strings.Contains(body, family) {
			t.Errorf("%s is missing from /metrics after a successful trace request", family)
		}
	}
}

func TestMetrics_SamplesRequestIsCounted(t *testing.T) {
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logPath, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&lines=2", ts.URL, logPath))
	if err != nil {
		t.Fatalf("get /v1/samples: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/samples status = %d, want 200", resp.StatusCode)
	}

	if body := scrapeMetrics(t, ts.URL); !strings.Contains(body, "rx_samples_requests_total") {
		t.Error("rx_samples_requests_total is missing from /metrics after a successful samples request")
	}
}

// A failed request must be counted too, and under an error_type an
// operator can alert on.
func TestMetrics_RejectedRequestIsCountedAsAnError(t *testing.T) {
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)

	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/samples?path=/nonexistent/nope.log&lines=2")
	if err != nil {
		t.Fatalf("get /v1/samples: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("/v1/samples on a missing file returned 200")
	}

	body := scrapeMetrics(t, ts.URL)
	if !strings.Contains(body, "rx_errors_total") {
		t.Error("rx_errors_total is missing from /metrics after a rejected request")
	}
	if !strings.Contains(body, `rx_samples_requests_total{status="error"}`) {
		t.Error(`rx_samples_requests_total{status="error"} is missing from /metrics`)
	}
}

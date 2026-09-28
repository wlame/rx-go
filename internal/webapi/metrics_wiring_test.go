package webapi

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	if _, err := exec.LookPath("rg"); err != nil {
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

// responsesAdded returns how much each rx_http_responses_total series
// grew between two snapshots, keyed by the series' labels.
func responsesAdded(before, after metricSnapshot) map[string]float64 {
	added := map[string]float64{}
	for key, value := range after["rx_http_responses_total"] {
		if delta := value - before["rx_http_responses_total"][key]; delta != 0 {
			added[key] = delta
		}
	}
	return added
}

// The middleware counts every response once. A handler that counted its
// own responses as well made one invalid-pattern 400 show up as a 400
// and a 500.
func TestMetrics_InvalidPatternIsOneResponseWithStatus400(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available; skipping")
	}
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("ERROR one\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ts := newTestServer(t)

	before := snapshotMetrics(t)
	query := url.Values{"path": {logPath}, "regexp": {"(unclosed"}}
	if got := mustGet(t, ts.URL+"/v1/trace?"+query.Encode()); got != http.StatusBadRequest {
		t.Fatalf("/v1/trace status = %d, want 400", got)
	}
	added := responsesAdded(before, snapshotMetrics(t))

	want := map[string]float64{"endpoint=/v1/trace,method=GET,status_code=400": 1}
	if !reflect.DeepEqual(added, want) {
		t.Errorf("rx_http_responses_total grew by %v, want %v", added, want)
	}
}

// A request no route matches is labeled with one constant, never with
// its path: every random path a scanner tries would otherwise become a
// series of its own.
func TestMetrics_UnmatchedRequestsShareOneEndpointLabel(t *testing.T) {
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)
	ts := newTestServer(t)

	before := snapshotMetrics(t)
	for _, path := range []string{"/scanner-probe-1", "/v1/scanner-probe-2"} {
		req, err := http.NewRequest(http.MethodPatch, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("patch %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}
	added := responsesAdded(before, snapshotMetrics(t))

	want := map[string]float64{"endpoint=unmatched,method=PATCH,status_code=405": 2}
	if !reflect.DeepEqual(added, want) {
		t.Errorf("rx_http_responses_total grew by %v, want %v", added, want)
	}
}

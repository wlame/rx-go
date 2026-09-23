package webapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/tasks"
)

// A request whose client has gone away gets no answer: the 500 a
// handler writes when its work stops reaches nobody. The request log and
// rx_http_responses_total report it as 499, the status for a request
// the client closed, and the endpoint counter counts it as canceled,
// never as an error or in rx_errors_total, so a canceled request never
// looks like a server error.
func TestCanceledRequestIsLoggedAndCountedAs499(t *testing.T) {
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_NO_INDEX", "true")
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("ERROR one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		endpoint string
		query    url.Values
		counter  string
	}{
		{"/v1/samples", url.Values{"path": {logPath}, "lines": {"2"}}, "rx_samples_requests_total"},
		{"/v1/trace", url.Values{"path": {logPath}, "regexp": {"ERROR"}}, "rx_trace_requests_total"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			var logs bytes.Buffer
			srv := NewServer(Config{
				AppVersion:  "unit-test",
				RipgrepPath: rgPath,
				TaskManager: tasks.New(tasks.Config{}),
				Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
			})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			req := httptest.NewRequest(http.MethodGet, tc.endpoint+"?"+tc.query.Encode(), nil).WithContext(ctx)
			before := snapshotMetrics(t)

			srv.ServeHTTP(httptest.NewRecorder(), req)

			after := snapshotMetrics(t)
			if got := loggedStatuses(t, &logs); !reflect.DeepEqual(got, []int{499}) {
				t.Errorf("http_request statuses logged = %v, want [499]", got)
			}
			wantResponses := map[string]float64{"endpoint=" + tc.endpoint + ",method=GET,status_code=499": 1}
			if got := responsesAdded(before, after); !reflect.DeepEqual(got, wantResponses) {
				t.Errorf("rx_http_responses_total grew by %v, want %v", got, wantResponses)
			}
			if before.moved(after, "rx_errors_total") {
				t.Error("rx_errors_total moved for a canceled request")
			}
			for key, want := range map[string]float64{"status=canceled": 1, "status=error": 0, "status=success": 0} {
				if got := after[tc.counter][key] - before[tc.counter][key]; got != want {
					t.Errorf("%s{%s} grew by %v, want %v", tc.counter, key, got, want)
				}
			}
		})
	}
}

// loggedStatuses returns the status of every http_request record in a
// JSON log.
func loggedStatuses(t *testing.T, logs *bytes.Buffer) []int {
	t.Helper()
	var statuses []int
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		var record struct {
			Msg    string `json:"msg"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if record.Msg == "http_request" {
			statuses = append(statuses, record.Status)
		}
	}
	return statuses
}

// Only a server error is replaced: a status the handler chose for the
// request itself is what it would have been with the client still
// there, and a live request keeps its 500.
func TestReportedStatus(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	cases := []struct {
		name    string
		ctx     context.Context
		written int
		want    int
	}{
		{"canceled server error", canceled, http.StatusInternalServerError, statusClientClosedRequest},
		{"canceled unavailable", canceled, http.StatusServiceUnavailable, statusClientClosedRequest},
		{"canceled answer", canceled, http.StatusOK, http.StatusOK},
		{"canceled bad request", canceled, http.StatusBadRequest, http.StatusBadRequest},
		{"live server error", live, http.StatusInternalServerError, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := reportedStatus(tc.ctx, tc.written); got != tc.want {
			t.Errorf("%s: reportedStatus = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The endpoint counter follows the request log: only an error the log
// reports as 499 counts as canceled; a request refused for its own fault
// stays an error even when its client has gone.
func TestRecordEndpoint_CountsCanceledOnlyForAServerError(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"canceled internal error", canceled, ErrInternal("context canceled"), "canceled"},
		{"canceled plain error", canceled, context.Canceled, "canceled"},
		{"canceled bad request", canceled, ErrBadRequest("bad"), "error"},
		{"live internal error", context.Background(), ErrInternal("boom"), "error"},
		{"canceled success", canceled, nil, "success"},
	}
	for _, tc := range cases {
		var got string
		recordEndpoint(tc.ctx, func(status string) { got = status }, tc.err)
		if got != tc.want {
			t.Errorf("%s: counted %q, want %q", tc.name, got, tc.want)
		}
	}
}

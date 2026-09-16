package webapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// samplesStatusOf sends GET /v1/samples with query and returns the status.
func samplesStatusOf(t *testing.T, ts *httptest.Server, query url.Values) int {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/samples?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The samples context counts stop at the trace endpoint's cap: up to
// MaxTraceContextLines lines per side, and a 422 for one more.
func TestSamples_ContextAboveTheCapIsRefused(t *testing.T) {
	file := sandboxedFile(t, numberedLines(20, 10))
	ts := newTestServer(t)

	for _, param := range []string{"context", "before_context", "after_context"} {
		t.Run(param, func(t *testing.T) {
			query := url.Values{"path": {file}, "lines": {"5"}}

			query.Set(param, strconv.Itoa(MaxTraceContextLines))
			if status := samplesStatusOf(t, ts, query); status != http.StatusOK {
				t.Errorf("%s=%d: status %d, want 200", param, MaxTraceContextLines, status)
			}
			query.Set(param, strconv.Itoa(MaxTraceContextLines+1))
			if status := samplesStatusOf(t, ts, query); status != http.StatusUnprocessableEntity {
				t.Errorf("%s=%d: status %d, want 422", param, MaxTraceContextLines+1, status)
			}
		})
	}
}

// The published contract states the cap for samples as it does for trace.
func TestOpenAPI_SamplesContextParametersDeclareTheCap(t *testing.T) {
	params := liveOperationParameters(t)["samples"]
	for _, name := range []string{"context", "before_context", "after_context"} {
		schema, ok := params[name]
		if !ok {
			t.Errorf("samples declares no %s parameter", name)
			continue
		}
		if got, _ := schema["maximum"].(float64); int(got) != MaxTraceContextLines {
			t.Errorf("%s: maximum %v, want %d", name, schema["maximum"], MaxTraceContextLines)
		}
	}
}

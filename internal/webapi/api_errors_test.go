package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Every path under the API prefixes answers the ApiError shape when no
// route takes it: 404 {"detail": "Not found: PATH"} for an unknown
// path, 405 with the Allow header and {"detail": …} for a method the
// path does not take. /health keeps answering, and a path outside the
// API prefixes gets the viewer's answer (a redirect to /docs without a
// viewer, as before).
func TestAPIErrors_UnknownPathsAndMethodsUnderTheAPIAreJSON(t *testing.T) {
	ts := newTestServer(t)
	do := func(method, path string) (int, http.Header, string) {
		req, err := http.NewRequest(method, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(body)
	}
	detailOf := func(body string) string {
		var e struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatalf("not the ApiError shape: %q (%v)", body, err)
		}
		return e.Detail
	}

	status, header, body := do(http.MethodGet, "/v1/nope")
	if status != http.StatusNotFound || !strings.HasPrefix(header.Get("Content-Type"), "application/json") || detailOf(body) != "Not found: /v1/nope" {
		t.Errorf("GET /v1/nope: %d %s %q", status, header.Get("Content-Type"), body)
	}
	status, header, body = do(http.MethodPost, "/v1/nope")
	if status != http.StatusMethodNotAllowed || !strings.HasPrefix(header.Get("Content-Type"), "application/json") ||
		!strings.Contains(header.Get("Allow"), "GET") || detailOf(body) != "Method POST not allowed: /v1/nope" {
		t.Errorf("POST /v1/nope: %d Allow=%q %s %q", status, header.Get("Allow"), header.Get("Content-Type"), body)
	}
	status, header, body = do(http.MethodPut, "/v1/trace")
	if status != http.StatusMethodNotAllowed || header.Get("Allow") != "GET" || detailOf(body) != "Method PUT not allowed: /v1/trace" {
		t.Errorf("PUT /v1/trace: %d Allow=%q %q", status, header.Get("Allow"), body)
	}
	if status, _, _ = do(http.MethodGet, "/health"); status != http.StatusOK {
		t.Errorf("GET /health: %d", status)
	}
	if status, _, body = do(http.MethodGet, "/some/viewer/route"); status == http.StatusNotFound && strings.Contains(body, "detail") {
		t.Errorf("a path outside the API prefixes got the API's 404: %d %q", status, body)
	}
}

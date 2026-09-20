package webapi

// Go's json.Encoder.Encode terminates every value with a newline, and
// huma's DefaultJSONFormat uses it, so every rx-go response body ended
// with one byte that rx-python's FastAPI body did not have. The
// documents were identical; only the framing differed, which is enough
// to make `diff` on two captured bodies always report a difference and
// to force every cross-backend check to parse before it can compare.
//
// The framing agreed on is the FastAPI one: no trailing newline. These
// tests hold rx-go to it.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// readBody returns the raw bytes, because the point of these tests is
// the byte the decoders throw away.
func readBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // test server URL
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(raw)
}

func TestResponseBody_HasNoTrailingNewline(t *testing.T) {
	ts := newTestServer(t)

	// Every route here answers without a filesystem or ripgrep, so the
	// test stays hermetic. The error case matters as much as the success
	// case: huma renders it through the same format.
	routes := []struct {
		name string
		path string
	}{
		{"health", "/health"},
		{"detectors", "/v1/detectors"},
		{"openapi", "/openapi.json"},
		// An error envelope is rendered by a different code path than a
		// success body, so it has to be checked separately.
		{"a 4xx error envelope", "/v1/samples?path=&lines=1"},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			status, body := readBody(t, ts.URL+route.path)
			if body == "" {
				t.Fatalf("%s: empty body (status %d)", route.path, status)
			}
			if strings.HasSuffix(body, "\n") {
				t.Errorf("%s (status %d): body ends with a newline; the last 40 bytes are %q",
					route.path, status, body[max(0, len(body)-40):])
			}
		})
	}
}

// huma's default config installs a link transformer that puts a
// `$schema` field in every response body. rx-python emits no such key,
// it is declared nowhere in pkg/rxtypes, and a strict decoder —
// DisallowUnknownFields, a pydantic model with extra='forbid' — rejects
// the whole document over it. It was also the last thing standing
// between the two backends and byte-identical bodies.
func TestResponseBody_CarriesNoSchemaKey(t *testing.T) {
	ts := newTestServer(t)

	routes := []string{
		"/health",
		"/v1/detectors",
		// An error envelope goes through a different code path than a
		// success body and grew the key too.
		"/v1/samples?path=&lines=1",
	}

	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			_, body := readBody(t, ts.URL+route)

			var decoded map[string]any
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("%s: decode %q: %v", route, body, err)
			}
			if value, present := decoded["$schema"]; present {
				t.Errorf("%s: body carries $schema = %v", route, value)
			}
		})
	}
}

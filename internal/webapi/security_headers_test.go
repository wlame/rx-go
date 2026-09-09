package webapi

import (
	"net/http"
	"strings"
	"testing"
)

// The viewer renders untrusted content by definition — the log lines it
// displays are attacker-influenced in many deployments — and three
// protections cannot be expressed in the meta tag the SPA carries,
// because a browser ignores them there. They have to come from the
// server, so both backends send them and rx-python sends the same ones.

// wantSecurityHeaders is the table of headers that are the same on every
// response. One copy, so a route that answers differently is caught
// rather than argued about.
var wantSecurityHeaders = map[string]string{
	"X-Frame-Options":        "DENY",
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "no-referrer",
}

func TestSecurityHeaders_OnEveryKindOfResponse(t *testing.T) {
	ts := newServerWithRipgrep(t)

	// A JSON API response, a health probe, the SPA fallback and a 404
	// all pass through the same middleware; if one of them does not,
	// that is the hole worth knowing about.
	for _, path := range []string{"/health", "/v1/detectors", "/", "/no/such/route"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("get %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			for header, want := range wantSecurityHeaders {
				if got := resp.Header.Get(header); got != want {
					t.Errorf("%s: %s is %q, want %q", path, header, got, want)
				}
			}
			// The docs page huma serves sets a fuller policy of its own,
			// which is right — it knows what Swagger UI needs. What must
			// hold everywhere is the clickjacking clause.
			if policy := resp.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "frame-ancestors 'none'") {
				t.Errorf("%s: Content-Security-Policy %q does not deny framing", path, policy)
			}
		})
	}
}

// The header CSP carries only what a meta tag cannot. The full policy
// stays in the SPA's meta tag, which is the artifact that knows what
// Monaco needs — one copy, and the header must not be stricter or it
// would intersect the meta tag into something that breaks the editor.
func TestSecurityHeaders_APIResponsesCarryOnlyFrameAncestors(t *testing.T) {
	ts := newServerWithRipgrep(t)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	policy := resp.Header.Get("Content-Security-Policy")
	if policy != "frame-ancestors 'none'" {
		t.Errorf("Content-Security-Policy: got %q, want only frame-ancestors", policy)
	}
	for _, directive := range []string{"script-src", "style-src", "default-src", "connect-src"} {
		if strings.Contains(policy, directive) {
			t.Errorf("the header CSP names %s; that belongs in the meta tag", directive)
		}
	}
}

package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/tasks"
)

const testToken = "s3cret-token-for-tests"

// newTokenServer is newTestServer with an API token configured.
func newTokenServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := NewServer(Config{
		AppVersion:  "unit-test",
		RipgrepPath: "/usr/bin/rg",
		TaskManager: tasks.New(tasks.Config{}),
		APIToken:    token,
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// get sends a GET with an optional Authorization header and returns the
// status, the WWW-Authenticate header and the body.
func get(t *testing.T, url, authorization string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), string(body)
}

func TestAPIToken_UnsetLeavesEveryRouteOpen(t *testing.T) {
	ts := newTokenServer(t, "")

	status, _, body := get(t, ts.URL+"/v1/detectors", "")

	if status != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", status, body)
	}
}

func TestAPIToken_SetGuardsTheV1Routes(t *testing.T) {
	ts := newTokenServer(t, testToken)
	cases := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"token without the scheme", testToken, http.StatusUnauthorized},
		{"another scheme", "Basic " + testToken, http.StatusUnauthorized},
		{"right token", "Bearer " + testToken, http.StatusOK},
		{"scheme in lower case", "bearer " + testToken, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, challenge, body := get(t, ts.URL+"/v1/detectors", tc.authorization)

			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", status, tc.wantStatus, body)
			}
			if status != http.StatusUnauthorized {
				return
			}
			if !strings.HasPrefix(challenge, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", challenge)
			}
			var envelope map[string]any
			if err := json.Unmarshal([]byte(body), &envelope); err != nil {
				t.Fatalf("401 body is not JSON: %q", body)
			}
			if detail, _ := envelope["detail"].(string); !strings.Contains(detail, "Authorization: Bearer") {
				t.Errorf("detail = %q, want it to say how to authenticate", detail)
			}
			if !strings.Contains(body, "<token>") || strings.HasSuffix(body, "\n") {
				t.Errorf("body %q should read <token> unescaped and end at the brace", body)
			}
		})
	}
}

// Probes, scrapers and the documentation do not carry a token, so only
// the /v1 API asks for one.
func TestAPIToken_SetLeavesProbesAndDocsOpen(t *testing.T) {
	ts := newTokenServer(t, testToken)

	for _, path := range []string{"/health", "/metrics", "/openapi.json", "/docs"} {
		t.Run(path, func(t *testing.T) {
			status, _, body := get(t, ts.URL+path, "")

			if status == http.StatusUnauthorized {
				t.Errorf("%s answered 401 without a token: %s", path, body)
			}
		})
	}
}

// /health is open and reports the RX_* environment, so a secret among
// those variables must be reported as set, never by value.
func TestHealth_RedactsSecretEnvironmentValues(t *testing.T) {
	t.Setenv("RX_API_TOKEN", testToken)
	t.Setenv("RX_LARGE_FILE_MB", "123")
	ts := newTestServer(t)

	_, _, body := get(t, ts.URL+"/health", "")

	if strings.Contains(body, testToken) {
		t.Fatalf("/health exposes the token: %s", body)
	}
	var health struct {
		Environment map[string]string `json:"environment"`
	}
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if got := health.Environment["RX_API_TOKEN"]; got != redactedEnvValue {
		t.Errorf("RX_API_TOKEN = %q, want %q", got, redactedEnvValue)
	}
	if got := health.Environment["RX_LARGE_FILE_MB"]; got != "123" {
		t.Errorf("RX_LARGE_FILE_MB = %q, want it reported as is", got)
	}
}

// The OpenAPI document says the /v1 operations accept a bearer token and
// may answer 401, and that the token is optional — a server without
// RX_API_TOKEN set ignores it.
func TestOpenAPI_DeclaresTheOptionalBearerToken(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := get(t, ts.URL+"/openapi.json", "")
	var doc struct {
		Components struct {
			SecuritySchemes map[string]struct {
				Type   string `json:"type"`
				Scheme string `json:"scheme"`
			} `json:"securitySchemes"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			Security  []map[string][]string `json:"security"`
			Responses map[string]any        `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}

	scheme := doc.Components.SecuritySchemes["bearerAuth"]
	if scheme.Type != "http" || scheme.Scheme != "bearer" {
		t.Errorf("bearerAuth scheme = %+v, want http bearer", scheme)
	}
	detectors := doc.Paths["/v1/detectors"]["get"]
	if len(detectors.Security) != 2 || len(detectors.Security[1]) != 0 {
		t.Errorf("/v1/detectors security = %v, want bearerAuth or nothing", detectors.Security)
	}
	if _, ok := detectors.Responses["401"]; !ok {
		t.Errorf("/v1/detectors declares no 401")
	}
	if health := doc.Paths["/health"]["get"]; len(health.Security) != 0 {
		t.Errorf("/health security = %v, want none", health.Security)
	}
}

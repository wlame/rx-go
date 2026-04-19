package webapi

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/tasks"
)

// TestIntegration_SPAFlow extracts the real v0.2.0 rx-viewer tarball
// into a temp dir, stands up a full Server, and exercises every
// SPA-serving code path. This is the Stage 9 parity probe for frontend
// compatibility: if rx-viewer's asset references resolve correctly
// against the Go server, production deployments will Just Work.
func TestIntegration_SPAFlow(t *testing.T) {
	// Extract the fixture tarball into a temp dir that will become the
	// frontend cache.
	cacheDir := t.TempDir()
	if err := extractTestTarball(t, "testdata/rx-viewer-v0.2.0-dist.tar.gz", cacheDir); err != nil {
		t.Fatalf("extract fixture: %v", err)
	}

	// Ensure the extraction worked: index.html must exist.
	if _, err := os.Stat(filepath.Join(cacheDir, "index.html")); err != nil {
		t.Fatalf("index.html missing after extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "assets")); err != nil {
		t.Fatalf("assets dir missing after extract: %v", err)
	}

	// Build a frontend.Manager backed by that cache dir.
	fm := frontend.NewManager(frontend.Config{
		CacheDir: cacheDir,
	})
	if !fm.IsAvailable() {
		t.Fatalf("frontend cache reports unavailable after extract")
	}

	// Stand up a server and wrap it in httptest.
	srv := NewServer(Config{
		AppVersion:  "test-0.0.0",
		Frontend:    fm,
		Hooks:       hooks.NewDispatcher(hooks.DispatcherConfig{}),
		TaskManager: tasks.New(tasks.Config{}),
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tests := []struct {
		name          string
		path          string
		expectStatus  int
		expectTypeHas string // prefix match on Content-Type
		bodyContains  string // substring assertion on body
		bodyMinBytes  int
	}{
		{
			name:          "GET / serves SPA index",
			path:          "/",
			expectStatus:  http.StatusOK,
			expectTypeHas: "text/html",
			bodyContains:  `id="app"`,
		},
		{
			name:          "GET /index.html serves itself",
			path:          "/index.html",
			expectStatus:  http.StatusOK,
			expectTypeHas: "text/html",
			bodyContains:  `id="app"`,
		},
		{
			name:          "GET /assets/index-D8mRyN95.js",
			path:          "/assets/index-D8mRyN95.js",
			expectStatus:  http.StatusOK,
			expectTypeHas: "application/javascript",
			bodyMinBytes:  1024, // it's a real SPA entry, plenty of bytes
		},
		{
			name:          "GET /assets/monaco-editor-B9--RLhC.js is huge",
			path:          "/assets/monaco-editor-B9--RLhC.js",
			expectStatus:  http.StatusOK,
			expectTypeHas: "application/javascript",
			bodyMinBytes:  100 * 1024,
		},
		{
			name:          "GET /assets/index-BsAyAYl0.css",
			path:          "/assets/index-BsAyAYl0.css",
			expectStatus:  http.StatusOK,
			expectTypeHas: "text/css",
			bodyMinBytes:  1,
		},
		{
			name:          "GET /favicon.svg from cache",
			path:          "/favicon.svg",
			expectStatus:  http.StatusOK,
			expectTypeHas: "image/svg+xml",
		},
		{
			name:          "GET /favicon.ico from embedded",
			path:          "/favicon.ico",
			expectStatus:  http.StatusOK,
			expectTypeHas: "image/svg+xml",
		},
		{
			name:          "GET /nonexistent-spa-route falls back to index.html",
			path:          "/nonexistent-spa-route",
			expectStatus:  http.StatusOK,
			expectTypeHas: "text/html",
			bodyContains:  `id="app"`,
		},
		{
			name:          "GET /deep/spa/route falls back to index.html",
			path:          "/deep/spa/route",
			expectStatus:  http.StatusOK,
			expectTypeHas: "text/html",
			bodyContains:  `id="app"`,
		},
		{
			name:         "GET /v1/nonexistent returns 404 not SPA",
			path:         "/v1/nonexistent-endpoint",
			expectStatus: http.StatusNotFound,
		},
		{
			name:          "GET /health returns JSON",
			path:          "/health",
			expectStatus:  http.StatusOK,
			expectTypeHas: "application/json",
			bodyContains:  `"status":"ok"`,
		},
		{
			// huma v2 emits the RFC 8259-correct "application/openapi+json"
			// media type. Python FastAPI uses "application/json". Both are
			// acceptable per the OpenAPI spec; tools render either correctly.
			name:          "GET /openapi.json is a valid OpenAPI 3.1 document",
			path:          "/openapi.json",
			expectStatus:  http.StatusOK,
			expectTypeHas: "json",
			bodyContains:  `"openapi":"3.1.0"`,
		},
		{
			name:          "GET /v1/detectors returns empty registry + fixed scale",
			path:          "/v1/detectors",
			expectStatus:  http.StatusOK,
			expectTypeHas: "application/json",
			bodyContains:  `"severity_scale"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.expectStatus {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.expectStatus)
			}
			if tc.expectTypeHas != "" {
				ct := resp.Header.Get("Content-Type")
				if !strings.Contains(ct, tc.expectTypeHas) {
					t.Errorf("Content-Type: got %q, want prefix %q", ct, tc.expectTypeHas)
				}
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if tc.bodyContains != "" && !strings.Contains(string(body), tc.bodyContains) {
				t.Errorf("body missing substring %q. Got first 200 chars: %q",
					tc.bodyContains, string(body[:min(200, len(body))]))
			}
			if tc.bodyMinBytes > 0 && len(body) < tc.bodyMinBytes {
				t.Errorf("body len %d < expected min %d", len(body), tc.bodyMinBytes)
			}
		})
	}
}

// TestIntegration_HealthShape verifies the /health response includes
// the Go-specific fields (go_version, go_packages) and excludes the
// Python-specific ones that the spec dropped.
func TestIntegration_HealthShape(t *testing.T) {
	srv := NewServer(Config{
		AppVersion: "test-1.2.3",
		Frontend:   nil, // no frontend — tests the "docs redirect" path elsewhere
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if _, ok := body["go_version"]; !ok {
		t.Errorf("health missing go_version")
	}
	if _, ok := body["go_packages"]; !ok {
		t.Errorf("health missing go_packages")
	}
	if _, ok := body["python_version"]; ok {
		t.Errorf("health should not emit python_version (Go port)")
	}
	if _, ok := body["python_packages"]; ok {
		t.Errorf("health should not emit python_packages (Go port)")
	}
	if v := body["app_version"]; v != "test-1.2.3" {
		t.Errorf("app_version: got %v, want test-1.2.3", v)
	}
}

// TestIntegration_NoFrontendRedirect verifies that when no frontend is
// extracted, GET / redirects to /docs rather than 404ing.
func TestIntegration_NoFrontendRedirect(t *testing.T) {
	srv := NewServer(Config{
		AppVersion: "test-0.0.0",
		Frontend:   nil,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Prevent automatic redirect following.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status: got %d, want 307", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/docs" {
		t.Errorf("Location: got %q, want /docs", loc)
	}
}

// TestIntegration_ReservedPrefixes verifies every reserved prefix is
// recognized (cannot be SPA-fallbacked).
func TestIntegration_ReservedPrefixes(t *testing.T) {
	reserved := []string{
		"v1/anything",
		"health",
		"metrics",
		"docs",
		"redoc",
		"openapi.json",
	}
	for _, p := range reserved {
		if !isReservedAPIPath(p) {
			t.Errorf("isReservedAPIPath(%q) = false, want true", p)
		}
	}
	nonreserved := []string{
		"assets/app.js",
		"favicon.svg",
		"some-spa-route",
		"healthxxx",
	}
	for _, p := range nonreserved {
		if isReservedAPIPath(p) {
			t.Errorf("isReservedAPIPath(%q) = true, want false", p)
		}
	}
}

// ============================================================================
// Test helpers
// ============================================================================

// extractTestTarball extracts a gzipped tarball to destDir. Stripped-down
// version of internal/frontend.extractTarGz (which is private) since we
// only need it for test setup.
func extractTestTarball(t *testing.T, tarPath, destDir string) error {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		// Strip a leading "./" prefix produced by the SPA build.
		clean = strings.TrimPrefix(clean, "./")
		if clean == "" || clean == "." {
			continue
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			_ = out.Close()
		}
	}
}

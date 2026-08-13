package frontend

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestViewerVersionCompatible pins the range this backend was built
// against. The rule and the numbers must match rx-python's.
func TestViewerVersionCompatible(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"0.2.0", true},
		{"v0.2.0", true},
		{"0.2.9", true},
		{"0.2.10-rc1", true}, // a pre-release of a supported patch
		{"0.1.9", false},     // below the minimum
		{"0.3.0", false},     // next minor: a 0.x minor may break
		{"1.0.0", false},
		{"2.4.1", false},
		{"", true}, // unknown version: do not block on a bad parse
		{"not-a-version", true},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			if got := viewerVersionCompatible(tc.version); got != tc.want {
				t.Errorf("viewerVersionCompatible(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// TestEnsure_DeclinesIncompatibleLatestRelease is scenario 5: a viewer
// past the supported range is not installed automatically, the reason is
// logged, and Ensure does not fail — the server runs without the SPA.
func TestEnsure_DeclinesIncompatibleLatestRelease(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v9.9.9",
			"assets": [{"name": "dist.tar.gz", "browser_download_url": "http://127.0.0.1:1/dist.tar.gz"}]
		}`))
	}))
	t.Cleanup(api.Close)

	var logged strings.Builder
	cache := t.TempDir()
	m := NewManager(Config{
		CacheDir: cache,
		APIBase:  api.URL,
		Logger:   slog.New(slog.NewTextHandler(&logged, nil)),
	})

	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: got %v, want nil (a newer viewer is not an error)", err)
	}
	if m.IsAvailable() {
		t.Errorf("an out-of-range viewer must not be installed")
	}
	if !strings.Contains(logged.String(), "frontend_version_incompatible") {
		t.Errorf("the refusal should be logged: %s", logged.String())
	}
	if !strings.Contains(logged.String(), "RX_FRONTEND_VERSION") {
		t.Errorf("the log should name the override: %s", logged.String())
	}
}

package frontend

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestViewerVersionCompatible pins the range this backend was built
// against. rx-python keeps its own window while it is paused, so these
// numbers may differ from its.
//
// Contract 2.0 removed what viewers before 0.8.0 read (the index
// analysis, GET /v1/detectors), and viewer 0.8.0 is the first that
// speaks contract 2, so the window starts there. Contract 2.1 only adds
// members, so viewer 0.8.x keeps working beside the 0.9.x viewer that
// reads them, and the window ends below 0.10.0.
func TestViewerVersionCompatible(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"0.8.0", true}, // the first viewer that speaks contract 2
		{"v0.8.0", true},
		{"0.8.1", true},
		{"0.8.5", true},
		{"0.8.10-rc1", true}, // a pre-release of a supported patch
		{"0.9.0", true},      // the viewer that reads contract 2.1
		{"v0.9.0", true},
		{"0.9.5", true},
		{"0.9.12", true}, // numbers compare as numbers, not as text
		{"0.7.1", false}, // speaks contract 1 only
		{"0.7.9", false},
		{"0.2.0", false},
		{"0.1.9", false},
		{"0.10.0", false}, // past the window: a 0.x minor may break
		{"0.10.3", false},
		{"0.11.0", false},
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

// TestEnsure_DeclinesReleasesOutsideTheRange: with no cache and only a
// release past the supported range published, nothing is installed and
// Ensure says why, naming the newest release and the override. The
// server runs without the SPA.
func TestEnsure_DeclinesReleasesOutsideTheRange(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{
			"tag_name": "v9.9.9",
			"assets": [{"name": "dist.tar.gz", "browser_download_url": "http://127.0.0.1:1/dist.tar.gz"}]
		}]`))
	}))
	t.Cleanup(api.Close)

	cache := t.TempDir()
	m := NewManager(Config{
		CacheDir: cache,
		APIBase:  api.URL,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	m.envURL, m.envVersion = "", ""

	served, err := m.Ensure(context.Background())

	if err == nil {
		t.Fatal("Ensure: want an error naming the refused release")
	}
	for _, want := range []string{"v9.9.9", "RX_FRONTEND_VERSION", supportedRange()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
	if served != (Served{Reason: ServedNone}) {
		t.Errorf("served = %+v, want none", served)
	}
	if m.IsAvailable() {
		t.Errorf("an out-of-range viewer must not be installed")
	}
}

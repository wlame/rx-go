package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestDownload_RefusesRedirectToInternalAddress asserts the bundle
// download re-checks every redirect hop. GitHub legitimately redirects
// asset URLs to a CDN, so redirects stay enabled — but a hop that
// lands on a loopback or metadata address is refused instead of
// followed.
func TestDownload_RefusesRedirectToInternalAddress(t *testing.T) {
	var internalHits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&internalHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(internal.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	m := NewManager(Config{CacheDir: t.TempDir()})
	err := m.Download(context.Background(), redirector.URL+"/dist.tar.gz", "v9.9.9")

	if err == nil {
		t.Fatalf("Download: got nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error should name the redirect: %v", err)
	}
	if got := atomic.LoadInt32(&internalHits); got != 0 {
		t.Errorf("internal target was fetched: %d hits, want 0", got)
	}
}

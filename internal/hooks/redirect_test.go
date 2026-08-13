package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/trace"
)

// TestDispatcher_DoesNotFollowRedirect asserts a hook target cannot
// bounce the dispatcher onto a second host. ValidateURL only sees the
// first URL, so following a redirect would hand an attacker a request
// to any address the server can reach — including the ones the SSRF
// guard rejects outright.
func TestDispatcher_DoesNotFollowRedirect(t *testing.T) {
	var internalHits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&internalHits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(internal.Close)

	var redirectHits int32
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&redirectHits, 1)
		http.Redirect(w, r, internal.URL+"/internal", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	d := NewDispatcher(DispatcherConfig{
		Env:        HookEnv{OnFileURL: redirector.URL},
		RequestID:  "req-redirect",
		Workers:    1,
		QueueDepth: 4,
		Timeout:    2 * time.Second,
		Logger:     newSilentLogger(),
	})
	d.OnFile(context.Background(), "/var/log/test.log", trace.FileInfo{})
	d.Close()
	d.Wait()

	if got := atomic.LoadInt32(&redirectHits); got != 1 {
		t.Fatalf("redirector hits: got %d, want 1", got)
	}
	if got := atomic.LoadInt32(&internalHits); got != 0 {
		t.Errorf("redirect target was followed: %d hits, want 0", got)
	}
}

// TestDispatcher_RedirectToPublicHostIsAlsoRefused pins the policy:
// every redirect is refused, not only the ones aimed at an internal
// address. A public second hop is still a host the operator never
// configured.
func TestDispatcher_RedirectToPublicHostIsAlsoRefused(t *testing.T) {
	var secondHits int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(second.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/next", http.StatusMovedPermanently)
	}))
	t.Cleanup(redirector.Close)

	d := NewDispatcher(DispatcherConfig{
		Env:        HookEnv{OnCompleteURL: redirector.URL},
		RequestID:  "req-public-redirect",
		Workers:    1,
		QueueDepth: 4,
		Timeout:    2 * time.Second,
		Logger:     newSilentLogger(),
	})
	d.enqueue(hookEvent{kind: "on_complete", url: redirector.URL, requestID: "req-public-redirect"})
	d.Close()
	d.Wait()

	if got := atomic.LoadInt32(&secondHits); got != 0 {
		t.Errorf("redirect target was followed: %d hits, want 0", got)
	}
}

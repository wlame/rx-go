package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookCallbacks_HasAny(t *testing.T) {
	tests := []struct {
		name     string
		hooks    HookCallbacks
		expected bool
	}{
		{"empty", HookCallbacks{}, false},
		{"only file", HookCallbacks{OnFileScanned: "http://example.com"}, true},
		{"only match", HookCallbacks{OnMatchFound: "http://example.com"}, true},
		{"only complete", HookCallbacks{OnComplete: "http://example.com"}, true},
		{"all set", HookCallbacks{
			OnFileScanned: "http://a.com",
			OnMatchFound:  "http://b.com",
			OnComplete:    "http://c.com",
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.hooks.HasAny())
		})
	}
}

func TestHookCallbacks_HasMatchHook(t *testing.T) {
	assert.False(t, HookCallbacks{}.HasMatchHook())
	assert.True(t, HookCallbacks{OnMatchFound: "http://example.com"}.HasMatchHook())
	assert.False(t, HookCallbacks{OnFileScanned: "http://example.com"}.HasMatchHook())
}

func TestGetEffectiveHooks_RequestPriority(t *testing.T) {
	reqHooks := HookCallbacks{
		OnFileScanned: "http://req-file.com",
		OnMatchFound:  "http://req-match.com",
	}
	envHooks := HookCallbacks{
		OnFileScanned: "http://env-file.com",
		OnMatchFound:  "http://env-match.com",
		OnComplete:    "http://env-complete.com",
	}

	// Request hooks take priority when custom hooks are allowed.
	effective := GetEffectiveHooks(reqHooks, envHooks, false)

	assert.Equal(t, "http://req-file.com", effective.OnFileScanned, "request hook should win")
	assert.Equal(t, "http://req-match.com", effective.OnMatchFound, "request hook should win")
	assert.Equal(t, "http://env-complete.com", effective.OnComplete, "env fallback when request is empty")
}

func TestGetEffectiveHooks_EnvFallback(t *testing.T) {
	reqHooks := HookCallbacks{} // no request hooks
	envHooks := HookCallbacks{
		OnFileScanned: "http://env-file.com",
		OnComplete:    "http://env-complete.com",
	}

	effective := GetEffectiveHooks(reqHooks, envHooks, false)

	assert.Equal(t, "http://env-file.com", effective.OnFileScanned)
	assert.Equal(t, "", effective.OnMatchFound)
	assert.Equal(t, "http://env-complete.com", effective.OnComplete)
}

func TestGetEffectiveHooks_DisableCustom(t *testing.T) {
	reqHooks := HookCallbacks{
		OnFileScanned: "http://req-file.com",
		OnMatchFound:  "http://req-match.com",
		OnComplete:    "http://req-complete.com",
	}
	envHooks := HookCallbacks{
		OnFileScanned: "http://env-file.com",
	}

	// When custom hooks are disabled, only env hooks are used.
	effective := GetEffectiveHooks(reqHooks, envHooks, true)

	assert.Equal(t, "http://env-file.com", effective.OnFileScanned)
	assert.Equal(t, "", effective.OnMatchFound, "request hook should be ignored")
	assert.Equal(t, "", effective.OnComplete, "request hook should be ignored")
}

func TestCallHook_Delivery(t *testing.T) {
	// Track received query parameters.
	var receivedParams map[string]string
	var callCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		receivedParams = make(map[string]string)
		for k, v := range r.URL.Query() {
			receivedParams[k] = v[0]
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	params := map[string]string{
		"request_id": "test-123",
		"file":       "/tmp/test.log",
		"matches":    "5",
	}

	err := CallHook(context.Background(), srv.URL, params)
	require.NoError(t, err)

	assert.Equal(t, int32(1), atomic.LoadInt32(&callCount))
	assert.Equal(t, "test-123", receivedParams["request_id"])
	assert.Equal(t, "/tmp/test.log", receivedParams["file"])
	assert.Equal(t, "5", receivedParams["matches"])
}

func TestCallHook_EmptyURL(t *testing.T) {
	// Empty URL should be a no-op (no error).
	err := CallHook(context.Background(), "", map[string]string{"key": "val"})
	assert.NoError(t, err)
}

func TestCallHook_Timeout(t *testing.T) {
	// Server that sleeps longer than the hook timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	err := CallHook(context.Background(), srv.URL, map[string]string{"request_id": "timeout-test"})
	elapsed := time.Since(start)

	// Should fail with a context deadline exceeded error.
	assert.Error(t, err)
	// Should have returned within ~3 seconds (the hook timeout), not 5.
	assert.Less(t, elapsed, 4*time.Second, "hook should timeout at %d seconds", HookTimeoutSeconds)
}

func TestCallHook_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Bad status is logged but not an error return.
	err := CallHook(context.Background(), srv.URL, map[string]string{"request_id": "bad-status"})
	assert.NoError(t, err)
}

func TestCallHook_InvalidURL(t *testing.T) {
	err := CallHook(context.Background(), "://invalid-url", map[string]string{})
	assert.Error(t, err)
}

func TestCallHook_UnreachableHost(t *testing.T) {
	// Port 1 is almost never listening.
	err := CallHook(context.Background(), "http://127.0.0.1:1/hook", map[string]string{"request_id": "unreachable"})
	assert.Error(t, err)
}

func TestCallHookAsync_FireAndForget(t *testing.T) {
	var called int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	CallHookAsync(context.Background(), srv.URL, map[string]string{"request_id": "async-test"})

	// Wait briefly for the goroutine to fire.
	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, int32(1), atomic.LoadInt32(&called))
}

func TestCallHookAsync_EmptyURL(t *testing.T) {
	// Should not panic or start a goroutine.
	CallHookAsync(context.Background(), "", map[string]string{})
}

func TestGetEffectiveHooks_AllEmpty(t *testing.T) {
	effective := GetEffectiveHooks(HookCallbacks{}, HookCallbacks{}, false)
	assert.False(t, effective.HasAny())
}

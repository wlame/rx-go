package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// The server runs with the hook environment it was started with: a hook
// URL set in the environment afterwards neither fires nor shows in
// /health, and the address policy a request's hook URL is checked
// against is the one the server started with.
func TestTraceHooks_HTTP_TheHookEnvironmentIsReadOnce(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "false")
	h := newHookedTraceWithEnv(t, map[string]string{"RX_ALLOW_INTERNAL_HOOKS": "false"})
	rec := newWebhookReceiver(t)
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", rec.server.URL)

	status, _ := h.trace(t, url.Values{"hook_on_complete": {rec.server.URL}})
	if status != http.StatusBadRequest {
		t.Errorf("a loopback hook URL after the environment allowed internal hooks: status %d, want 400 from the snapshot", status)
	}
	h.mustTrace(t, nil)
	h.drain()
	if got := len(rec.received()); got != 0 {
		t.Errorf("the environment URL set after the start fired %d times", got)
	}
	_, _, body := get(t, h.server.URL+"/health", "")
	var health struct {
		Hooks map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if health.Hooks["RX_HOOK_ON_COMPLETE_URL"] != "" || health.Hooks["hooks_configured"] != false {
		t.Errorf("/health reports the environment set after the start: %v", health.Hooks)
	}
}

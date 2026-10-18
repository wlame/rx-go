package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every command honors RX_LOG_LEVEL: under ERROR a trace writes no
// invalid_setting warning for a bad integer setting, and without the
// variable it does; under DEBUG a trace with a hook that answers writes
// the hook_fired record the dispatcher keeps at Debug.
func TestRxBinary_HonorsLogLevel(t *testing.T) {
	_, path := writeLog(t)
	bad := "RX_MAX_SUBPROCESSES=abc"

	if code, _, stderr := runRxEnv(t, []string{bad}, "trace", "error", path, "--json"); code != 0 || !strings.Contains(stderr, "invalid_setting") {
		t.Errorf("without RX_LOG_LEVEL: exit %d, stderr %q; want the invalid_setting warning", code, stderr)
	}
	if code, _, stderr := runRxEnv(t, []string{bad, "RX_LOG_LEVEL=ERROR"}, "trace", "error", path, "--json"); code != 0 || strings.Contains(stderr, "invalid_setting") {
		t.Errorf("under ERROR: exit %d, stderr %q; want no warning", code, stderr)
	}
	if code, _, stderr := runRxEnv(t, []string{"RX_LOG_LEVEL=verbose"}, "trace", "error", path, "--json"); code != 0 ||
		!strings.Contains(stderr, "invalid_setting") || !strings.Contains(stderr, "RX_LOG_LEVEL") {
		t.Errorf("under verbose: exit %d, stderr %q; want one invalid_setting warning naming RX_LOG_LEVEL", code, stderr)
	}

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(hook.Close)
	env := []string{"RX_ALLOW_INTERNAL_HOOKS=true", "RX_HOOK_ON_COMPLETE_URL=" + hook.URL}
	if code, _, stderr := runRxEnv(t, env, "trace", "error", path, "--json"); code != 0 || strings.Contains(stderr, "hook_fired") {
		t.Errorf("at INFO: exit %d, stderr %q; want no hook_fired record", code, stderr)
	}
	if code, _, stderr := runRxEnv(t, append(env, "RX_LOG_LEVEL=DEBUG"), "trace", "error", path, "--json"); code != 0 || !strings.Contains(stderr, "hook_fired") {
		t.Errorf("under DEBUG: exit %d, stderr %q; want the hook_fired record", code, stderr)
	}
}

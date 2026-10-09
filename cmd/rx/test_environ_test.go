package main

// The binaries these tests start inherit the environment of the test
// run, which is the environment of a developer's shell. A variable
// there that sends data out — a webhook URL — would send each test's
// paths and matches to that target, so the helpers that start rx leave
// such variables out.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// outboundEnvPrefixes are the prefixes of the environment variables a
// test never passes on to the binaries it starts: the variables that
// make rx send data out. RX_HOOK_ covers the webhook URLs
// (RX_HOOK_ON_FILE_URL, RX_HOOK_ON_MATCH_URL, RX_HOOK_ON_COMPLETE_URL)
// and the other hook settings. A test that wants a hook passes its URL
// as an extra variable or a flag, which testEnviron keeps.
var outboundEnvPrefixes = []string{"RX_HOOK_"}

// testEnviron is the environment for a binary a test starts: this
// process's environment without the variables of outboundEnvPrefixes,
// then extra, so a variable in extra wins over the inherited one.
func testEnviron(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), isOutboundVariable)
	return append(env, extra...)
}

// isOutboundVariable reports whether entry, a NAME=value entry of the
// environment, names a variable of outboundEnvPrefixes.
func isOutboundVariable(entry string) bool {
	name, _, _ := strings.Cut(entry, "=")
	for _, prefix := range outboundEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// rxCommand is the command that runs the built binary with args, in the
// environment testEnviron gives (with extra): every test starts rx
// through it.
func rxCommand(t *testing.T, extra []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(rxBinary(t), args...)
	cmd.Env = testEnviron(extra...)
	return cmd
}

// A webhook URL set in the environment of the test run reaches none of
// the binaries the tests start, by any of the helpers that start one:
// a trace that would call it on completion calls nothing.
func TestRxBinary_GetsNoWebhookVariableOfTheTestRun(t *testing.T) {
	// calls counts the requests the hook receives. The handler runs on
	// the test server's goroutines, so the count is atomic.
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)
	// The hook listens on loopback, which the URL guard refuses unless
	// internal targets are allowed: without this, a leaked URL would be
	// refused rather than called, and the test would prove nothing.
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", hook.URL)
	dir, path := writeLog(t)

	runs := map[string]func() (int, string, string){
		"runRx":    func() (int, string, string) { return runRx(t, "trace", "error", path, "--json") },
		"runRxIn":  func() (int, string, string) { return runRxIn(t, dir, nil, "trace", "error", path, "--json") },
		"runRxEnv": func() (int, string, string) { return runRxEnv(t, nil, "trace", "error", path, "--json") },
	}
	for name, run := range runs {
		if code, _, stderr := run(); code != 0 {
			t.Fatalf("%s: rx trace exited %d: %s", name, code, stderr)
		}
	}

	if n := calls.Load(); n != 0 {
		t.Errorf("the hook of the test run's environment received %d calls; want none", n)
	}
}

// testEnviron drops the webhook variables of the test run, keeps every
// other variable, and lets an extra variable, a hook URL included, win.
func TestTestEnviron_DropsTheWebhookVariablesOnly(t *testing.T) {
	t.Setenv("RX_HOOK_ON_MATCH_URL", "https://hooks.example.invalid/match")
	t.Setenv("RX_HOOK_STRICT_IP_ONLY", "true")
	t.Setenv("RX_LARGE_FILE_MB", "7")

	env := testEnviron("RX_CACHE_DIR=/tmp/a-cache", "RX_HOOK_ON_FILE_URL=https://hooks.example.invalid/file")

	for _, entry := range env[:len(env)-2] {
		if isOutboundVariable(entry) {
			t.Errorf("the inherited %s was passed on", entry)
		}
	}
	if !slices.Contains(env, "RX_LARGE_FILE_MB=7") {
		t.Error("RX_LARGE_FILE_MB=7 was dropped")
	}
	if tail := env[len(env)-2:]; !slices.Equal(tail, []string{"RX_CACHE_DIR=/tmp/a-cache", "RX_HOOK_ON_FILE_URL=https://hooks.example.invalid/file"}) {
		t.Errorf("the extra variables are %v, want them last, as given", tail)
	}
}

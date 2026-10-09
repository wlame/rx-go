package testparity

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

// TestFixtures_Created verifies EnsureFixtures produces the basic files.
func TestFixtures_Created(t *testing.T) {
	EnsureFixtures(t)
	for _, name := range []string{FixtureTiny, FixtureMedium, FixtureBinary} {
		info, err := os.Stat(FixturePath(name))
		if err != nil {
			t.Errorf("fixture %s missing: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("fixture %s is empty", name)
		}
	}
}

// TestCompressedFixtures verifies EnsureCompressedFixtures creates all
// 4 variants of a source fixture.
func TestCompressedFixtures(t *testing.T) {
	EnsureFixtures(t)
	src := FixturePath(FixtureTiny)
	got := EnsureCompressedFixtures(t, src)

	// We expect entries for gz, xz, zst, and possibly bz2 (skipped on
	// hosts without bzip2).
	wantFormats := []string{"gz", "xz", "zst"}
	for _, fmtName := range wantFormats {
		path, ok := got[fmtName]
		if !ok {
			t.Errorf("missing %s variant in result map", fmtName)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s variant not on disk: %v", fmtName, err)
			continue
		}
		// Tiny is ~220 bytes. Compressed should typically be < 300 bytes,
		// but we only assert non-empty to stay robust to library updates.
		if info.Size() == 0 {
			t.Errorf("%s variant is empty", fmtName)
		}
	}
}

// TestAssertJSONParity_BasicMatch verifies the differ returns "" for
// identical JSON payloads.
func TestAssertJSONParity_BasicMatch(t *testing.T) {
	a := []byte(`{"ok":true,"count":5}`)
	AssertJSONParity(t, a, a, nil)
}

// TestAssertJSONParity_IgnoresFields verifies the ignoreFields list
// suppresses differences on named keys.
func TestAssertJSONParity_IgnoresFields(t *testing.T) {
	a := []byte(`{"ok":true,"time":1.5}`)
	b := []byte(`{"ok":true,"time":9.9}`)
	AssertJSONParity(t, a, b, []string{"time"})
}

// TestDiffJSON_StructuralDifferences catches the common failure modes.
func TestDiffJSON_StructuralDifferences(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []byte
		wantDiff bool
	}{
		{"scalar_match", []byte(`5`), []byte(`5`), false},
		{"scalar_differ", []byte(`5`), []byte(`6`), true},
		{"array_length", []byte(`[1,2]`), []byte(`[1,2,3]`), true},
		{"map_extra_key", []byte(`{"a":1}`), []byte(`{"a":1,"b":2}`), true},
		{"map_missing_key", []byte(`{"a":1,"b":2}`), []byte(`{"a":1}`), true},
		{"nested_match", []byte(`{"a":[{"b":1}]}`), []byte(`{"a":[{"b":1}]}`), false},
		{"nested_differ", []byte(`{"a":[{"b":1}]}`), []byte(`{"a":[{"b":2}]}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av, bv, err := parseBoth(tc.a, tc.b)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			d := diffJSON("", av, bv, nil)
			got := d != ""
			if got != tc.wantDiff {
				t.Errorf("diff=%v (%q), want diff=%v", got, d, tc.wantDiff)
			}
		})
	}
}

// TestPythonRunner_FailsLoudlyWhenPathIsSet asserts that an
// RX_PYTHON_PATH pointing at nothing is an error, not a skip. Someone who
// set the variable asked for the parity comparison, and silently skipping
// it turns the whole harness into a no-op that reports success.
func TestPythonRunner_FailsLoudlyWhenPathIsSet(t *testing.T) {
	t.Setenv("RX_PYTHON_PATH", "/definitely/not/a/path")

	_, err := RunPythonRx(t, "--version")

	if err == nil {
		t.Fatalf("expected an error for a bad RX_PYTHON_PATH")
	}
	if IsPythonUnavailable(err) {
		t.Errorf("a set RX_PYTHON_PATH must not produce the skip sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "RX_PYTHON_PATH") {
		t.Errorf("the error should name the variable: %v", err)
	}
}

// TestPythonRunner_SkipsWhenPathIsUnsetAndTreeIsAbsent keeps the skip
// path for an environment that simply has no rx-python checkout.
func TestPythonRunner_SkipsWhenPathIsUnsetAndTreeIsAbsent(t *testing.T) {
	t.Setenv("RX_PYTHON_PATH", "")
	t.Setenv("RX_PARITY_SEARCH_ROOT", t.TempDir())

	_, err := RunPythonRx(t, "--version")

	if !IsPythonUnavailable(err) {
		t.Errorf("expected the skip sentinel, got %v", err)
	}
}

// A webhook URL in the environment of the test run reaches no binary
// RunGoRx starts: a trace that would call it on completion calls
// nothing. The same binary started with the inherited environment calls
// it once, which shows that the listener sees a call that is made.
func TestRunGoRx_CallsNoHookOfTheTestRun(t *testing.T) {
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
	args := []string{"trace", "error", FixturePath("medium.log"), "--json"}

	if err := exec.Command(BuildGoBinary(t), args...).Run(); err != nil {
		t.Fatalf("rx-go with the inherited environment: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("rx-go with the inherited environment called the hook %d times; want 1", n)
	}
	calls.Store(0)
	if _, err := RunGoRx(t, args...); err != nil {
		t.Fatalf("RunGoRx: %v", err)
	}

	if n := calls.Load(); n != 0 {
		t.Errorf("the hook of the test run received %d calls from RunGoRx; want none", n)
	}
}

// The runners pass on every variable of the test run but the webhook
// variables, rx-python's runner too.
func TestRunnerEnviron_DropsTheWebhookVariables(t *testing.T) {
	t.Setenv("RX_HOOK_ON_MATCH_URL", "https://hooks.example.invalid/match")
	t.Setenv("RX_HOOK_STRICT_IP_ONLY", "true")
	t.Setenv("RX_LARGE_FILE_MB", "7")

	env := runnerEnviron()

	for _, entry := range env {
		if strings.HasPrefix(entry, "RX_HOOK_") {
			t.Errorf("the runners pass on %s", entry)
		}
	}
	if !slices.Contains(env, "RX_LARGE_FILE_MB=7") {
		t.Error("the runners drop RX_LARGE_FILE_MB=7")
	}
}

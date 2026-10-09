package isolatedcache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// recordingRunner stands in for *testing.M: it records the cache
// directory the tests would have seen and returns a chosen exit code.
type recordingRunner struct {
	exitCode     int
	seenCacheDir string
	dirExisted   bool
}

func (r *recordingRunner) Run() int {
	r.seenCacheDir = os.Getenv(cacheDirEnv)
	info, err := os.Stat(r.seenCacheDir)
	r.dirExisted = err == nil && info.IsDir()
	return r.exitCode
}

func TestRun_TestsSeeFreshCacheDirThatIsRemovedAfterwards(t *testing.T) {
	t.Setenv(cacheDirEnv, "/should/be/replaced")
	runner := &recordingRunner{exitCode: 3}

	got := Run(runner)

	if got != 3 {
		t.Errorf("exit code = %d, want the tests' own 3", got)
	}
	if runner.seenCacheDir == "" || runner.seenCacheDir == "/should/be/replaced" {
		t.Fatalf("tests saw %s=%q, want a fresh directory", cacheDirEnv, runner.seenCacheDir)
	}
	if !runner.dirExisted {
		t.Errorf("cache directory %q did not exist while the tests ran", runner.seenCacheDir)
	}
	if _, err := os.Stat(runner.seenCacheDir); !os.IsNotExist(err) {
		t.Errorf("cache directory %q still exists after the run (stat err: %v)", runner.seenCacheDir, err)
	}
}

// environRunner stands in for *testing.M: it records the environment
// the tests would have seen.
type environRunner struct {
	seen []string
}

func (r *environRunner) Run() int {
	r.seen = os.Environ()
	return 0
}

// The tests Run runs see no hook variable of the shell that started
// `go test`, and every other variable as it was.
func TestRun_TestsSeeNoHookVariable(t *testing.T) {
	// Run sets RX_CACHE_DIR in the process; t.Setenv restores it after.
	t.Setenv(cacheDirEnv, os.Getenv(cacheDirEnv))
	t.Setenv("RX_HOOK_ON_FILE_URL", "https://hooks.example.invalid/file")
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", "https://hooks.example.invalid/complete")
	t.Setenv("RX_HOOK_STRICT_IP_ONLY", "true")
	t.Setenv("RX_LARGE_FILE_MB", "7")
	runner := &environRunner{}

	Run(runner)

	for _, entry := range runner.seen {
		if strings.HasPrefix(entry, "RX_HOOK_") {
			t.Errorf("the tests saw %s", entry)
		}
	}
	if !slices.Contains(runner.seen, "RX_LARGE_FILE_MB=7") {
		t.Error("RX_LARGE_FILE_MB=7 was removed")
	}
}

// hookCallingRunner stands in for the tests of a package that run a
// trace: it fires the trace_complete webhook the environment names, as
// rx trace does, and returns once the call was made or none was due.
type hookCallingRunner struct{}

func (hookCallingRunner) Run() int {
	urls := hooks.EffectiveHooks(hooks.HookEnvFromEnv(), hooks.HookOverrides{})
	dispatcher := hooks.NewDispatcher(hooks.DispatcherConfig{})
	dispatcher.ForRequest(urls, "isolatedcache-test").OnComplete(&rxtypes.TraceResponse{})
	// Close lets the workers drain the queue; Wait blocks until they
	// have, so a call that was due has reached the listener.
	dispatcher.Close()
	dispatcher.Wait()
	return 0
}

// A webhook URL in the environment of `go test` receives no call from
// the tests Run runs. The same runner without Run calls it once, which
// shows that the listener sees a call that is made.
func TestRun_ATestCallsNoHookOfTheShell(t *testing.T) {
	t.Setenv(cacheDirEnv, os.Getenv(cacheDirEnv))
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

	hookCallingRunner{}.Run()
	if n := calls.Load(); n != 1 {
		t.Fatalf("without Run the hook received %d calls; want 1", n)
	}
	calls.Store(0)
	Run(hookCallingRunner{})

	if n := calls.Load(); n != 0 {
		t.Errorf("the hook of the shell received %d calls from the tests Run runs; want none", n)
	}
}

// WithoutHookVariables drops the hook variables and keeps every other
// entry, in order.
func TestWithoutHookVariables_KeepsEveryOtherVariableInOrder(t *testing.T) {
	env := []string{"PATH=/bin", "RX_HOOK_ON_MATCH_URL=https://hooks.example.invalid/m", "RX_LARGE_FILE_MB=7",
		"RX_HOOK_STRICT_IP_ONLY=true", "RX_HOOKS=kept", "HOME=/home/x"}

	got := WithoutHookVariables(env)

	if want := []string{"PATH=/bin", "RX_LARGE_FILE_MB=7", "RX_HOOKS=kept", "HOME=/home/x"}; !slices.Equal(got, want) {
		t.Errorf("WithoutHookVariables = %q, want %q", got, want)
	}
}

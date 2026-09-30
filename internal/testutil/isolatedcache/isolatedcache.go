// Package isolatedcache gives the tests of one package their own rx cache
// directory, so no test reads or writes the user's real cache.
//
// rx resolves its cache as $RX_CACHE_DIR/rx, then $XDG_CACHE_HOME/rx,
// then $HOME/.cache/rx (internal/config.GetCacheBase). A test that builds
// an index or a trace cache without setting RX_CACHE_DIR itself would
// otherwise use the developer's real cache: its result could depend on a
// stale entry left by an earlier run, and every run would leave files
// behind. Each package whose code can reach the cache calls Main from its
// TestMain; scripts/test-isolated-home.sh fails the gate when one does not.
//
// Main also keeps the webhook variables of the developer's shell
// (RX_HOOK_*) from the tests, so a plain `go test` outside the just
// recipes sends no test's paths and matches to a hook.
package isolatedcache

import (
	"fmt"
	"os"
	"strings"
)

// cacheDirEnv is the variable rx reads first when it resolves its cache.
const cacheDirEnv = "RX_CACHE_DIR"

// hookEnvPrefix starts the name of every variable that makes rx send
// data to a webhook: the URLs (RX_HOOK_ON_FILE_URL, RX_HOOK_ON_MATCH_URL,
// RX_HOOK_ON_COMPLETE_URL) and the other hook settings.
const hookEnvPrefix = "RX_HOOK_"

// Runner is what Main needs from *testing.M: something that runs the
// tests and returns the process exit code. An interface rather than the
// concrete type lets this package's own test pass a fake.
type Runner interface {
	Run() int
}

// Main runs a package's tests with RX_CACHE_DIR set to a fresh temporary
// directory and without the hook variables (see Run), removes that
// directory afterwards and exits the process with the tests' exit code.
// Call it as the whole body of the package's TestMain:
//
//	func TestMain(m *testing.M) { isolatedcache.Main(m) }
//
// The variable is set in the process environment, so a binary that a test
// starts with exec.Command inherits it as long as the test leaves cmd.Env
// nil or builds it from os.Environ(). A test that needs another cache
// directory still sets its own with t.Setenv, which restores this one when
// the test ends.
func Main(m Runner) {
	// os.Exit skips deferred calls, so the work that needs a defer (the
	// cleanup) lives in Run and only the exit happens here.
	os.Exit(Run(m))
}

// Run does what Main does except exit: it returns the tests' exit code,
// or 1 when the temporary directory cannot be prepared or a hook
// variable cannot be removed.
//
// Run also removes every hook variable from the process environment
// before the tests run. The variables come from the shell that runs
// `go test`, and code under test reads them live (a trace run, a trace
// request): left in place, a developer's webhook would receive the
// paths and matches of every test that runs a trace. Removing them here
// covers the tests of each package whose TestMain calls Main, and every
// binary those tests start with the inherited environment. A test that
// needs a hook sets its own with t.Setenv.
func Run(m Runner) int {
	dir, err := os.MkdirTemp("", "rx-test-cache-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "isolatedcache: create the test cache directory: %v\n", err)
		return 1
	}
	// Runs after m.Run returns, whatever the tests did to the directory.
	defer func() { _ = os.RemoveAll(dir) }()

	if err := os.Setenv(cacheDirEnv, dir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "isolatedcache: set %s: %v\n", cacheDirEnv, err)
		return 1
	}
	if err := unsetHookVariables(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "isolatedcache: %v\n", err)
		return 1
	}
	return m.Run()
}

// unsetHookVariables removes every hook variable from the process
// environment.
func unsetHookVariables() error {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !isHookVariable(name) {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("unset %s: %w", name, err)
		}
	}
	return nil
}

// WithoutHookVariables returns env, NAME=value entries as os.Environ
// gives them, without the hook variables: the environment for a binary
// a test starts, whichever package the test is in.
func WithoutHookVariables(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !isHookVariable(name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// isHookVariable reports whether name is a hook variable.
func isHookVariable(name string) bool {
	return strings.HasPrefix(name, hookEnvPrefix)
}

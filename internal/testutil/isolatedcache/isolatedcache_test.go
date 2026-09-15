package isolatedcache

import (
	"os"
	"testing"
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

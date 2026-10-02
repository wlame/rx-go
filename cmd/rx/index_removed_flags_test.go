package main

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// `rx index --analyze` and `--analyze-window-lines` were replaced by
// signatures. The flags stay as hidden ones so an old script gets the
// replacement on stderr and exit 2, a usage error, rather than "unknown
// flag"; nothing is indexed and nothing reaches stdout.
func TestIndexRemovedAnalyzeFlagsExitWithTheirReplacement(t *testing.T) {
	const (
		analyzeMessage = "Error: --analyze was replaced by: rx trace --signatures PATH\n"
		windowMessage  = "Error: --analyze-window-lines was removed with --analyze, " +
			"which was replaced by: rx trace --signatures PATH\n"
	)
	_, path := writeLog(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"analyze", []string{"index", "--analyze", path}, analyzeMessage},
		{"analyze shorthand", []string{"index", "-a", path}, analyzeMessage},
		{"analyze set to false", []string{"index", "--analyze=false", path}, analyzeMessage},
		{"analyze with json", []string{"index", "--json", "--analyze", path}, analyzeMessage},
		{"analyze without a path", []string{"index", "--analyze"}, analyzeMessage},
		{"window", []string{"index", "--analyze-window-lines=5", path}, windowMessage},
		{"window with any value", []string{"index", "--analyze-window-lines=wide", path}, windowMessage},
		{"both flags", []string{"index", "--analyze-window-lines=5", "--analyze", path}, analyzeMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()

			code, stdout, stderr := runRxEnv(t, []string{"RX_CACHE_DIR=" + cache}, tc.args...)

			if code != 2 {
				t.Errorf("exit code %d, want 2 (stderr: %s)", code, stderr)
			}
			if stderr != tc.want {
				t.Errorf("stderr %q, want %q", stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout %q, want nothing", stdout)
			}
			if stored := filesUnder(t, cache); len(stored) > 0 {
				t.Errorf("the cache holds %v, want no index", stored)
			}
		})
	}
}

// `rx index --help` offers neither removed flag.
func TestIndexHelpListsNoRemovedAnalyzeFlag(t *testing.T) {
	code, help, stderr := runRx(t, "index", "--help")
	if code != 0 {
		t.Fatalf("rx index --help exited %d: %s", code, stderr)
	}
	for _, flag := range []string{"--analyze", "-a,"} {
		if strings.Contains(help, flag) {
			t.Errorf("rx index --help lists %s:\n%s", flag, help)
		}
	}
}

// filesUnder returns every regular file under dir, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

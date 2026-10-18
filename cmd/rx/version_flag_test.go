package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The version is printed by --version wherever it stands among the root
// flags; -v is not offered (ripgrep's -v inverts a match, and a version
// in its place would be a silent wrong answer), so `rx -v` is a usage
// error, as `rx version` is.
func TestExitCode_VersionFlag(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	if err := os.WriteFile(a, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		args     []string
		code     int
		stdout   string
		noStdout bool
	}{
		{"version", []string{"--version"}, 0, "rx version ", false},
		{"hidden then version", []string{"--hidden", "--version"}, 0, "rx version ", false},
		{"search-root equals then version", []string{"--search-root=" + dir, "--version"}, 0, "rx version ", false},
		{"search-root value then version", []string{"--search-root", dir, "--version"}, 0, "rx version ", false},
		{"short v", []string{"-v"}, 2, "", true},
		{"short v then a search", []string{"-v", "error", a}, 2, "", true},
		{"version word", []string{"version"}, 2, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runRx(t, tc.args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d (stderr %q)", code, tc.code, stderr)
			}
			if tc.noStdout && stdout != "" {
				t.Errorf("stdout %q, want nothing", stdout)
			}
			if tc.stdout != "" && !strings.HasPrefix(stdout, tc.stdout) {
				t.Errorf("stdout %q, want a prefix %q", stdout, tc.stdout)
			}
		})
	}
	_, stdout, _ := runRx(t, "--version")
	if lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "rx version ") || len(strings.Fields(lines[0])) != 3 {
		t.Errorf("--version printed %q, want exactly one line `rx version <tag>`", stdout)
	}
}

// The root help lists --version only: no -v.
func TestHelp_ListsVersionWithoutAShorthand(t *testing.T) {
	code, stdout, _ := runRx(t, "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "--version") || strings.Contains(stdout, "-v, --version") {
		t.Errorf("help lists the version flag as %q, want --version without -v", stdout)
	}
}

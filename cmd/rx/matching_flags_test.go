package main

// rx trace accepts ripgrep's matching flags (-i, -w, -x, -F, -P) and its
// answer has to be ripgrep's answer for the same flags. These tests run
// the built binary, because the bug they guard against lived in argument
// parsing: a flag that never reached rg, and the argument after it taken
// as the flag's value.

import (
	"compress/gzip"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
)

// flagFixture names its own line numbers in the comments below, so a
// wrong answer is readable straight from a failure message.
const flagFixture = "ERROR one\n" + // 1
	"error two\n" + // 2
	"Error three\n" + // 3
	"errors four\n" + // 4
	"an error\n" + // 5
	"foo( bar\n" + // 6
	"ab\n" + // 7
	"a\n" + // 8
	"error twofold\n" // 9

// traceAnswer is the part of a trace response these tests compare.
type traceAnswer struct {
	Path    []string `json:"path"`
	Matches []struct {
		AbsoluteLineNumber int `json:"absolute_line_number"`
	} `json:"matches"`
	FileChunks map[string]int `json:"file_chunks"`
}

// lines returns the distinct matched line numbers in ascending order.
func (a traceAnswer) lines() []int {
	out := []int{}
	for _, m := range a.Matches {
		if !slices.Contains(out, m.AbsoluteLineNumber) {
			out = append(out, m.AbsoluteLineNumber)
		}
	}
	slices.Sort(out)
	return out
}

func writeFlagFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "case.log")
	if err := os.WriteFile(path, []byte(flagFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// runRxIn runs the binary in dir with extra environment variables and an
// isolated cache directory, and returns exit code, stdout and stderr.
func runRxIn(t *testing.T, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(rxBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RX_CACHE_DIR="+t.TempDir())
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			t.Fatalf("run rx %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// traceJSON runs `rx <args> --json` and decodes the answer.
func traceJSON(t *testing.T, dir string, env []string, args ...string) traceAnswer {
	t.Helper()
	code, stdout, stderr := runRxIn(t, dir, env, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("rx %v exited %d: %s", args, code, stderr)
	}
	var ans traceAnswer
	if err := json.Unmarshal([]byte(stdout), &ans); err != nil {
		t.Fatalf("rx %v: decode JSON: %v\n%s", args, err, stdout)
	}
	return ans
}

// rgLines asks ripgrep itself which lines match, ignoring any user
// config, so the expected answer is ripgrep's and not a hand-written one.
func rgLines(t *testing.T, path string, args ...string) []int {
	t.Helper()
	full := append([]string{"--no-config", "--line-number", "--no-filename"}, args...)
	out, err := exec.Command("rg", append(full, path)...).Output()
	if err != nil && len(out) > 0 {
		t.Fatalf("rg %v: %v", args, err)
	}
	lines := []int{}
	for _, row := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		number, _, ok := strings.Cut(row, ":")
		if !ok {
			continue
		}
		n, convErr := strconv.Atoi(number)
		if convErr != nil {
			t.Fatalf("rg output row %q: %v", row, convErr)
		}
		lines = append(lines, n)
	}
	return lines
}

func requirePCRE2(t *testing.T) {
	t.Helper()
	if err := exec.Command("rg", "--pcre2-version").Run(); err != nil {
		t.Skip("this ripgrep is built without PCRE2")
	}
}

func TestTraceMatchingFlag_IgnoreCaseInAnyPositionSearchesTheNamedFile(t *testing.T) {
	path := writeFlagFixture(t)
	dir := filepath.Dir(path)
	want := []int{1, 2, 3, 4, 5, 9}

	argLists := [][]string{
		{"trace", "-i", "error", path},
		{"trace", "error", "-i", path},
		{"trace", "error", path, "-i"},
		{"trace", "--ignore-case", "error", path},
		{"-i", "error", path},
	}
	for _, args := range argLists {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			ans := traceJSON(t, dir, nil, append(args, "--no-cache")...)

			if !slices.Equal(ans.Path, []string{path}) {
				t.Errorf("path = %v, want [%s]", ans.Path, path)
			}
			if got := ans.lines(); !slices.Equal(got, want) {
				t.Errorf("lines = %v, want %v", got, want)
			}
		})
	}
}

func TestTraceMatchingFlag_AnswerEqualsRipgrep(t *testing.T) {
	path := writeFlagFixture(t)
	dir := filepath.Dir(path)

	cases := []struct {
		name     string
		flags    []string
		patterns []string
		needPCRE bool
	}{
		{"word", []string{"-w"}, []string{"error"}, false},
		{"word, longer alternative", []string{"--word-regexp"}, []string{"a|ab"}, false},
		{"line", []string{"-x"}, []string{"a|ab"}, false},
		{"fixed string not valid as regex", []string{"-F"}, []string{"foo("}, false},
		{"fixed string with a dot", []string{"--fixed-strings"}, []string{"r.r"}, false},
		{"pcre2 look-ahead", []string{"-P"}, []string{"error(?= two)"}, true},
		{"pcre2 next to a plain pattern", []string{"--pcre2"}, []string{"one$", "(?<=an )error"}, true},
		{"ignore case and word together", []string{"-i", "-w"}, []string{"error"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needPCRE {
				requirePCRE2(t)
			}
			rxArgs := append([]string{"trace"}, tc.flags...)
			rgArgs := append([]string{}, tc.flags...)
			for _, p := range tc.patterns {
				rxArgs = append(rxArgs, "-e", p)
				rgArgs = append(rgArgs, "-e", p)
			}
			want := rgLines(t, path, rgArgs...)

			ans := traceJSON(t, dir, nil, append(rxArgs, path, "--no-cache")...)

			if got := ans.lines(); !slices.Equal(got, want) {
				t.Errorf("rx lines = %v, rg lines = %v", got, want)
			}
		})
	}
}

func TestTraceMatchingFlag_SameAnswerForEveryStorage(t *testing.T) {
	plain := writeFlagFixture(t)
	dir := filepath.Dir(plain)

	gz := plain + ".gz"
	gzFile, err := os.Create(gz)
	if err != nil {
		t.Fatalf("create gz: %v", err)
	}
	w := gzip.NewWriter(gzFile)
	if _, err := w.Write([]byte(flagFixture)); err != nil {
		t.Fatalf("write gz: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close gz writer: %v", err)
	}
	if err := gzFile.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	zst := plain + ".zst"
	if code, _, stderr := runRxIn(t, dir, nil, "compress", plain, "--output="+zst); code != 0 {
		t.Fatalf("rx compress exited %d: %s", code, stderr)
	}
	want := []int{1, 2, 3, 4, 5, 9}

	for _, path := range []string{plain, gz, zst} {
		t.Run(filepath.Ext(path), func(t *testing.T) {
			ans := traceJSON(t, dir, nil, "trace", "-i", "error", path, "--no-cache")

			if got := ans.lines(); !slices.Equal(got, want) {
				t.Errorf("lines = %v, want %v", got, want)
			}
		})
	}
}

func TestTraceMatchingFlag_CacheIsKeyedByTheFlags(t *testing.T) {
	path := writeFlagFixture(t)
	dir := filepath.Dir(path)
	cacheDir := t.TempDir()
	// Every file counts as large, so every completed scan is cached; the
	// cache directory is shared by the three runs below.
	env := []string{"RX_LARGE_FILE_MB=0", "RX_CACHE_DIR=" + cacheDir}

	caseSensitive := traceJSON(t, dir, env, "trace", "error", path)
	firstIgnoreCase := traceJSON(t, dir, env, "trace", "-i", "error", path)
	written := traceCacheFiles(t, cacheDir)
	cachedIgnoreCase := traceJSON(t, dir, env, "trace", "-i", "error", path)

	if got, want := caseSensitive.lines(), []int{2, 4, 5, 9}; !slices.Equal(got, want) {
		t.Errorf("case-sensitive lines = %v, want %v", got, want)
	}
	if got, want := firstIgnoreCase.lines(), []int{1, 2, 3, 4, 5, 9}; !slices.Equal(got, want) {
		t.Errorf("first -i lines = %v, want %v", got, want)
	}
	traceanswer.RequireSame(t, "cached -i", cachedIgnoreCase, firstIgnoreCase)
	if !maps.Equal(traceCacheFiles(t, cacheDir), written) {
		t.Error("the third run scanned the file instead of reading the cache")
	}
}

func TestTraceMatchingFlag_UnknownFlagIsAUsageError(t *testing.T) {
	path := writeFlagFixture(t)

	code, stdout, stderr := runRxIn(t, filepath.Dir(path), nil, "trace", "--frobnicate", "error", path)

	if code != 2 {
		t.Errorf("exit code = %d, want 2; stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Errorf("stderr does not name the flag: %q", stderr)
	}
}

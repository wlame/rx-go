package main

import (
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/internal/webapi"
)

// The CLI and an rx server share the trace cache. An entry either one
// writes answers the other, with the patterns in any order, exactly as
// the reader's own scan does: each match carries the reader's ID for the
// pattern it matched.

// sharedCacheFixture is a log under a search root, a cache directory,
// an in-process rx server over both, and the environment that points
// `rx trace` at the same cache.
type sharedCacheFixture struct {
	logPath  string
	baseURL  string
	cacheDir string
	env      []string
}

// startSharedCacheFixture writes a log of about 3 MB whose lines carry
// WARN, NEEDLE and a.b tags, some of them two or three at once, and
// serves it. The path is canonical, so the CLI and the server key the
// cache on the same path.
func startSharedCacheFixture(t *testing.T) sharedCacheFixture {
	t.Helper()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	var b strings.Builder
	for line := 1; b.Len() < 3<<20; line++ {
		fmt.Fprintf(&b, "LINE %d %s", line, strings.Repeat("x", 60))
		for _, tag := range []struct {
			every int
			text  string
		}{{30, "WARN"}, {50, "NEEDLE"}, {70, "needle"}, {110, "a.b"}} {
			if line%tag.every == 0 {
				b.WriteString(" " + tag.text)
			}
		}
		b.WriteByte('\n')
	}
	logPath := filepath.Join(root, "app.log")
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	cacheDir := t.TempDir()
	settings := map[string]string{
		"RX_CACHE_DIR": cacheDir, "RX_LARGE_FILE_MB": "1",
		"RX_MIN_CHUNK_SIZE_MB": "1", "RX_MAX_SUBPROCESSES": "4",
	}
	var env []string
	for name, value := range settings {
		t.Setenv(name, value) // for the server in this process
		env = append(env, name+"="+value)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set search roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	server := httptest.NewServer(webapi.NewServer(webapi.Config{
		AppVersion:  "trace-cache-order-test",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RipgrepPath: rgPath,
		TaskManager: tasks.New(tasks.Config{}),
	}))
	t.Cleanup(server.Close)
	return sharedCacheFixture{logPath: logPath, baseURL: server.URL, cacheDir: cacheDir, env: env}
}

// httpTrace runs GET /v1/trace on the fixture's log for patterns, in
// order, with the extra query parameters.
func (f sharedCacheFixture) httpTrace(t *testing.T, patterns []string, extra url.Values) map[string]any {
	t.Helper()
	query := url.Values{"path": {f.logPath}, "regexp": patterns}
	for k, v := range extra {
		query[k] = v
	}
	routes := routerFixture{baseURL: f.baseURL}
	return routes.get(t, "/v1/trace", query)
}

// cliTrace runs `rx trace --json` on the fixture's log with -e for each
// pattern, in order, and the extra arguments.
func (f sharedCacheFixture) cliTrace(t *testing.T, patterns []string, extra ...string) map[string]any {
	t.Helper()
	var args []string
	for _, p := range patterns {
		args = append(args, "-e", p)
	}
	return traceAnswerJSON(t, f.env, append(append(args, extra...), f.logPath)...)
}

// requireCacheHit runs read and fails the test when it rewrote the
// cache, which a scan does and a hit does not.
func (f sharedCacheFixture) requireCacheHit(t *testing.T, read func() map[string]any) map[string]any {
	t.Helper()
	before := traceCacheFiles(t, f.cacheDir)
	answer := read()
	if !maps.Equal(traceCacheFiles(t, f.cacheDir), before) {
		t.Fatal("the trace scanned the file instead of reading the cache")
	}
	return answer
}

// requireEveryPatternCredited fails t unless every pattern ID of the
// answer has a match, so a label check cannot pass on an empty fixture.
func requireEveryPatternCredited(t *testing.T, answer map[string]any) {
	t.Helper()
	credited := map[string]bool{}
	for _, m := range answer["matches"].([]any) {
		credited[m.(map[string]any)["pattern"].(string)] = true
	}
	for pid := range answer["patterns"].(map[string]any) {
		if !credited[pid] {
			t.Fatalf("no match credited to %s in %v", pid, answer["patterns"])
		}
	}
}

// An entry `rx trace` writes answers an HTTP search with the patterns
// in every other order as the HTTP scan does.
func TestTraceCacheWrittenByTheCLIAnswersHTTPInAnyPatternOrder(t *testing.T) {
	f := startSharedCacheFixture(t)
	f.cliTrace(t, []string{"WARN", "NEEDLE", "a.b"})

	for _, read := range [][]string{
		{"WARN", "a.b", "NEEDLE"}, {"NEEDLE", "WARN", "a.b"}, {"NEEDLE", "a.b", "WARN"},
		{"a.b", "WARN", "NEEDLE"}, {"a.b", "NEEDLE", "WARN"},
	} {
		hit := f.requireCacheHit(t, func() map[string]any { return f.httpTrace(t, read, nil) })
		scan := f.httpTrace(t, read, url.Values{"no_cache": {"true"}})
		requireEveryPatternCredited(t, scan)
		traceanswer.RequireSame(t, fmt.Sprintf("HTTP cache hit for %q", read), hit, scan)
	}
}

// An entry an HTTP search writes answers `rx trace` with the patterns in
// the other order as the CLI's scan does, with and without -i.
func TestTraceCacheWrittenOverHTTPAnswersTheCLIInAnyPatternOrder(t *testing.T) {
	cases := []struct {
		name     string
		param    url.Values
		flag     []string
		patterns []string
	}{
		{name: "case-sensitive", patterns: []string{"NEEDLE", "WARN"}},
		{name: "ignore case", param: url.Values{"ignore_case": {"true"}}, flag: []string{"-i"}, patterns: []string{"needle", "WARN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startSharedCacheFixture(t)
			f.httpTrace(t, tc.patterns, tc.param)

			read := []string{tc.patterns[1], tc.patterns[0]}
			hit := f.requireCacheHit(t, func() map[string]any { return f.cliTrace(t, read, tc.flag...) })
			scan := f.cliTrace(t, read, append(tc.flag, "--no-cache")...)
			requireEveryPatternCredited(t, scan)
			traceanswer.RequireSame(t, fmt.Sprintf("CLI cache hit for %q", read), hit, scan)
		})
	}
}

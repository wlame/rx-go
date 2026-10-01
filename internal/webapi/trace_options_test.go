package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// numberedLines returns count lines that each name their own number,
// with NEEDLE on the lines in needles.
func numberedLines(count int, needles ...int) string {
	var text strings.Builder
	for line := 1; line <= count; line++ {
		word := "filler"
		if slices.Contains(needles, line) {
			word = "NEEDLE"
		}
		fmt.Fprintf(&text, "LINE %06d %s text to make the line longer\n", line, word)
	}
	return text.String()
}

// windowLines lists the line numbers of the one context window a trace
// with a single match returns.
func windowLines(t *testing.T, body rxtypes.TraceResponse) []int {
	t.Helper()
	if len(body.ContextLines) != 1 {
		t.Fatalf("context_lines has %d windows, want 1: %v", len(body.ContextLines), body.ContextLines)
	}
	lines := []int{}
	for _, window := range body.ContextLines {
		for _, line := range window {
			lines = append(lines, line.AbsoluteLineNumber)
		}
	}
	return lines
}

// lineRange lists the numbers first..last.
func lineRange(first, last int) []int {
	lines := []int{}
	for line := first; line <= last; line++ {
		lines = append(lines, line)
	}
	return lines
}

// intOrNil renders a nullable count for a failure message.
func intOrNil(n *int) string {
	if n == nil {
		return "null"
	}
	return strconv.Itoa(*n)
}

// The three context parameters set the window the way -C, -B and -A set
// it on the command line: before_context and after_context each win over
// context, zero included, and the answer echoes the window it used.
func TestTrace_ContextParametersSetTheWindowAroundEachMatch(t *testing.T) {
	file := sandboxedFile(t, numberedLines(20, 10))
	ts := newServerWithRipgrep(t)

	cases := []struct {
		name       string
		query      url.Values
		wantLines  []int
		wantBefore string
		wantAfter  string
	}{
		{name: "no context", query: url.Values{}, wantLines: []int{10}, wantBefore: "null", wantAfter: "null"},
		{name: "context", query: url.Values{"context": {"2"}}, wantLines: lineRange(8, 12), wantBefore: "2", wantAfter: "2"},
		{name: "before_context alone", query: url.Values{"before_context": {"3"}}, wantLines: lineRange(7, 10), wantBefore: "3", wantAfter: "null"},
		{name: "after_context alone", query: url.Values{"after_context": {"1"}}, wantLines: lineRange(10, 11), wantBefore: "null", wantAfter: "1"},
		{name: "before_context zero wins over context", query: url.Values{"context": {"2"}, "before_context": {"0"}}, wantLines: lineRange(10, 12), wantBefore: "null", wantAfter: "2"},
		{name: "after_context wins over context", query: url.Values{"context": {"1"}, "after_context": {"4"}}, wantLines: lineRange(9, 14), wantBefore: "1", wantAfter: "4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := url.Values{"path": {file}, "regexp": {"NEEDLE"}}
			for key, values := range tc.query {
				query[key] = values
			}

			body := getTrace(t, ts.URL, query)

			if got := windowLines(t, body); !slices.Equal(got, tc.wantLines) {
				t.Errorf("window lines %v, want %v", got, tc.wantLines)
			}
			if got := intOrNil(body.BeforeContext); got != tc.wantBefore {
				t.Errorf("before_context %s, want %s", got, tc.wantBefore)
			}
			if got := intOrNil(body.AfterContext); got != tc.wantAfter {
				t.Errorf("after_context %s, want %s", got, tc.wantAfter)
			}
		})
	}
}

// statusOf sends GET /v1/trace with query and returns the status.
func statusOf(t *testing.T, ts *httptest.Server, query url.Values) int {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/trace?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Each context parameter takes up to MaxTraceContextLines lines per side
// and refuses one more with a 422, so one request cannot multiply the
// size of its answer without bound.
func TestTrace_ContextAboveTheCapIsRefused(t *testing.T) {
	file := sandboxedFile(t, numberedLines(20, 10))
	ts := newServerWithRipgrep(t)

	for _, param := range []string{"context", "before_context", "after_context"} {
		t.Run(param, func(t *testing.T) {
			query := url.Values{"path": {file}, "regexp": {"NEEDLE"}}

			query.Set(param, strconv.Itoa(MaxTraceContextLines))
			if status := statusOf(t, ts, query); status != http.StatusOK {
				t.Errorf("%s=%d: status %d, want 200", param, MaxTraceContextLines, status)
			}
			query.Set(param, strconv.Itoa(MaxTraceContextLines+1))
			if status := statusOf(t, ts, query); status != http.StatusUnprocessableEntity {
				t.Errorf("%s=%d: status %d, want 422", param, MaxTraceContextLines+1, status)
			}
		})
	}
}

// liveOperationParameters reads the OpenAPI document a server serves and
// returns, by operation ID, each operation's parameters by name.
func liveOperationParameters(t *testing.T) map[string]map[string]map[string]any {
	t.Helper()
	ts := httptest.NewServer(NewServer(Config{AppVersion: "params-test"}))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("fetch openapi: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name   string         `json:"name"`
				In     string         `json:"in"`
				Schema map[string]any `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	out := map[string]map[string]map[string]any{}
	for _, operations := range doc.Paths {
		for _, op := range operations {
			params := map[string]map[string]any{}
			for _, p := range op.Parameters {
				if p.In == "query" {
					params[p.Name] = p.Schema
				}
			}
			out[op.OperationID] = params
		}
	}
	return out
}

// The published contract states the cap, so a client can read it rather
// than find it with a 422.
func TestOpenAPI_TraceContextParametersDeclareTheCap(t *testing.T) {
	params := liveOperationParameters(t)["trace"]
	for _, name := range []string{"context", "before_context", "after_context"} {
		schema, ok := params[name]
		if !ok {
			t.Errorf("trace declares no %s parameter", name)
			continue
		}
		if got, _ := schema["maximum"].(float64); int(got) != MaxTraceContextLines {
			t.Errorf("%s: maximum %v, want %d", name, schema["maximum"], MaxTraceContextLines)
		}
	}
}

// Every query parameter of an operation that renders a cli_command is a
// field of that operation's row in the table BuildCLICommand reads, or
// one of the matching flags its switch list renders. A parameter added
// to a handler without a way into the equivalent command fails here.
func TestCLICommand_EveryQueryParameterHasARowInTheTable(t *testing.T) {
	matchingFlagParams := map[string]bool{}
	for _, flag := range trace.MatchingFlags {
		matchingFlagParams[flag.QueryParam()] = true
	}
	live := liveOperationParameters(t)
	for name, op := range CLICommandOperations() {
		params, ok := live[strings.ReplaceAll(name, "_", "-")]
		if !ok {
			continue
		}
		fields := map[string]bool{}
		hasSwitchList := false
		for _, arg := range op.Args {
			fields[arg.Field] = true
			hasSwitchList = hasSwitchList || arg.Kind == ArgSwitchList
		}
		for param := range params {
			if fields[param] || (hasSwitchList && matchingFlagParams[param]) {
				continue
			}
			t.Errorf("%s: query parameter %q has no row in the cli_command table", name, param)
		}
	}
}

// largeTraceFixture writes a file over a 1 MB RX_LARGE_FILE_MB, so a
// completed scan of it is written to the trace cache, with one match on
// line 30000, and isolates the cache.
func largeTraceFixture(t *testing.T) (file, cacheDir string) {
	t.Helper()
	cacheDir = t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	return sandboxedFile(t, numberedLines(40_000, 30_000)), cacheDir
}

// saveShiftedLineIndex stores a valid line index for path with every
// checkpoint's line number moved up by shift. The index still describes
// the file as far as its identity goes, so a pass that starts counting
// from it answers numbers that are wrong by exactly shift; a right
// number shows the index was not read.
func saveShiftedLineIndex(t *testing.T, path string, shift int64) {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	for i := range idx.LineIndex {
		idx.LineIndex[i].LineNumber += shift
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
}

// matchLine returns the line number of the one match of a trace.
func matchLine(t *testing.T, body rxtypes.TraceResponse) int {
	t.Helper()
	if len(body.Matches) != 1 {
		t.Fatalf("%d matches, want 1", len(body.Matches))
	}
	return body.Matches[0].AbsoluteLineNumber
}

// no_cache=true neither writes the trace cache nor reads it: the answer
// comes from a fresh scan even when the cache holds one for the request.
func TestTrace_NoCacheNeitherWritesNorReadsTheTraceCache(t *testing.T) {
	file, _ := largeTraceFixture(t)
	ts := newServerWithRipgrep(t)
	cachePath := trace.CachePath(file, []string{"NEEDLE"}, []string{})
	noCache := url.Values{"path": {file}, "regexp": {"NEEDLE"}, "no_cache": {"true"}}
	withCache := url.Values{"path": {file}, "regexp": {"NEEDLE"}}

	getTrace(t, ts.URL, noCache)
	if _, err := os.Stat(cachePath); err == nil {
		t.Fatal("no_cache=true wrote the trace cache")
	}
	getTrace(t, ts.URL, withCache)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("the default request wrote no trace cache: %v", err)
	}

	// A cache hit counts its lines from the nearest index checkpoint,
	// so with a shifted index the default answer shows it read the
	// cache, and the no_cache answer shows it did not.
	saveShiftedLineIndex(t, file, 1000)
	if got := matchLine(t, getTrace(t, ts.URL, withCache)); got != 31_000 {
		t.Fatalf("cache hit numbered the match %d, want 31000 from the shifted index", got)
	}
	if got := matchLine(t, getTrace(t, ts.URL, noCache)); got != 30_000 {
		t.Errorf("no_cache=true numbered the match %d, want 30000 from a fresh scan", got)
	}
}

// no_index=true answers without reading a line index: a cache hit counts
// its lines from the start of the file instead of from a checkpoint.
func TestTrace_NoIndexAnswersACacheHitWithoutTheIndex(t *testing.T) {
	file, _ := largeTraceFixture(t)
	ts := newServerWithRipgrep(t)
	withIndex := url.Values{"path": {file}, "regexp": {"NEEDLE"}}
	noIndex := url.Values{"path": {file}, "regexp": {"NEEDLE"}, "no_index": {"true"}}

	getTrace(t, ts.URL, withIndex) // writes the trace cache
	saveShiftedLineIndex(t, file, 1000)

	if got := matchLine(t, getTrace(t, ts.URL, withIndex)); got != 31_000 {
		t.Fatalf("default numbered the match %d, want 31000 from the shifted index", got)
	}
	if got := matchLine(t, getTrace(t, ts.URL, noIndex)); got != 30_000 {
		t.Errorf("no_index=true numbered the match %d, want 30000 counted from the file", got)
	}
}

// no_recursive=true searches only the files directly inside a directory,
// as --no-recursive does; by default the walk goes into subdirectories.
func TestTrace_NoRecursiveSearchesOnlyTheTopLevelOfADirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	top := filepath.Join(root, "top.log")
	nested := filepath.Join(root, "sub", "nested.log")
	for _, file := range []string{top, nested} {
		if err := os.WriteFile(file, []byte("NEEDLE\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newServerWithRipgrep(t)

	searched := func(query url.Values) []string {
		files := []string{}
		for _, file := range getTrace(t, ts.URL, query).Files {
			files = append(files, filepath.Base(file))
		}
		slices.Sort(files)
		return files
	}
	recursive := url.Values{"path": {root}, "regexp": {"NEEDLE"}}
	topLevel := url.Values{"path": {root}, "regexp": {"NEEDLE"}, "no_recursive": {"true"}}

	if got := searched(recursive); !slices.Equal(got, []string{"nested.log", "top.log"}) {
		t.Errorf("default searched %v, want both files", got)
	}
	if got := searched(topLevel); !slices.Equal(got, []string{"top.log"}) {
		t.Errorf("no_recursive=true searched %v, want top.log alone", got)
	}
}

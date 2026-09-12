package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// matchingFlagsFixture holds one line per behavior the flags change. The
// line numbers in the cases below are positions in this text.
const matchingFlagsFixture = "error\n" + // 1
	"ERROR here\n" + // 2
	"errors\n" + // 3
	"an error here\n" + // 4
	"a.b\n" + // 5
	"axb\n" // 6

// getTrace runs GET /v1/trace with query and decodes a 200 body, failing
// the test on any other status.
func getTrace(t *testing.T, ts string, query url.Values) rxtypes.TraceResponse {
	t.Helper()
	resp, err := http.Get(ts + "/v1/trace?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("status %d, want 200 (body: %v)", resp.StatusCode, body)
	}
	var body rxtypes.TraceResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// matchedLines lists the line numbers a trace response reports, in order.
func matchedLines(body rxtypes.TraceResponse) []int {
	lines := make([]int, 0, len(body.Matches))
	for _, m := range body.Matches {
		lines = append(lines, m.AbsoluteLineNumber)
	}
	return lines
}

// sandboxedFile writes content into a fresh search root and returns its path.
func sandboxedFile(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "app.log")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return file
}

// Each query parameter selects the ripgrep flag of the same name, so the
// lines an HTTP search returns are the lines `rx trace` returns with that
// flag on the command line.
func TestTrace_MatchingFlagParametersChangeWhichLinesMatch(t *testing.T) {
	file := sandboxedFile(t, matchingFlagsFixture)
	ts := newServerWithRipgrep(t)

	cases := []struct {
		name    string
		pattern string
		param   string
		want    []int
	}{
		{name: "no flag is case-sensitive", pattern: "error", want: []int{1, 3, 4}},
		{name: "ignore_case", pattern: "error", param: "ignore_case", want: []int{1, 2, 3, 4}},
		{name: "word_regexp", pattern: "error", param: "word_regexp", want: []int{1, 4}},
		{name: "line_regexp", pattern: "error", param: "line_regexp", want: []int{1}},
		{name: "no flag reads a dot as any character", pattern: "a.b", want: []int{5, 6}},
		{name: "fixed_strings", pattern: "a.b", param: "fixed_strings", want: []int{5}},
		{name: "pcre2", pattern: "error(?= here)", param: "pcre2", want: []int{4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := url.Values{"path": {file}, "regexp": {tc.pattern}}
			if tc.param != "" {
				query.Set(tc.param, "true")
			}

			got := matchedLines(getTrace(t, ts.URL, query))

			if !slices.Equal(got, tc.want) {
				t.Errorf("lines %v, want %v", got, tc.want)
			}
		})
	}
}

// Every flag in the shared table has a query parameter, and the response's
// cli_command shows it the way the CLI takes it. A flag added to the table
// without a parameter fails here.
func TestTrace_EveryMatchingFlagHasAParameterShownInTheCLICommand(t *testing.T) {
	file := sandboxedFile(t, matchingFlagsFixture)
	ts := newServerWithRipgrep(t)

	for _, flag := range trace.MatchingFlags {
		t.Run(flag.QueryParam(), func(t *testing.T) {
			query := url.Values{"path": {file}, "regexp": {"error"}, flag.QueryParam(): {"true"}}

			body := getTrace(t, ts.URL, query)

			if body.CLICommand == nil {
				t.Fatal("cli_command is null")
			}
			if !strings.Contains(*body.CLICommand, " --"+flag.Long) {
				t.Errorf("cli_command %q does not carry --%s", *body.CLICommand, flag.Long)
			}
		})
	}
}

// A pattern that needs PCRE2 is refused by ripgrep's default engine; the
// parameter is what makes it valid, so without it the caller gets the
// same 400 any other uncompilable pattern gets.
func TestTrace_LookAroundWithoutPCRE2IsAnInvalidPattern(t *testing.T) {
	file := sandboxedFile(t, matchingFlagsFixture)
	ts := newServerWithRipgrep(t)

	query := url.Values{"path": {file}, "regexp": {"error(?= here)"}}
	resp, err := http.Get(ts.URL + "/v1/trace?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

// The trace cache is keyed by the flags as well as the patterns, so a
// cached case-insensitive answer is never served to a case-sensitive
// request for the same pattern, or the other way round.
func TestTrace_CacheKeepsAnswersForDifferentFlagsApart(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_LARGE_FILE_MB", "1")

	// Over the 1 MB threshold, so a completed scan is cached. The filler
	// holds no "error" in any case, so only the fixture lines can match.
	filler := strings.Repeat("filler line without the word\n", 40_000)
	file := sandboxedFile(t, matchingFlagsFixture+filler)
	ts := newServerWithRipgrep(t)

	sensitive := url.Values{"path": {file}, "regexp": {"error"}}
	insensitive := url.Values{"path": {file}, "regexp": {"error"}, "ignore_case": {"true"}}

	// The first round writes one cache entry per flag set; the second
	// reads them back.
	for round := 1; round <= 2; round++ {
		if got := matchedLines(getTrace(t, ts.URL, insensitive)); !slices.Equal(got, []int{1, 2, 3, 4}) {
			t.Errorf("round %d, ignore_case: lines %v, want [1 2 3 4]", round, got)
		}
		if got := matchedLines(getTrace(t, ts.URL, sensitive)); !slices.Equal(got, []int{1, 3, 4}) {
			t.Errorf("round %d, no flag: lines %v, want [1 3 4]", round, got)
		}
	}

	for _, flags := range [][]string{{"-i"}, {}} {
		cachePath := trace.CachePath(file, []string{"error"}, flags)
		if _, err := os.Stat(cachePath); err != nil {
			t.Errorf("no cache entry for flags %q: %v", flags, err)
		}
	}
}

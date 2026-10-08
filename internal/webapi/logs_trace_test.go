package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// getLogTrace asks GET /v1/logs/trace with query and returns the status
// and the body.
func getLogTrace(t *testing.T, base string, query url.Values) (int, []byte) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	resp, err := http.Get(base + "/v1/logs/trace?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// decodeChainTrace decodes a chain search answer.
func decodeChainTrace(t *testing.T, raw []byte) rxtypes.ChainTraceResponse {
	t.Helper()
	var resp rxtypes.ChainTraceResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return resp
}

// withoutRun is a chain search answer without the fields that name the
// run rather than the answer, to compare two answers.
func withoutRun(resp rxtypes.ChainTraceResponse) rxtypes.ChainTraceResponse {
	resp.RequestID, resp.Time, resp.CLICommand = "", 0, nil
	return resp
}

// The route answers what the chain search answers for the same paths
// (logchain.Search, as over HTTP: no scan), with its cli_command. Over
// HTTP no index is built: the chain is pending, searched in its
// provisional order, and every chain_line is -1 until its frozen parts
// are indexed; then each match has its line in the chain. The route
// starts no index task.
func TestLogTrace_AnswersAsTheChainSearchAndNumbersOnceIndexed(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	manager := tasks.New(tasks.Config{})
	ts := httptest.NewServer(NewServer(Config{AppVersion: "unit-test", RipgrepPath: "rg", TaskManager: manager}))
	t.Cleanup(ts.Close)
	query := url.Values{"path": {filepath.Join(root, "app.log")}, "regexp": {`LINE [0-9]*5$`}}

	status, raw := getLogTrace(t, ts.URL, query)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	got := decodeChainTrace(t, raw)
	want, err := logchain.Search(context.Background(), trace.New(), logchain.SearchRequest{
		Paths: []string{filepath.Join(root, "app.log")}, Patterns: []string{`LINE [0-9]*5$`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := jsonText(t, withoutRun(got)), jsonText(t, withoutRun(*want.Answer)); a != b {
		t.Fatalf("the route gives\n%s\nthe chain search\n%s", a, b)
	}
	if got.CLICommand == nil || *got.CLICommand != "rx logs trace "+filepath.Join(root, "app.log")+" --regexp='LINE [0-9]*5$'" {
		t.Fatalf("cli_command %v", got.CLICommand)
	}
	if ref := got.Chains["c1"]; ref.State != rxtypes.ChainStatePending || len(got.Matches) != 4 {
		t.Fatalf("chain %+v, %d matches", ref, len(got.Matches))
	}
	for _, m := range got.Matches {
		if m.ChainLine != -1 || m.AbsoluteLineNumber < 1 {
			t.Fatalf("pending match %+v", m)
		}
	}
	if started := manager.List(); len(started) != 0 {
		t.Fatalf("the search started %d tasks", len(started))
	}

	for _, name := range []string{"app.log.2", "app.log.1"} {
		built, err := index.Build(filepath.Join(root, name), index.BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := index.Save(built); err != nil {
			t.Fatal(err)
		}
	}
	status, raw = getLogTrace(t, ts.URL, query)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	ready := decodeChainTrace(t, raw)
	if ready.Chains["c1"].State != rxtypes.ChainStateReady {
		t.Fatalf("state %s after indexing", ready.Chains["c1"].State)
	}
	// LINE 5, 15, 25, 35: the fixture's lines name their global number.
	for i, m := range ready.Matches {
		if want := int64(10*i + 5); m.ChainLine != want {
			t.Fatalf("match %d (%s): chain_line %d, want %d", i, *m.LineText, m.ChainLine, want)
		}
	}
}

// GET /v1/trace keeps its own answer for a directory of rotated files:
// no chains, and no chain fields on its matches.
func TestTrace_ADirectoryOfRotatedFilesHasNoChainFields(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/trace?" + url.Values{"path": {root}, "regexp": {"LINE 1$"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["chains"]; ok || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %v", resp.StatusCode, body)
	}
	for _, m := range body["matches"].([]any) {
		match := m.(map[string]any)
		if _, ok := match["chain"]; ok {
			t.Fatalf("a /v1/trace match has a chain: %v", match)
		}
		if _, ok := match["chain_line"]; ok {
			t.Fatalf("a /v1/trace match has a chain_line: %v", match)
		}
	}
}

// The statuses: 404 for a path that is no directory, chain or file, 403
// outside the roots and into a hidden entry, 400 for a pattern that
// does not compile and for a match webhook without max_results, 422
// without path or regexp.
func TestLogTrace_Statuses(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	ts := newTestServer(t)
	cases := []struct {
		label string
		query url.Values
		want  int
	}{
		{"a directory", url.Values{"path": {root}, "regexp": {"LINE"}}, http.StatusOK},
		{"a missing path", url.Values{"path": {filepath.Join(root, "nope.log")}, "regexp": {"LINE"}}, http.StatusNotFound},
		{"outside the roots", url.Values{"path": {"/etc/syslog"}, "regexp": {"LINE"}}, http.StatusForbidden},
		{"a hidden directory", url.Values{"path": {filepath.Join(root, ".hidden")}, "regexp": {"LINE"}}, http.StatusForbidden},
		{"a pattern that does not compile", url.Values{"path": {root}, "regexp": {"(unclosed"}}, http.StatusBadRequest},
		{"a match webhook without a cap", url.Values{"path": {root}, "regexp": {"LINE"},
			"hook_on_match": {"https://hooks.example.com/x"}}, http.StatusBadRequest},
		{"no path", url.Values{"regexp": {"LINE"}}, http.StatusUnprocessableEntity},
		{"no pattern", url.Values{"path": {root}}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		if status, raw := getLogTrace(t, ts.URL, tc.query); status != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.label, status, tc.want, raw)
		}
	}
}

// The errors a chain search returns map to the statuses GET /v1/trace
// gives the same failures, and a part that changed while its chain was
// described to 409.
func TestLogTraceError_Statuses(t *testing.T) {
	cases := []struct {
		label string
		err   error
		want  int
	}{
		{"a part changed", fmt.Errorf("%w: %w", logchain.ErrPartChanged, paths.ErrFileChanged), http.StatusConflict},
		{"a bad pattern", fmt.Errorf("%w: unclosed group", trace.ErrInvalidPattern), http.StatusBadRequest},
		{"a missing path", &logchain.SearchPathError{Path: "/x", Err: fmt.Errorf("pin: %w", fs.ErrNotExist)}, http.StatusNotFound},
		{"outside the roots", &logchain.SearchPathError{Path: "/x", Err: &paths.ErrPathOutsideRoots{Path: "/x"}}, http.StatusForbidden},
		{"a hidden path", &logchain.SearchPathError{Path: "/x", Err: &paths.ErrHiddenPath{Path: "/x", Component: ".x"}}, http.StatusForbidden},
		{"a file that cannot be opened", &logchain.SearchPathError{Path: "/x", Err: fs.ErrPermission}, http.StatusForbidden},
		{"anything else", errors.New("rg died"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := logTraceError(tc.err).GetStatus(); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.label, got, tc.want)
		}
	}
}

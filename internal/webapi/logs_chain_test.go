package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// describedChainRoot makes a search root holding the chain `app.log`
// (a frozen part `app.log.2`, a frozen part `app.log.1` and the active
// file, every line timestamped, none indexed), the invalid chain
// `bad.log` (a frozen part without timestamps), a lone `single.log` and
// a hidden directory, and installs it.
func describedChainRoot(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	lines := func(at time.Time, first, n int) []byte {
		var buf bytes.Buffer
		for i := 0; i < n; i++ {
			fmt.Fprintf(&buf, "%s LINE %d\n", at.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05.000"), first+i)
		}
		return buf.Bytes()
	}
	files := map[string][]byte{
		"app.log.2":  lines(start, 1, 20),
		"app.log.1":  lines(start.Add(time.Hour), 21, 10),
		"app.log":    lines(start.Add(2*time.Hour), 31, 5),
		"bad.log.1":  []byte("no time here\n"),
		"bad.log":    lines(start, 1, 2),
		"single.log": lines(start, 1, 2),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	return root
}

// getChain asks GET /v1/logs/chain with query and returns the status
// and the body.
func getChain(t *testing.T, base string, query url.Values) (int, []byte) {
	t.Helper()
	resp, err := http.Get(base + "/v1/logs/chain?" + query.Encode())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	return resp.StatusCode, body.Bytes()
}

// decodeChain decodes a description.
func decodeChain(t *testing.T, raw []byte) rxtypes.ChainResponse {
	t.Helper()
	var resp rxtypes.ChainResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return resp
}

// storePartIndexes builds and stores the line index of each named file
// of dir, as `rx index` does.
func storePartIndexes(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		built, err := index.Build(filepath.Join(dir, name), index.BuildOptions{})
		if err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		if _, err := index.Save(built); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
}

// Over HTTP a frozen part without an index is not read: the chain is
// pending, without global numbers. Once its parts are indexed it is
// ready, and the description is what `rx logs show` reads by scanning
// the parts (logchain.Options.Scan), field by field, except is_indexed
// and cli_command, which say how the answer was made, and the active
// file's count, which the scan reads and the request does not.
func TestLogChain_PendingThenReadyAsTheCLIScanGives(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	ts := newTestServer(t)
	handle := filepath.Join(root, "app.log")

	status, raw := getChain(t, ts.URL, url.Values{"path": {handle}})
	pending := decodeChain(t, raw)
	if status != http.StatusOK || pending.State != rxtypes.ChainStatePending || pending.FrozenLineCount != nil {
		t.Fatalf("status %d: %s", status, raw)
	}
	for _, p := range pending.Parts {
		if p.GlobalStart != nil {
			t.Fatalf("%s has a global start in a pending chain", p.Name)
		}
	}
	if pending.CLICommand != "rx logs show "+handle {
		t.Fatalf("cli_command %q", pending.CLICommand)
	}

	scanned, _, err := logchain.DescribeHandle(context.Background(), handle, logchain.Options{Scan: true})
	if err != nil {
		t.Fatal(err)
	}
	storePartIndexes(t, root, "app.log.2", "app.log.1")
	status, raw = getChain(t, ts.URL, url.Values{"path": {handle}})
	ready := decodeChain(t, raw)
	if status != http.StatusOK || ready.State != rxtypes.ChainStateReady || *ready.FrozenLineCount != 30 {
		t.Fatalf("status %d: %s", status, raw)
	}
	cli := *scanned.Response
	cli.CLICommand, ready.CLICommand = "", ""
	cli.Parts = append([]rxtypes.ChainPart(nil), cli.Parts...)
	for k := range cli.Parts {
		cli.Parts[k].IsIndexed, ready.Parts[k].IsIndexed = false, false
	}
	// The scan read the active file's count and highest time; the
	// request did not (no index of it).
	active := len(cli.Parts) - 1
	cli.Parts[active].LineCount, cli.Parts[active].MaxMs, cli.LineCount = nil, nil, nil
	if a, b := jsonText(t, cli), jsonText(t, ready); a != b {
		t.Fatalf("the CLI scan and the request differ\n%s\n%s", a, b)
	}
}

// jsonText is v as indented JSON.
func jsonText(t *testing.T, v any) string {
	t.Helper()
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The statuses: 200 for every state, 409 with the current description
// for a fingerprint that is not the chain's, 404 for a handle that names
// fewer than two parts or a directory that does not exist, 400 for a
// handle without a name and a bad file_tz, 403 outside the roots or into
// a hidden directory, 422 without path or with a malformed fingerprint.
func TestLogChain_Statuses(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	ts := newTestServer(t)
	handle := filepath.Join(root, "app.log")
	_, raw := getChain(t, ts.URL, url.Values{"path": {handle}})
	current := decodeChain(t, raw).Fingerprint

	cases := []struct {
		label string
		query url.Values
		want  int
	}{
		{"the same fingerprint", url.Values{"path": {handle}, "fingerprint": {current}}, http.StatusOK},
		{"an invalid chain", url.Values{"path": {filepath.Join(root, "bad.log")}}, http.StatusOK},
		{"a zone", url.Values{"path": {handle}, "file_tz": {"Asia/Tokyo"}}, http.StatusOK},
		{"another fingerprint", url.Values{"path": {handle}, "fingerprint": {"0000000000000000"}}, http.StatusConflict},
		{"one part", url.Values{"path": {filepath.Join(root, "single.log")}}, http.StatusNotFound},
		{"a missing directory", url.Values{"path": {filepath.Join(root, "absent", "x.log")}}, http.StatusNotFound},
		{"no name", url.Values{"path": {root + "/"}}, http.StatusBadRequest},
		{"a bad zone", url.Values{"path": {handle}, "file_tz": {"Mars/Olympus"}}, http.StatusBadRequest},
		{"outside the roots", url.Values{"path": {filepath.Join(t.TempDir(), "x.log")}}, http.StatusForbidden},
		{"a hidden directory", url.Values{"path": {filepath.Join(root, ".hidden", "x.log")}}, http.StatusForbidden},
		{"a hidden name", url.Values{"path": {filepath.Join(root, ".x.log")}}, http.StatusForbidden},
		{"no path", url.Values{}, http.StatusUnprocessableEntity},
		{"a malformed fingerprint", url.Values{"path": {handle}, "fingerprint": {"xyz"}}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		status, raw := getChain(t, ts.URL, tc.query)
		if status != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.label, status, tc.want, raw)
			continue
		}
		if status == http.StatusConflict || status == http.StatusOK {
			if resp := decodeChain(t, raw); resp.Fingerprint != current && tc.query.Get("path") == handle {
				t.Errorf("%s: fingerprint %s, want the current %s", tc.label, resp.Fingerprint, current)
			}
		}
	}
	// The part without timestamps is read once it is indexed; until
	// then the chain is pending.
	_, raw = getChain(t, ts.URL, url.Values{"path": {filepath.Join(root, "bad.log")}})
	if bad := decodeChain(t, raw); bad.State != rxtypes.ChainStatePending {
		t.Fatalf("unindexed: %s", raw)
	}
	storePartIndexes(t, root, "bad.log.1")
	_, raw = getChain(t, ts.URL, url.Values{"path": {filepath.Join(root, "bad.log")}})
	if bad := decodeChain(t, raw); bad.State != rxtypes.ChainStateInvalid || len(bad.Reasons) != 1 ||
		bad.Reasons[0].Code != rxtypes.ChainReasonNoTimestamps {
		t.Fatalf("invalid chain: %s", raw)
	}
}

// Lines appended to the active file leave the fingerprint and every
// global start as they were: no 409 for a client that sends the
// fingerprint it holds.
func TestLogChain_GrowthIsNoChange(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	storePartIndexes(t, root, "app.log.2", "app.log.1")
	ts := newTestServer(t)
	handle := filepath.Join(root, "app.log")
	_, raw := getChain(t, ts.URL, url.Values{"path": {handle}})
	before := decodeChain(t, raw)
	f, err := os.OpenFile(handle, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("2026-10-01 02:00:59.000 LINE 36\n")
	_ = f.Close()
	status, raw := getChain(t, ts.URL, url.Values{"path": {handle}, "fingerprint": {before.Fingerprint}})
	after := decodeChain(t, raw)
	if status != http.StatusOK || after.Fingerprint != before.Fingerprint || after.State != rxtypes.ChainStateReady {
		t.Fatalf("status %d: %s", status, raw)
	}
	for k := range before.Parts {
		if *before.Parts[k].GlobalStart != *after.Parts[k].GlobalStart {
			t.Fatalf("%s moved", before.Parts[k].Name)
		}
	}
	if *after.LastMs <= *before.LastMs {
		t.Fatalf("last_ms %d then %d", *before.LastMs, *after.LastMs)
	}
}

// A part that changed while the request read it answers 409 like a
// fingerprint that differs.
func TestLogChainStatus(t *testing.T) {
	cases := []struct {
		changed       bool
		sent, current string
		want          int
	}{
		{false, "", "aaaaaaaaaaaaaaaa", http.StatusOK},
		{false, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", http.StatusOK},
		{false, "bbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaa", http.StatusConflict},
		{true, "", "aaaaaaaaaaaaaaaa", http.StatusConflict},
		{true, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", http.StatusConflict},
	}
	for _, tc := range cases {
		if got := logChainStatus(tc.changed, tc.sent, tc.current); got != tc.want {
			t.Errorf("changed %v sent %q: %d, want %d", tc.changed, tc.sent, got, tc.want)
		}
	}
}

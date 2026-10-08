package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// getLogSamples asks GET /v1/logs/samples with query and header, and
// returns the status and the body.
func getLogSamples(t *testing.T, base string, query url.Values, header http.Header) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/logs/samples?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// decodeChainSamples decodes a chain samples answer.
func decodeChainSamples(t *testing.T, raw []byte) rxtypes.ChainSamplesResponse {
	t.Helper()
	var resp rxtypes.ChainSamplesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return resp
}

// cliSamples answers req on the chain at handle the way `rx logs
// samples` does: described by a scan, every part read with its stored
// index or without one.
func cliSamples(t *testing.T, handle string, req logchain.SamplesRequest) *rxtypes.ChainSamplesResponse {
	t.Helper()
	d, _, err := logchain.DescribeHandle(context.Background(), handle, logchain.Options{Scan: true})
	if err != nil {
		t.Fatal(err)
	}
	req.IndexLoader = samples.StoredIndex
	resp, err := logchain.Samples(context.Background(), d, req,
		func(ctx context.Context, _ logchain.Part, r samples.Request) (*rxtypes.SamplesResponse, error) {
			return samples.Resolve(ctx, r)
		})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A request by global line on a pending chain waits for the chain's
// index task (no Prefer header) and answers what `rx logs samples`
// answers from a scan: the same pieces, lines and timestamps; the
// pieces' cli_command is the per-part rx samples command.
func TestLogSamples_WaitsForThePendingChainAndAnswersAsTheCLI(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	ts := newTestServer(t)
	handle := filepath.Join(root, "app.log")

	status, raw := getLogSamples(t, ts.URL, url.Values{"path": {handle}, "lines": {"19-22,30"}, "context": {"2"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	got := decodeChainSamples(t, raw)
	if got.State != rxtypes.ChainStateReady {
		t.Fatalf("state %s", got.State)
	}
	want := cliSamples(t, handle, logchain.SamplesRequest{Lines: mustLines(t, "19-22,30"), BeforeContext: 2, AfterContext: 2})
	for key := range want.Samples {
		if a, b := piecesText(t, want.Samples[key]), piecesText(t, got.Samples[key]); a != b {
			t.Fatalf("key %s: the CLI gives\n%s\nthe route\n%s", key, a, b)
		}
		if want.Lines[key] != got.Lines[key] {
			t.Fatalf("lines[%s] %d, the CLI %d", key, got.Lines[key], want.Lines[key])
		}
	}
	pieces := got.Samples["19-22"]
	if len(pieces) != 2 || pieces[0].Part != "app.log.2" || pieces[1].Part != "app.log.1" {
		t.Fatalf("19-22 pieces: %s", raw)
	}
	wantCmd := "rx samples " + filepath.Join(root, "app.log.1") + " --lines=1-2"
	if pieces[1].CLICommand != wantCmd || !strings.HasPrefix(got.CLICommand, "rx logs samples "+handle+" --lines=19-22,30") {
		t.Fatalf("cli_command %q, piece %q", got.CLICommand, pieces[1].CLICommand)
	}
}

// piecesText is pieces as JSON without their cli_command, which the
// route fills and the library does not.
func piecesText(t *testing.T, pieces []rxtypes.ChainPiece) string {
	t.Helper()
	out := make([]rxtypes.ChainPiece, len(pieces))
	copy(out, pieces)
	for i := range out {
		out[i].CLICommand = ""
	}
	return jsonText(t, out)
}

// mustLines parses a lines spec.
func mustLines(t *testing.T, spec string) []samples.OffsetOrRange {
	t.Helper()
	parsed, err := samples.ParseCSV(spec)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// With Prefer: respond-async, a request by global line or by time on a
// pending chain whose index task outlasts the server's wait answers 202
// with that task; a request addressed to a part answers 200 at once,
// reading that part alone, and names the task in index_build.
func TestLogSamples_APendingChainAnswers202OrReadsAPart(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	manager := tasks.New(tasks.Config{})
	ts := httptest.NewServer(NewServer(Config{AppVersion: "unit-test", TaskManager: manager, SamplesIndexWait: 10 * time.Millisecond}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { awaitEveryTask(t, manager) })
	handle := filepath.Join(root, "app.log")
	// A task that holds a part's path keeps the chain's task waiting.
	held, _ := manager.Create(filepath.Join(root, "app.log.1"), "compress")
	prefer := http.Header{"Prefer": {"respond-async"}}

	for _, query := range []url.Values{
		{"path": {handle}, "lines": {"5"}},
		{"path": {handle}, "timestamps": {"2026-10-01 01:00:00"}},
	} {
		status, raw := getLogSamples(t, ts.URL, query, prefer)
		var task rxtypes.TaskResponse
		_ = json.Unmarshal(raw, &task)
		if status != http.StatusAccepted || !strings.Contains(task.Message, "log chain") {
			t.Fatalf("%v: status %d: %s", query, status, raw)
		}
	}

	status, raw := getLogSamples(t, ts.URL, url.Values{"path": {handle}, "part": {"app.log.2"}, "lines": {"20"}, "context": {"3"}}, prefer)
	got := decodeChainSamples(t, raw)
	if status != http.StatusOK || got.State != rxtypes.ChainStatePending || got.IndexBuild == nil {
		t.Fatalf("status %d: %s", status, raw)
	}
	p := got.Samples["20"]
	if len(p) != 1 || p[0].FirstLocalLine != 17 || len(p[0].Lines) != 4 || !p[0].PartEnd || p[0].FirstGlobalLine != -1 || got.Lines["20"] != -1 {
		t.Fatalf("a part read alone: %s", raw)
	}
	manager.Fail(held.TaskID, "released by the test")
}

// The statuses: 409 with the current description for a fingerprint that
// is not the chain's, 422 for an invalid chain, 404 for a handle that
// names fewer than two parts, 400 for a part that is not a member, a
// part that is a path, a part with times, lines with times, neither, a
// bad lines spec or file_tz and an invalid time, 403 outside the roots,
// 422 for a malformed fingerprint.
func TestLogSamples_Statuses(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	storePartIndexes(t, root, "app.log.2", "app.log.1")
	ts := newTestServer(t)
	handle := filepath.Join(root, "app.log")
	cases := []struct {
		name  string
		query url.Values
		want  int
	}{
		{"lines", url.Values{"path": {handle}, "lines": {"1,-1"}}, http.StatusOK},
		{"times", url.Values{"path": {handle}, "timestamps": {"2026-10-01 01:00:00", "2026-10-01 00:00:05..2026-10-01 01:00:03"}}, http.StatusOK},
		{"an old fingerprint", url.Values{"path": {handle}, "lines": {"1"}, "fingerprint": {"0000000000000000"}}, http.StatusConflict},
		{"an invalid chain", url.Values{"path": {filepath.Join(root, "bad.log")}, "lines": {"1"}}, http.StatusUnprocessableEntity},
		{"a lone file", url.Values{"path": {filepath.Join(root, "single.log")}, "lines": {"1"}}, http.StatusNotFound},
		{"not a member", url.Values{"path": {handle}, "part": {"app.log.7"}, "lines": {"1"}}, http.StatusBadRequest},
		{"a part that is a path", url.Values{"path": {handle}, "part": {"../app.log.1"}, "lines": {"1"}}, http.StatusBadRequest},
		{"a part with times", url.Values{"path": {handle}, "part": {"app.log.1"}, "timestamps": {"2026-10-01"}}, http.StatusBadRequest},
		{"lines and times", url.Values{"path": {handle}, "lines": {"1"}, "timestamps": {"2026-10-01"}}, http.StatusBadRequest},
		{"neither", url.Values{"path": {handle}}, http.StatusBadRequest},
		{"a bad lines spec", url.Values{"path": {handle}, "lines": {"5-1"}}, http.StatusBadRequest},
		{"a bad zone", url.Values{"path": {handle}, "lines": {"1"}, "file_tz": {"Mars/Base"}}, http.StatusBadRequest},
		{"an invalid time", url.Values{"path": {handle}, "timestamps": {"soon"}}, http.StatusBadRequest},
		{"outside the roots", url.Values{"path": {"/etc/syslog"}, "lines": {"1"}}, http.StatusForbidden},
		{"a malformed fingerprint", url.Values{"path": {handle}, "lines": {"1"}, "fingerprint": {"xyz"}}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := getLogSamples(t, ts.URL, tc.query, nil)
			if status != tc.want {
				t.Fatalf("status %d, want %d: %s", status, tc.want, raw)
			}
			switch tc.want {
			case http.StatusConflict:
				if c := decodeChain(t, raw); c.Fingerprint == "" || c.Path != handle {
					t.Fatalf("409 body: %s", raw)
				}
			case http.StatusUnprocessableEntity:
				if tc.name == "an invalid chain" && !strings.Contains(string(raw), "no_timestamps") {
					t.Fatalf("422 body names no reason: %s", raw)
				}
			}
		})
	}
}

// RX_SAMPLES_MAX_LINES bounds the whole answer: a range whose lines fit
// in each part's read but not in their sum is refused with 400, which
// names the setting.
func TestLogSamples_TheLimitBoundsTheWholeAnswer(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_SAMPLES_MAX_LINES", "1000")
	root := t.TempDir()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	write := func(name string, at time.Time, first int) {
		var b strings.Builder
		for i := 0; i < 600; i++ {
			fmt.Fprintf(&b, "%s LINE %d\n", at.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05.000"), first+i)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("big.log.1", start, 1)
	write("big.log", start.Add(time.Hour), 601)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	storePartIndexes(t, root, "big.log.1")
	ts := newTestServer(t)
	handle := filepath.Join(root, "big.log")
	if status, raw := getLogSamples(t, ts.URL, url.Values{"path": {handle}, "lines": {"1-1000"}}, nil); status != http.StatusOK {
		t.Fatalf("1000 lines: %d %s", status, raw)
	}
	status, raw := getLogSamples(t, ts.URL, url.Values{"path": {handle}, "lines": {"1-1001"}}, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "RX_SAMPLES_MAX_LINES") {
		t.Fatalf("1001 lines: %d %s", status, raw)
	}
}

// A part that changed after the chain was described (a rotation
// renamed it before it was read) answers 409 with the chain as it is
// now.
func TestLogSamples_APartThatChangedAnswers409(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	s := NewServer(Config{AppVersion: "unit-test", TaskManager: tasks.New(tasks.Config{})})
	in := &logSamplesInput{Path: filepath.Join(root, "app.log"), Lines: "1"}
	if err := os.Rename(filepath.Join(root, "app.log.1"), filepath.Join(root, "app.log.3")); err != nil {
		t.Fatal(err)
	}
	out, err := s.logSamplesError(context.Background(), in, logchain.Options{}, fmt.Errorf("read: %w", paths.ErrFileChanged))
	if err != nil || out.Status != http.StatusConflict {
		t.Fatalf("got %+v, %v", out, err)
	}
	if d := out.Body.(*rxtypes.ChainResponse); d.Parts[1].Name != "app.log.3" && d.Parts[0].Name != "app.log.3" {
		t.Fatalf("409 body is not the chain as it is now: %+v", d.Parts)
	}
}

// The parameters are checked before anything is read: whatever part,
// lines and timestamps a client sends, the check refuses them with 400
// or gives a request whose part is a bare name, never a panic.
func FuzzParseLogSamplesInput(f *testing.F) {
	f.Add("app.log.1", "1,2-5,-3", "", 3)
	f.Add("../x", "1", "", -1)
	f.Add("", "", "2026-10-01..", 0)
	f.Add("a/b", "9223372036854775807-9223372036854775807", "x", 100)
	f.Add("..", "-9223372036854775808", "", 2)
	f.Fuzz(func(t *testing.T, part, lines, times string, context int) {
		in := &logSamplesInput{Path: "/x/app.log", Part: part, Lines: lines, Context: context, BeforeContext: -1, AfterContext: -1}
		if times != "" {
			in.Timestamps = []string{times}
		}
		got, err := parseLogSamplesInput(in)
		if err != nil {
			var status interface{ GetStatus() int }
			if !errors.As(err, &status) || status.GetStatus() != http.StatusBadRequest {
				t.Fatalf("error %v is no 400", err)
			}
			return
		}
		if got.samples.Part != "" && (strings.ContainsAny(got.samples.Part, `/\`) || got.samples.Part == ".." || got.samples.Part == ".") {
			t.Fatalf("part %q passed", got.samples.Part)
		}
		if (len(got.samples.Lines) > 0) == (len(got.samples.Timestamps) > 0) {
			t.Fatalf("lines %v and times %v", got.samples.Lines, got.samples.Timestamps)
		}
	})
}

// Lines appended to the active file between two requests: the second
// request, sent with the first answer's fingerprint, answers 200, not
// 409; every earlier line keeps its global number, and -1 reaches the
// new last line.
func TestLogSamples_TheActiveFileGrowsBetweenTwoRequests(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := describedChainRoot(t)
	storePartIndexes(t, root, "app.log.2", "app.log.1")
	ts := newTestServer(t)
	handle := filepath.Join(root, "app.log")
	query := url.Values{"path": {handle}, "lines": {"30,33,-1"}, "context": {"0"}}

	status, raw := getLogSamples(t, ts.URL, query, nil)
	first := decodeChainSamples(t, raw)
	if status != http.StatusOK || first.Lines["35"] != 35 {
		t.Fatalf("first: %d %s", status, raw)
	}
	f, err := os.OpenFile(handle, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2026-10-01 02:00:05.000 LINE 36\n2026-10-01 02:00:06.000 LINE 37\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	query.Set("fingerprint", first.Fingerprint)
	status, raw = getLogSamples(t, ts.URL, query, nil)
	second := decodeChainSamples(t, raw)
	if status != http.StatusOK || second.Fingerprint != first.Fingerprint {
		t.Fatalf("second: %d %s", status, raw)
	}
	for _, key := range []string{"30", "33"} {
		if a, b := piecesText(t, first.Samples[key]), piecesText(t, second.Samples[key]); a != b {
			t.Fatalf("key %s changed:\n%s\n%s", key, a, b)
		}
	}
	if got := second.Samples["37"]; second.Lines["37"] != 37 || len(got) != 1 || got[0].Lines[0] != "2026-10-01 02:00:06.000 LINE 37" {
		t.Fatalf("-1 after the append: %s", raw)
	}
}

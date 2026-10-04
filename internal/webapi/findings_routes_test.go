package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// fakeKeyBody is the body line of the private key in findingsLog: an
// invented value (base64 of FAKEKEYBODYFAKEKEYBODY), no real key.
const fakeKeyBody = "RkFLRUtFWUJPRFlGQUtFS0VZQk9EWQ=="

// findingsLog is the log of the route tests. Its findings by the
// fixture catalogue: err-line on lines 1 (no time: no stamp before it),
// 3, 11 and 13; py-trace on 4-6; pem-key on 8-10 (8 the anchor, 9-10
// its masked body); the overridden built-in (FATAL) on 12.
var findingsLog = strings.Join([]string{
	"ERROR before any time",
	"2026-01-15 10:00:00 INFO start",
	"2026-01-15 10:00:01 ERROR disk full",
	"Traceback (most recent call last):",
	`  File "app.py", line 3, in <module>`,
	"ValueError: bad input",
	"2026-01-15 10:00:05 INFO between",
	"-----BEGIN PRIVATE KEY-----",
	fakeKeyBody,
	"-----END PRIVATE KEY-----",
	"2026-01-15 10:00:09 ERROR Timeout after the key",
	"2026-01-15 10:00:10 FATAL crash",
	"2026-01-15 10:30:00 ERROR much later",
}, "\n") + "\n"

// findingsFixture is a search root holding findingsLog (app.log), a
// directory and a binary file, a catalogue of user signatures beside
// the built-ins, and a server over them.
type findingsFixture struct {
	root, log, dir, binary string
	// builtin is the id of the built-in the user file overrides; every
	// other built-in is disabled, so the default set is the override,
	// err-line, py-trace and pem-key.
	builtin string
	cat     *signatures.Catalogue
	manager *tasks.Manager
	ts      *httptest.Server
	// summaries counts the tree's findings summaries, by path.
	mu        sync.Mutex
	summaries map[string]int
}

// findingsCatalogue writes the fixture's user signature file and loads
// it with the built-ins, every example checked as `rx serve` does: one
// user signature overrides a built-in, the others are disabled, and one
// definition is refused (a load error).
func findingsCatalogue(t *testing.T) (*signatures.Catalogue, string) {
	t.Helper()
	builtins := signatures.Load(signatures.LoadOptions{Paths: []string{}}).Signatures()
	if len(builtins) == 0 {
		t.Fatal("no built-in signature")
	}
	override := builtins[0].ID
	var disabled []string
	for _, b := range builtins[1:] {
		disabled = append(disabled, b.ID)
	}
	body := "disable: [" + strings.Join(disabled, ", ") + "]\nsignatures:\n" + `  - id: ` + override + `
    name: Fatal line
    group: severity
    level: critical
    description: Replaces a built-in for the tests.
    regex: 'FATAL'
  - id: err-line
    name: Error line
    group: severity
    level: high
    description: A line that says ERROR.
    tags: [errors]
    links: [https://example.com/err-line]
    metadata: {team: core}
    regex: 'ERROR'
  - id: py-trace
    name: Python traceback
    group: traceback
    description: A traceback with its frames.
    block:
      start: '^Traceback \(most recent call last\):'
      continue: '^\s'
      end: '^[A-Za-z]+Error: '
  - id: pem-key
    name: Private key
    group: secret
    level: high
    description: A PEM private key.
    redact: true
    block:
      start: '-----BEGIN [A-Z ]*PRIVATE KEY-----'
      end: '-----END [A-Z ]*PRIVATE KEY-----'
  - id: opt-sql
    name: SQL select
    group: sql
    description: A SELECT statement.
    default: false
    regex: '(?i)select\s'
  - id: broken
    name: Broken
    group: severity
    description: A regex that does not compile.
    regex: '(unclosed'
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return signatures.Load(signatures.LoadOptions{Paths: []string{dir}, CheckExamples: sigscan.CheckExamples}), override
}

// newFindingsFixture builds the fixture; configure, when set, changes
// the server's configuration before the server is built.
func newFindingsFixture(t *testing.T, configure func(*Config)) *findingsFixture {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	for _, name := range []string{"RX_LOG_TZ", "RX_NO_INDEX", "RX_FINDINGS_TEXT_MAX_BYTES", "RX_MAX_LINE_TEXT_BYTES", "RX_MASK_SECRETS"} {
		t.Setenv(name, "")
	}
	f := &findingsFixture{root: t.TempDir(), summaries: map[string]int{}}
	f.log = filepath.Join(f.root, "app.log")
	f.dir = filepath.Join(f.root, "sub")
	f.binary = filepath.Join(f.root, "blob.bin")
	writeFixture(t, f.log, []byte(findingsLog))
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(f.dir, "other.log"), []byte("2026-01-15 11:00:00 ERROR elsewhere\n"))
	writeFixture(t, f.binary, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 1})
	if err := paths.SetSearchRoots([]string{f.root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	f.cat, f.builtin = findingsCatalogue(t)
	f.manager = tasks.New(tasks.Config{})
	opts := FindingsOptionsFromEnv()
	cfg := Config{
		AppVersion: "findings-test", RipgrepPath: mustLookRg(t), TaskManager: f.manager, Signatures: f.cat, Findings: &opts,
		findingsSummary: func(file paths.Pinned, cat *signatures.Catalogue, o findings.ScanOptions) (*findings.FileSummary, error) {
			f.mu.Lock()
			f.summaries[file.Path()]++
			f.mu.Unlock()
			return findings.Summary(file, cat, o)
		},
	}
	if configure != nil {
		configure(&cfg)
	}
	f.ts = httptest.NewServer(NewServer(cfg))
	t.Cleanup(f.ts.Close)
	t.Cleanup(func() { awaitEveryTask(t, f.manager) })
	return f
}

// sibling builds a second server over the fixture's root, catalogue and
// findings cache directory, with a task manager of its own: two `rx
// serve` processes started one after the other over one cache.
// configure, when set, changes its configuration as for
// newFindingsFixture.
func (f *findingsFixture) sibling(t *testing.T, configure func(*Config)) *findingsFixture {
	t.Helper()
	g := &findingsFixture{root: f.root, log: f.log, dir: f.dir, binary: f.binary, builtin: f.builtin, cat: f.cat,
		manager: tasks.New(tasks.Config{}), summaries: map[string]int{}}
	opts := FindingsOptionsFromEnv()
	cfg := Config{AppVersion: "findings-test", RipgrepPath: mustLookRg(t), TaskManager: g.manager, Signatures: g.cat, Findings: &opts}
	if configure != nil {
		configure(&cfg)
	}
	g.ts = httptest.NewServer(NewServer(cfg))
	t.Cleanup(g.ts.Close)
	t.Cleanup(func() { awaitEveryTask(t, g.manager) })
	return g
}

// masking returns a configure function that sets the server's masking
// mode, as `rx serve --mask-secrets` or RX_MASK_SECRETS sets it.
func masking(mask bool) func(*Config) {
	return func(cfg *Config) { cfg.Findings.Scan.MaskSecrets = mask }
}

// mustLookRg is the path of ripgrep, a hard requirement of the tests.
func mustLookRg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	return path
}

// call sends one request and returns the status and the body.
func (f *findingsFixture) call(t *testing.T, method, path string, query url.Values, body any) (int, []byte) {
	t.Helper()
	target := f.ts.URL + path
	if query != nil {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// decode parses raw into v, failing the test on an error.
func decode(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
}

// getFindings asks GET /v1/findings for the log with query (the path
// added) and returns the answer; any status but 200 fails the test.
func (f *findingsFixture) getFindings(t *testing.T, pairs ...string) rxtypes.FindingsResponse {
	t.Helper()
	q := url.Values{"path": {f.log}}
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Add(pairs[i], pairs[i+1])
	}
	status, raw := f.call(t, http.MethodGet, "/v1/findings", q, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/findings %v: status %d %s", q, status, raw)
	}
	var resp rxtypes.FindingsResponse
	decode(t, raw, &resp)
	return resp
}

// startScan posts a scan of path for selection and returns the task.
func (f *findingsFixture) startScan(t *testing.T, path, selection string) rxtypes.TaskResponse {
	t.Helper()
	status, raw := f.call(t, http.MethodPost, "/v1/findings", nil, map[string]any{"path": path, "signatures": selection})
	if status != http.StatusAccepted {
		t.Fatalf("POST /v1/findings: status %d %s", status, raw)
	}
	var task rxtypes.TaskResponse
	decode(t, raw, &task)
	return task
}

// taskStatus reads one task.
func (f *findingsFixture) taskStatus(t *testing.T, id string) rxtypes.TaskStatusResponse {
	t.Helper()
	status, raw := f.call(t, http.MethodGet, "/v1/tasks/"+id, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/tasks/%s: status %d %s", id, status, raw)
	}
	var task rxtypes.TaskStatusResponse
	decode(t, raw, &task)
	return task
}

// finish polls a task until it ends and returns its last status and
// every bytes_read it showed on the way.
func (f *findingsFixture) finish(t *testing.T, id string) (rxtypes.TaskStatusResponse, []int64) {
	t.Helper()
	var read []int64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		task := f.taskStatus(t, id)
		if task.BytesRead != nil {
			read = append(read, *task.BytesRead)
		}
		switch task.Status {
		case "completed", "failed", "cancelled":
			return task, read
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not end", id)
	return rxtypes.TaskStatusResponse{}, nil
}

// scanned scans the log for selection and waits for the task to
// complete.
func (f *findingsFixture) scanned(t *testing.T, selection string) rxtypes.TaskStatusResponse {
	t.Helper()
	task, _ := f.finish(t, f.startScan(t, f.log, selection).TaskID)
	if task.Status != "completed" {
		t.Fatalf("scan: %+v", task)
	}
	return task
}

// starts lists the first lines of findings.
func starts(found []rxtypes.Finding) []int64 {
	out := []int64{}
	for _, f := range found {
		out = append(out, f.StartLine)
	}
	return out
}

// The catalogue lists every signature with every field, the override
// and its source, the disabled built-ins, the groups with their counts,
// the levels, the load error and the search path.
func TestSignaturesRoute_ListsTheCatalogueTheServerLoaded(t *testing.T) {
	f := newFindingsFixture(t, nil)
	status, raw := f.call(t, http.MethodGet, "/v1/signatures", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d %s", status, raw)
	}
	var got rxtypes.SignaturesResponse
	decode(t, raw, &got)
	byID := map[string]rxtypes.SignatureInfo{}
	for _, s := range got.Signatures {
		byID[s.ID] = s
	}
	errLine := byID["err-line"]
	if errLine.Name != "Error line" || errLine.Group != "severity" || errLine.Level != "high" || errLine.Kind != "regex" ||
		!slices.Equal(errLine.Tags, []string{"errors"}) || !slices.Equal(errLine.Links, []string{"https://example.com/err-line"}) ||
		errLine.Metadata["team"] != "core" || !errLine.Default || errLine.Redact || errLine.Regex == nil ||
		errLine.Regex.Regex != "ERROR" || errLine.Block != nil || errLine.Source.Builtin || errLine.Source.Line == 0 ||
		!strings.HasSuffix(errLine.Source.File, "team.yml") || len(errLine.Keywords) == 0 || errLine.LinePass {
		t.Errorf("err-line: %+v", errLine)
	}
	if o := byID[f.builtin]; !o.Overrides || o.Source.Builtin || o.Level != "critical" {
		t.Errorf("the override %s: %+v", f.builtin, o)
	}
	if pem := byID["pem-key"]; !pem.Redact || pem.Kind != "block" || pem.Block == nil || pem.Block.Start == "" {
		t.Errorf("pem-key: %+v", pem)
	}
	if sql := byID["opt-sql"]; sql.Default {
		t.Errorf("opt-sql is in the default set: %+v", sql)
	}
	for _, s := range got.Signatures {
		if s.DisabledBy != nil && (s.Source.Builtin != true || s.DisabledBy.Line == 0) {
			t.Errorf("disabled built-in %s: %+v", s.ID, s)
		}
	}
	if _, refused := byID["broken"]; refused {
		t.Error("the refused signature is listed")
	}
	if len(got.Errors) != 1 || got.Errors[0].ID != "broken" || got.Errors[0].Line == 0 || !strings.HasSuffix(got.Errors[0].File, "team.yml") ||
		got.Errors[0].Message == "" {
		t.Errorf("errors %+v", got.Errors)
	}
	if !slices.Equal(got.Levels, []string{"info", "low", "medium", "high", "critical"}) || len(got.SearchPath) != 1 ||
		len(got.Files) != 1 {
		t.Errorf("levels %v search path %v files %v", got.Levels, got.SearchPath, got.Files)
	}
	counts := map[string]int{}
	for _, g := range got.Groups {
		counts[g.Name] = g.Count
		if g.Order == 0 {
			t.Errorf("group %s has no order", g.Name)
		}
	}
	if counts["severity"] != 2 || counts["traceback"] != 1 || counts["secret"] != 1 || counts["sql"] != 1 {
		t.Errorf("group counts %v", counts)
	}
	// The same document `rx signatures --json` prints.
	if want := SignaturesView(f.cat, ListedSignatures(f.cat, "", ""), ""); !jsonEqual(t, got, want) {
		t.Errorf("the route's catalogue differs from the view the CLI prints")
	}
}

// jsonEqual compares two values by their JSON documents.
func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	var va, vb any
	_ = json.Unmarshal(ja, &va)
	_ = json.Unmarshal(jb, &vb)
	return fmt.Sprint(va) == fmt.Sprint(vb)
}

// Before any scan the answer is state none, the default set missing,
// no findings, and no scan runs: the cache is not even created.
func TestFindingsRoute_BeforeAnyScanAnswersNoneAndNeverScans(t *testing.T) {
	scans := 0
	f := newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			scans++
			return findings.Scan(ctx, file, cat, sel, o)
		}
	})
	got := f.getFindings(t)
	want := []string{f.builtin, "err-line", "py-trace", "pem-key"}
	if got.State != "none" || len(got.Checked) != 0 || !sameSet(got.Missing, want) || len(got.Findings) != 0 ||
		got.Total != 0 || got.Ticks != nil || got.CLICommand != "rx trace "+f.log+" --signatures=default" {
		t.Fatalf("answer %+v, want none with %v missing", got, want)
	}
	if scans != 0 {
		t.Fatalf("GET ran %d scans", scans)
	}
	if entries, err := os.ReadDir(filepath.Join(os.Getenv("RX_CACHE_DIR"), "rx", "findings")); err == nil && len(entries) > 0 {
		t.Fatalf("the cache holds %d entries after a read", len(entries))
	}
}

// sameSet reports whether a and b hold the same strings.
func sameSet(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(s string) bool { return !slices.Contains(b, s) })
}

// A scan is a task: it reports the bytes it read, rising to the file's
// size; its result is the answer a later GET gives; and a second POST
// while it runs joins it.
func TestFindingsRoute_ScanTaskReportsProgressAndItsResultIsTheGetAnswer(t *testing.T) {
	f := newFindingsFixture(t, nil)
	created := f.startScan(t, f.log, "default")
	if created.TaskID == "" || created.Path != f.log {
		t.Fatalf("created %+v", created)
	}
	task, read := f.finish(t, created.TaskID)
	if task.Status != "completed" || task.Operation != "findings" || task.BytesRead == nil || task.BytesTotal == nil ||
		*task.BytesRead != int64(len(findingsLog)) || *task.BytesTotal != int64(len(findingsLog)) ||
		task.Progress == nil || *task.Progress != 1 {
		t.Fatalf("finished task %+v", task)
	}
	if !slices.IsSorted(read) {
		t.Fatalf("bytes_read went down: %v", read)
	}
	raw, err := json.Marshal(task.Result)
	if err != nil {
		t.Fatal(err)
	}
	var result rxtypes.FindingsResponse
	decode(t, raw, &result)
	if after := f.getFindings(t); !jsonEqual(t, result, after) {
		t.Fatalf("task result %+v\nGET after it %+v", result, after)
	}
	if result.State != "full" || result.Total != 7 || !slices.Equal(starts(result.Findings), []int64{1, 3, 4, 8, 11, 12, 13}) {
		t.Fatalf("result %+v", result)
	}
}

// A second POST for a file whose scan runs returns that task: the
// scan's task is held as running, so the join is certain.
func TestFindingsRoute_SecondScanOfTheFileJoinsTheRunningOne(t *testing.T) {
	f := newFindingsFixture(t, nil)
	held, _, isNew := f.manager.CreateCancellable(f.log, "findings")
	if !isNew {
		t.Fatal("no task held")
	}
	t.Cleanup(func() { _ = f.manager.Cancel(held.TaskID) })
	joined := f.startScan(t, f.log, "all")
	if joined.TaskID != held.TaskID || !strings.Contains(joined.Message, "Joined") {
		t.Fatalf("second POST %+v, want the held task %s", joined, held.TaskID)
	}
}

// DELETE cancels a running scan: the task ends cancelled, the scan
// writes nothing, and the file can be scanned again; DELETE answers 409
// for an index task and 404 for an unknown id.
func TestTaskCancelRoute_CancelsARunningScanAndRefusesOtherTasks(t *testing.T) {
	// The first scan waits, once it has started, until its task is
	// cancelled, then runs the real scan with the canceled context.
	started, released := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	f := newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			hold := false
			once.Do(func() { hold = true })
			if hold {
				close(started)
				<-ctx.Done()
			}
			ans, err := findings.Scan(ctx, file, cat, sel, o)
			if hold {
				released <- err
			}
			return ans, err
		}
	})
	task := f.startScan(t, f.log, "default")
	<-started
	status, raw := f.call(t, http.MethodDelete, "/v1/tasks/"+task.TaskID, nil, nil)
	var cancelled rxtypes.TaskStatusResponse
	decode(t, raw, &cancelled)
	if status != http.StatusOK || cancelled.Status != "cancelled" || cancelled.CompletedAt == nil {
		t.Fatalf("DELETE: status %d %s", status, raw)
	}
	if err := <-released; err == nil {
		t.Fatal("the held scan ended without the cancel's error")
	}
	if after := f.taskStatus(t, task.TaskID); after.Status != "cancelled" || after.Error != nil || after.Result != nil {
		t.Fatalf("task after the scan stopped: %+v", after)
	}
	if got := f.getFindings(t); got.State != "none" || len(got.Checked) != 0 {
		t.Fatalf("the cancelled scan cached %+v", got)
	}
	if again := f.startScan(t, f.log, "default"); again.TaskID == task.TaskID {
		t.Fatal("a scan after the cancel joined the cancelled task")
	} else if done, _ := f.finish(t, again.TaskID); done.Status != "completed" {
		t.Fatalf("scan after the cancel: %+v", done)
	}
	if status, _ := f.call(t, http.MethodDelete, "/v1/tasks/"+task.TaskID, nil, nil); status != http.StatusConflict {
		t.Fatalf("DELETE of an ended task: %d", status)
	}

	index, _ := f.manager.Create(f.log, "index")
	t.Cleanup(func() { f.manager.Fail(index.TaskID, "held by the test") })
	status, raw = f.call(t, http.MethodDelete, "/v1/tasks/"+index.TaskID, nil, nil)
	var conflict rxtypes.TaskConflictError
	decode(t, raw, &conflict)
	if status != http.StatusConflict || conflict.TaskID != index.TaskID {
		t.Fatalf("DELETE of an index task: %d %s", status, raw)
	}
	if got, _ := f.manager.Get(index.TaskID); got.Status != tasks.StatusQueued {
		t.Fatalf("the index task changed: %+v", got)
	}
	if status, _ := f.call(t, http.MethodDelete, "/v1/tasks/"+unknownTaskID, nil, nil); status != http.StatusNotFound {
		t.Fatalf("DELETE of an unknown task: %d", status)
	}
}

// Each filter keeps what it names before the page is cut, total counts
// the filtered findings, after_line pages forward, and before_line
// lists from the nearest down.
func TestFindingsRoute_FiltersAndPaging(t *testing.T) {
	f := newFindingsFixture(t, nil)
	f.scanned(t, "default")
	cases := []struct {
		name  string
		query []string
		total int
		want  []int64
	}{
		{"everything", nil, 7, []int64{1, 3, 4, 8, 11, 12, 13}},
		{"group", []string{"group", "severity"}, 5, []int64{1, 3, 11, 12, 13}},
		{"two groups", []string{"group", "traceback", "group", "secret"}, 2, []int64{4, 8}},
		{"signature", []string{"signature", "err-line"}, 4, []int64{1, 3, 11, 13}},
		{"q in an excerpt", []string{"q", "timeout"}, 1, []int64{11}},
		{"q in a signature name", []string{"q", "PYTHON"}, 1, []int64{4}},
		{"lines overlap", []string{"lines", "5-9"}, 2, []int64{4, 8}},
		{"after_line, one", []string{"after_line", "4", "limit", "1"}, 4, []int64{8}},
		{"before_line, nearest first", []string{"before_line", "11", "limit", "2"}, 4, []int64{8, 4}},
		{"offset", []string{"offset", "5", "limit", "10"}, 7, []int64{12, 13}},
		{"signature and after_line", []string{"signature", "err-line", "after_line", "3"}, 2, []int64{11, 13}},
	}
	for _, tc := range cases {
		got := f.getFindings(t, tc.query...)
		if got.Total != tc.total || !slices.Equal(starts(got.Findings), tc.want) {
			t.Errorf("%s: total %d starts %v, want %d %v", tc.name, got.Total, starts(got.Findings), tc.total, tc.want)
		}
		if got.State != "full" || len(got.Counts) != 4 || got.Counts["err-line"].Count != 4 {
			t.Errorf("%s: the summary changed with the filter: %+v", tc.name, got)
		}
	}
}

// ticks=100 places each filtered finding with a time in its column
// over the file's time range, with its count per signature and its
// first line; the finding without a time is in no column.
func TestFindingsRoute_TicksOverTheFileTimeRange(t *testing.T) {
	f := newFindingsFixture(t, nil)
	f.scanned(t, "default")
	got := f.getFindings(t, "ticks", "100")
	start := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	end := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC).UnixMilli()
	if got.Ticks == nil || got.Ticks.Width != 100 || got.Ticks.StartMS == nil || *got.Ticks.StartMS != start ||
		got.Ticks.EndMS == nil || *got.Ticks.EndMS != end {
		t.Fatalf("ticks %+v", got.Ticks)
	}
	// 10:00:01 to 10:00:10 lie in column 0 of 30 minutes in 100
	// columns (18 s each); 10:30:00 is the last column.
	want := []rxtypes.TickColumn{
		{Column: 0, Signatures: map[string]rxtypes.TickCount{
			"err-line": {Count: 2, FirstLine: 3}, "py-trace": {Count: 1, FirstLine: 4},
			"pem-key": {Count: 1, FirstLine: 8}, f.builtin: {Count: 1, FirstLine: 12},
		}},
		{Column: 99, Signatures: map[string]rxtypes.TickCount{"err-line": {Count: 1, FirstLine: 13}}},
	}
	if !jsonEqual(t, got.Ticks.Columns, want) {
		t.Fatalf("columns %+v, want %+v", got.Ticks.Columns, want)
	}
	if grouped := f.getFindings(t, "ticks", "100", "group", "traceback"); len(grouped.Ticks.Columns) != 1 ||
		len(grouped.Ticks.Columns[0].Signatures) != 1 {
		t.Fatalf("ticks of one group: %+v", grouped.Ticks.Columns)
	}
}

// text=true fills each finding's lines and the lines asked for before
// it, masked on a server that masks: the body of the private key shows
// its length, and the budget (max_bytes) cuts the text and marks the
// rest.
func TestFindingsRoute_TextIsMaskedAndHeldToTheBudget(t *testing.T) {
	f := newFindingsFixture(t, masking(true))
	f.scanned(t, "default")
	got := f.getFindings(t, "text", "true", "before", "1", "after_line", "4")
	byLine := map[int64]rxtypes.Finding{}
	for _, found := range got.Findings {
		byLine[found.StartLine] = found
	}
	pem := byLine[8]
	if !pem.Masked || !slices.Equal(pem.Lines, []string{"----************", "[masked, 32 bytes]", "[masked, 25 bytes]"}) ||
		!slices.Equal(pem.Before, []string{"2026-01-15 10:00:05 INFO between"}) {
		t.Errorf("pem-key: masked %v lines %q before %q", pem.Masked, pem.Lines, pem.Before)
	}
	after := byLine[11]
	if !slices.Equal(after.Before, []string{"[masked, 25 bytes]"}) || !after.Masked ||
		!slices.Equal(after.Lines, []string{"2026-01-15 10:00:09 ERROR Timeout after the key"}) {
		t.Errorf("err-line after the key: masked %v lines %q before %q", after.Masked, after.Lines, after.Before)
	}
	raw, _ := json.Marshal(got)
	if bytes.Contains(raw, []byte(fakeKeyBody)) {
		t.Fatalf("the answer holds the key body: %s", raw)
	}
	if without := f.getFindings(t); slices.ContainsFunc(without.Findings, func(x rxtypes.Finding) bool { return x.Lines != nil }) {
		t.Error("lines without text=true")
	}

	// A budget that holds the first finding's line and the line before it.
	budget := len("ERROR before any time") + 1
	cut := f.getFindings(t, "text", "true", "max_bytes", fmt.Sprint(budget))
	if len(cut.Findings[0].Lines) != 1 || cut.Findings[0].LinesTruncated || cut.Findings[1].Lines != nil ||
		!cut.Findings[1].LinesTruncated || !cut.Findings[len(cut.Findings)-1].LinesTruncated {
		t.Errorf("budget %d: %+v", budget, cut.Findings)
	}
}

// file_tz moves time_ms as it moves the times of every other answer:
// each finding's time is the line_timestamps GET /v1/samples gives for
// its line under the same file_tz.
func TestFindingsRoute_FileZoneMovesTimesAsSamplesDoes(t *testing.T) {
	f := newFindingsFixture(t, nil)
	f.scanned(t, "default")
	plain := f.getFindings(t)
	got := f.getFindings(t, "file_tz", "Asia/Tokyo")
	for i, found := range got.Findings {
		q := url.Values{"path": {f.log}, "lines": {fmt.Sprint(found.StartLine)}, "context": {"0"}, "file_tz": {"Asia/Tokyo"}}
		status, raw := f.call(t, http.MethodGet, "/v1/samples", q, nil)
		var samples rxtypes.SamplesResponse
		decode(t, raw, &samples)
		stamps := samples.LineTimestamps[fmt.Sprint(found.StartLine)]
		if status != http.StatusOK || len(stamps) != 1 {
			t.Fatalf("samples of line %d: %d %s", found.StartLine, status, raw)
		}
		switch {
		case stamps[0] == nil && found.TimeMS == nil:
		case stamps[0] == nil || found.TimeMS == nil || *stamps[0] != *found.TimeMS:
			t.Errorf("line %d: time %v, samples %v", found.StartLine, found.TimeMS, stamps[0])
		}
		if found.TimeMS != nil && *found.TimeMS != *plain.Findings[i].TimeMS-9*3600*1000 {
			t.Errorf("line %d: %d is not 9 hours before %d", found.StartLine, *found.TimeMS, *plain.Findings[i].TimeMS)
		}
	}
	if status, _ := f.call(t, http.MethodGet, "/v1/findings", url.Values{"path": {f.log}, "file_tz": {"Mars/Base"}}, nil); status != http.StatusBadRequest {
		t.Errorf("an invalid file_tz: %d", status)
	}
}

// Under a file zone the ticks are in the frame of the timeline drawn
// for that zone: start_ms and end_ms are the range GET /v1/time-range
// gives for the same file_tz, and each tick's column is where that
// axis puts the finding's time read in the zone.
func TestFindingsRoute_TicksUnderAFileZoneSitWhereTheTimelinePrintsTheirTime(t *testing.T) {
	f := newFindingsFixture(t, nil)
	f.scanned(t, "default")
	for _, zone := range []string{"", "Asia/Tokyo", "-05:30"} {
		q := url.Values{"path": {f.log}}
		if zone != "" {
			q.Set("file_tz", zone)
		}
		status, raw := f.call(t, http.MethodGet, "/v1/time-range", q, nil)
		var span rxtypes.TimeRangeResponse
		decode(t, raw, &span)
		if status != http.StatusOK || span.FirstMs == nil || span.LastMs == nil {
			t.Fatalf("time range %q: %d %s", zone, status, raw)
		}
		pairs := []string{"ticks", "50"}
		if zone != "" {
			pairs = append(pairs, "file_tz", zone)
		}
		got := f.getFindings(t, pairs...)
		if *got.Ticks.StartMS != *span.FirstMs || *got.Ticks.EndMS != *span.LastMs {
			t.Fatalf("zone %q: ticks %d..%d, time range %d..%d", zone, *got.Ticks.StartMS, *got.Ticks.EndMS, *span.FirstMs, *span.LastMs)
		}
		// The axis puts time t at (t - first) / (last - first + 1) of its
		// width: every finding's time, read in the zone, is in the column
		// of that place.
		columnOf := map[int64]int{}
		for _, c := range got.Ticks.Columns {
			for _, count := range c.Signatures {
				columnOf[count.FirstLine] = c.Column
			}
		}
		for _, found := range got.Findings {
			if found.TimeMS == nil {
				continue
			}
			want := int((*found.TimeMS - *span.FirstMs) * 50 / (*span.LastMs - *span.FirstMs + 1))
			if column, first := columnOf[found.StartLine]; first && column != want {
				t.Errorf("zone %q line %d: column %d, the axis puts it in %d", zone, found.StartLine, column, want)
			}
		}
	}
}

// The tree gives each text file what the findings cache holds: null
// before a scan, the state and the count after it; a directory and a
// binary file are null. The listing asks each text file's summary only
// (findings.Summary, which reads meta.json alone), once.
func TestTreeRoute_EntriesCarryTheirFindings(t *testing.T) {
	f := newFindingsFixture(t, nil)
	tree := func() map[string]rxtypes.TreeEntry {
		t.Helper()
		status, raw := f.call(t, http.MethodGet, "/v1/tree", url.Values{"path": {f.root}}, nil)
		if status != http.StatusOK {
			t.Fatalf("tree: %d %s", status, raw)
		}
		var resp rxtypes.TreeResponse
		decode(t, raw, &resp)
		out := map[string]rxtypes.TreeEntry{}
		for _, e := range resp.Entries {
			out[e.Name] = e
		}
		return out
	}
	before := tree()
	if before["app.log"].Findings != nil || before["sub"].Findings != nil || before["blob.bin"].Findings != nil {
		t.Fatalf("findings before a scan: %+v", before)
	}
	f.scanned(t, "default")
	after := tree()
	if got := after["app.log"].Findings; got == nil || got.State != "full" || got.Count != 7 {
		t.Fatalf("app.log after the scan: %+v", got)
	}
	if after["sub"].Findings != nil || after["blob.bin"].Findings != nil {
		t.Fatalf("a directory or a binary file has findings: %+v", after)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.summaries) != 1 || f.summaries[f.log] != 2 {
		t.Fatalf("summaries asked %v, want app.log once per listing", f.summaries)
	}
}

// A path outside the search roots is refused by every route that takes
// one, before anything is read; so is a selection that names a refused
// signature, with its load error.
func TestFindingsRoutes_RefusePathsOutsideTheRootsAndBadSelections(t *testing.T) {
	f := newFindingsFixture(t, nil)
	outside := filepath.Join(t.TempDir(), "elsewhere.log")
	writeFixture(t, outside, []byte("ERROR outside\n"))
	checks := []struct {
		name   string
		method string
		path   string
		query  url.Values
		body   any
	}{
		{"GET findings", http.MethodGet, "/v1/findings", url.Values{"path": {outside}}, nil},
		{"POST findings", http.MethodPost, "/v1/findings", nil, map[string]any{"path": outside}},
		{"trace with signatures", http.MethodGet, "/v1/trace", url.Values{"path": {outside}, "signatures": {"default"}}, nil},
	}
	for _, c := range checks {
		status, raw := f.call(t, c.method, c.path, c.query, c.body)
		var refusal rxtypes.SandboxError
		decode(t, raw, &refusal)
		if status != http.StatusForbidden || refusal.ErrorCode != rxtypes.SandboxErrorCode {
			t.Errorf("%s: %d %s", c.name, status, raw)
		}
	}
	for _, c := range []struct {
		name string
		call func() (int, []byte)
	}{
		{"GET", func() (int, []byte) {
			return f.call(t, http.MethodGet, "/v1/findings", url.Values{"path": {f.log}, "signatures": {"broken"}}, nil)
		}},
		{"POST", func() (int, []byte) {
			return f.call(t, http.MethodPost, "/v1/findings", nil, map[string]any{"path": f.log, "signatures": "broken"})
		}},
		{"trace", func() (int, []byte) {
			return f.call(t, http.MethodGet, "/v1/trace", url.Values{"path": {f.log}, "signatures": {"broken"}}, nil)
		}},
	} {
		status, raw := c.call()
		if status != http.StatusBadRequest || !bytes.Contains(raw, []byte("team.yml")) {
			t.Errorf("%s with a refused signature: %d %s", c.name, status, raw)
		}
	}
	for _, c := range []struct {
		name  string
		query url.Values
		want  int
	}{
		{"a directory", url.Values{"path": {f.dir}}, http.StatusBadRequest},
		{"a binary file", url.Values{"path": {f.binary}}, http.StatusBadRequest},
		{"a missing file", url.Values{"path": {filepath.Join(f.root, "nope.log")}}, http.StatusNotFound},
		{"bad lines", url.Values{"path": {f.log}, "lines": {"9-3"}}, http.StatusBadRequest},
		{"limit over the maximum", url.Values{"path": {f.log}, "limit": {"10001"}}, http.StatusUnprocessableEntity},
		{"before over the maximum", url.Values{"path": {f.log}, "before": {"6"}}, http.StatusUnprocessableEntity},
		{"ticks over the maximum", url.Values{"path": {f.log}, "ticks": {"4001"}}, http.StatusUnprocessableEntity},
	} {
		if status, raw := f.call(t, http.MethodGet, "/v1/findings", c.query, nil); status != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, status, raw, c.want)
		}
	}
}

// The scans queued or running are bounded: past the bound a POST for
// another file answers 503 with Retry-After and starts nothing, a POST
// that joins the running scan still gets it, and a place frees when a
// scan's worker returns.
func TestFindingsRoute_UnfinishedScansAreBounded(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := newFindingsFixture(t, func(c *Config) {
		c.maxUnfinishedScans = 1
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			hold := false
			once.Do(func() { hold = true })
			if hold {
				close(started)
				<-ctx.Done()
				defer close(finished)
			}
			return findings.Scan(ctx, file, cat, sel, o)
		}
	})
	other := filepath.Join(f.dir, "other.log")
	held := f.startScan(t, f.log, "default")
	<-started

	status, raw := f.call(t, http.MethodPost, "/v1/findings", nil, map[string]any{"path": other})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a scan past the bound: %d %s", status, raw)
	}
	if joined := f.startScan(t, f.log, "default"); joined.TaskID != held.TaskID {
		t.Fatalf("a POST for the running scan's file: %+v", joined)
	}
	if status, raw := f.call(t, http.MethodDelete, "/v1/tasks/"+held.TaskID, nil, nil); status != http.StatusOK {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	<-finished
	// The worker gives its place back as it returns, just after the
	// scan does.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, raw = f.call(t, http.MethodPost, "/v1/findings", nil, map[string]any{"path": other})
		if status == http.StatusAccepted || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status != http.StatusAccepted {
		t.Fatalf("a scan once the place is free: %d %s", status, raw)
	}
	var task rxtypes.TaskResponse
	decode(t, raw, &task)
	if done, _ := f.finish(t, task.TaskID); done.Status != "completed" {
		t.Fatalf("the later scan: %+v", done)
	}
}

// A findings answer consults the file's line index (for the format and
// the time range of the ticks, and to reach the lines of the text): it
// is the same with the cache empty and right after the file is indexed,
// in the file's own zone and under a file zone.
func TestFindingsRoute_SameAnswerWithAndWithoutTheLineIndex(t *testing.T) {
	f := newFindingsFixture(t, nil)
	f.scanned(t, "default")
	queries := [][]string{
		{"text", "true", "before", "2", "ticks", "40"},
		{"text", "true", "before", "1", "ticks", "40", "file_tz", "America/New_York"},
		{"signature", "err-line", "after_line", "3", "ticks", "7", "file_tz", "+05:30"},
	}
	cold := make([]rxtypes.FindingsResponse, len(queries))
	for i, q := range queries {
		cold[i] = f.getFindings(t, q...)
	}
	status, raw := f.call(t, http.MethodPost, "/v1/index", nil, map[string]any{"path": f.log, "threshold": 0})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/index: %d %s", status, raw)
	}
	var task rxtypes.TaskResponse
	decode(t, raw, &task)
	if done, _ := f.finish(t, task.TaskID); done.Status != "completed" {
		t.Fatalf("index task: %+v", done)
	}
	// The time range of the ticks now comes from the index.
	status, raw = f.call(t, http.MethodGet, "/v1/time-range", url.Values{"path": {f.log}}, nil)
	var span rxtypes.TimeRangeResponse
	decode(t, raw, &span)
	if status != http.StatusOK || span.Source != "index" {
		t.Fatalf("time range after the build: %d %s", status, raw)
	}
	for i, q := range queries {
		if indexed := f.getFindings(t, q...); !jsonEqual(t, indexed, cold[i]) {
			t.Errorf("%v: with the index %+v\nwithout %+v", q, indexed, cold[i])
		}
	}
}

// A scan worker that panics fails its task (runDetached) and is counted
// as a failed scan.
func TestFindingsRoute_APanickingScanIsCountedAsFailed(t *testing.T) {
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)
	f := newFindingsFixture(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		c.scanFindings = func(context.Context, trace.SearchFile, *signatures.Catalogue,
			[]*signatures.Signature, findings.ScanOptions) (*findings.Answer, error) {
			panic("a scan that breaks")
		}
	})
	before := snapshotMetrics(t)["rx_signature_scans_total"]["status=failed"]
	task, _ := f.finish(t, f.startScan(t, f.log, "default").TaskID)
	if task.Status != "failed" || task.Error == nil || !strings.Contains(*task.Error, "a scan that breaks") {
		t.Fatalf("task %+v", task)
	}
	if after := snapshotMetrics(t)["rx_signature_scans_total"]["status=failed"]; after != before+1 {
		t.Fatalf("failed scans %v → %v, want one more", before, after)
	}
}

// A cancel and the scan's write meet at one point (MayWrite): a cancel
// that comes before it leaves the task cancelled and the cache empty; a
// DELETE that comes after it answers 409 and the task completes with
// its findings written. A task never ends cancelled with entries on
// disk.
func TestTaskCancelRoute_ACancelAtTheWriteNeverLeavesCancelledEntries(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		var f *findingsFixture
		// Go note: the DELETE runs on the scan's goroutine, and its
		// answer comes back on this channel. A cancel marks the task
		// cancelled before the DELETE call has returned to the scan, so
		// the task can end before the status is known; receiving from
		// the channel waits for it and orders the two goroutines.
		deleteStatuses := make(chan int, 1)
		sendDeleteStatus := func(status int) {
			select {
			case deleteStatuses <- status:
			default: // a second write point keeps the first answer
			}
		}
		f = newFindingsFixture(t, func(c *Config) {
			c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
				sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
				mayWrite := o.MayWrite
				o.MayWrite = func() bool {
					running, _ := f.manager.CancellableHolder(f.log, "findings")
					if cancelFirst {
						status, _ := f.call(t, http.MethodDelete, "/v1/tasks/"+running.TaskID, nil, nil)
						sendDeleteStatus(status)
						return mayWrite()
					}
					ok := mayWrite()
					status, _ := f.call(t, http.MethodDelete, "/v1/tasks/"+running.TaskID, nil, nil)
					sendDeleteStatus(status)
					return ok
				}
				return findings.Scan(ctx, file, cat, sel, o)
			}
		})
		task, _ := f.finish(t, f.startScan(t, f.log, "default").TaskID)
		var deleteStatus int
		select {
		case deleteStatus = <-deleteStatuses:
		case <-time.After(10 * time.Second):
			t.Fatalf("cancel first %v: the scan never reached its write", cancelFirst)
		}
		got := f.getFindings(t)
		switch {
		case cancelFirst && (deleteStatus != http.StatusOK || task.Status != "cancelled" || got.State != "none"):
			t.Errorf("cancel before the write: DELETE %d, task %s, cache %s", deleteStatus, task.Status, got.State)
		case !cancelFirst && (deleteStatus != http.StatusConflict || task.Status != "completed" || got.State != "full"):
			t.Errorf("cancel after the write: DELETE %d, task %s, cache %s", deleteStatus, task.Status, got.State)
		}
	}
}

// Under file_tz the findings of a file whose timestamps carry zones, of
// a zone-less file and of an epoch file have the times GET /v1/samples
// gives their lines under the same file_tz, and ticks over the range
// GET /v1/time-range gives; and the answer is the same before and after
// the file is indexed. The first file is the case the index once
// decided: a plain zoned file whose line 2 reads 10:00:01 in Tokyo.
func TestFindingsRoute_FileZoneTimesNeedNoIndex(t *testing.T) {
	f := newFindingsFixture(t, nil)
	files := map[string]string{
		"zoned.log": "2026-01-15T10:00:00+02:00 INFO start\n2026-01-15T10:00:01+02:00 ERROR one\nERROR no stamp\n" +
			"2026-01-15T10:00:02+01:00 ERROR another offset\n2026-01-15T10:00:03 ERROR without a zone\n" +
			"2026-01-15T10:30:00Z ERROR later\n",
		"local.log": "2026-01-15 10:00:00 INFO start\n2026-01-15 10:00:01 ERROR one\nERROR no stamp\n2026-01-15 10:30:00 ERROR later\n",
		"epoch.log": "1768471200 INFO start\n1768471201 ERROR one\nERROR no stamp\n1768473000 ERROR later\n",
	}
	const zone = "Asia/Tokyo"
	for name, text := range files {
		path := filepath.Join(f.root, name)
		writeFixture(t, path, []byte(text))
		if task, _ := f.finish(t, f.startScan(t, path, "err-line").TaskID); task.Status != "completed" {
			t.Fatalf("%s: scan %+v", name, task)
		}
		get := func() rxtypes.FindingsResponse {
			t.Helper()
			status, raw := f.call(t, http.MethodGet, "/v1/findings",
				url.Values{"path": {path}, "signatures": {"err-line"}, "file_tz": {zone}, "ticks": {"10"}}, nil)
			if status != http.StatusOK {
				t.Fatalf("%s: GET %d %s", name, status, raw)
			}
			var resp rxtypes.FindingsResponse
			decode(t, raw, &resp)
			return resp
		}
		cold := get()
		status, raw := f.call(t, http.MethodGet, "/v1/time-range", url.Values{"path": {path}, "file_tz": {zone}}, nil)
		var span rxtypes.TimeRangeResponse
		decode(t, raw, &span)
		if status != http.StatusOK || span.FirstMs == nil || span.LastMs == nil || *cold.Ticks.StartMS != *span.FirstMs ||
			*cold.Ticks.EndMS != *span.LastMs || len(cold.Ticks.Columns) < 2 {
			t.Fatalf("%s: ticks %+v, time range %s", name, cold.Ticks, raw)
		}
		for _, found := range cold.Findings {
			q := url.Values{"path": {path}, "lines": {fmt.Sprint(found.StartLine)}, "context": {"0"}, "file_tz": {zone}}
			_, raw := f.call(t, http.MethodGet, "/v1/samples", q, nil)
			var lines rxtypes.SamplesResponse
			decode(t, raw, &lines)
			stamps := lines.LineTimestamps[fmt.Sprint(found.StartLine)]
			if len(stamps) != 1 || stamps[0] == nil || found.TimeMS == nil || *found.TimeMS != *stamps[0] {
				t.Errorf("%s line %d: time %v, samples %s", name, found.StartLine, found.TimeMS, raw)
			}
		}
		if name == "zoned.log" && (cold.Findings[0].StartLine != 2 || *cold.Findings[0].TimeMS != 1768438801000) {
			t.Errorf("zoned.log line 2: %+v, want 1768438801000 (10:00:01 read in Tokyo)", cold.Findings[0])
		}

		status, raw = f.call(t, http.MethodPost, "/v1/index", nil, map[string]any{"path": path, "threshold": 0})
		var task rxtypes.TaskResponse
		decode(t, raw, &task)
		if done, _ := f.finish(t, task.TaskID); status != http.StatusOK || done.Status != "completed" {
			t.Fatalf("%s: index %d %+v", name, status, done)
		}
		if indexed := get(); !jsonEqual(t, indexed, cold) {
			t.Errorf("%s: with the index %+v\nwithout %+v", name, indexed, cold)
		}
	}
}

// requireServerMode fails unless a GET /v1/findings answer with text of
// the log's default set is in the server's mode: masked, the private
// key's body lines show their length; shown, they are the file's lines
// and nothing is marked masked. cli_command carries --mask-secrets on a
// server that masks, and only there.
func requireServerMode(t *testing.T, got rxtypes.FindingsResponse, masked bool) {
	t.Helper()
	var pem *rxtypes.Finding
	for i := range got.Findings {
		if got.Findings[i].Signature == "pem-key" {
			pem = &got.Findings[i]
		}
	}
	if pem == nil {
		t.Fatalf("no pem-key finding: checked %v missing %v", got.Checked, got.Missing)
	}
	raw, _ := json.Marshal(got)
	if masked {
		if !pem.Masked || !slices.Equal(pem.Lines, []string{"----************", "[masked, 32 bytes]", "[masked, 25 bytes]"}) ||
			bytes.Contains(raw, []byte(fakeKeyBody)) || !slices.Contains(strings.Fields(got.CLICommand), "--mask-secrets") {
			t.Errorf("masking server: pem-key masked %v lines %q, cli_command %q", pem.Masked, pem.Lines, got.CLICommand)
		}
		return
	}
	if pem.Masked || !slices.Equal(pem.Lines, []string{"-----BEGIN PRIVATE KEY-----", fakeKeyBody, "-----END PRIVATE KEY-----"}) ||
		bytes.Contains(raw, []byte(`"masked":true`)) || slices.Contains(strings.Fields(got.CLICommand), "--mask-secrets") {
		t.Errorf("showing server: pem-key masked %v lines %q, cli_command %q", pem.Masked, pem.Lines, got.CLICommand)
	}
}

// A server that masks and one that shows, over one findings cache
// directory and with the read cache of rx serve on, never give each
// other's answer, in either order: after a scan by one, the other's
// GET finds every signature missing; after the other's own scan each
// answers in its own mode, and the first server's next GET, which its
// read cache could answer from what it decoded before, finds every
// signature missing again rather than the other mode's entries.
func TestFindingsRoutes_MaskedAndShownServersNeverShareAnAnswer(t *testing.T) {
	findings.SetReadCache(8, 64<<20)
	t.Cleanup(func() { findings.SetReadCache(0, 0) })
	for _, firstMasks := range []bool{false, true} {
		t.Run(map[bool]string{false: "shown then masked", true: "masked then shown"}[firstMasks], func(t *testing.T) {
			first := newFindingsFixture(t, masking(firstMasks))
			second := first.sibling(t, masking(!firstMasks))
			requireMissing := func(f *findingsFixture) {
				t.Helper()
				got := f.getFindings(t, "text", "true")
				if got.State != "none" || len(got.Checked) != 0 || len(got.Findings) != 0 || len(got.Missing) == 0 {
					t.Errorf("the other mode's cache: state %s checked %v missing %v findings %d, want every signature missing",
						got.State, got.Checked, got.Missing, len(got.Findings))
				}
			}

			first.scanned(t, "default")
			requireServerMode(t, first.getFindings(t, "text", "true"), firstMasks)
			requireMissing(second)
			second.scanned(t, "default")
			requireServerMode(t, second.getFindings(t, "text", "true"), !firstMasks)
			requireMissing(first)
			first.scanned(t, "default")
			requireServerMode(t, first.getFindings(t, "text", "true"), firstMasks)
		})
	}
}

// GET /v1/trace with signatures answers in the server's mode too: the
// private key's lines are masked on a server that masks and shown as
// the file holds them on one that does not, and cli_command carries
// --mask-secrets on the first only.
func TestTraceRoute_FindingsFollowTheServerMode(t *testing.T) {
	for _, mask := range []bool{false, true} {
		f := newFindingsFixture(t, masking(mask))
		status, raw := f.call(t, http.MethodGet, "/v1/trace", url.Values{"path": {f.log}, "signatures": {"pem-key"}}, nil)
		if status != http.StatusOK {
			t.Fatalf("masking %v: status %d %s", mask, status, raw)
		}
		var got rxtypes.TraceResponse
		decode(t, raw, &got)
		if len(got.Findings) != 1 || got.CLICommand == nil {
			t.Fatalf("masking %v: findings %+v cli_command %v", mask, got.Findings, got.CLICommand)
		}
		pem, cli := got.Findings[0], *got.CLICommand
		want := []string{"-----BEGIN PRIVATE KEY-----", fakeKeyBody, "-----END PRIVATE KEY-----"}
		if mask {
			want = []string{"----************", "[masked, 32 bytes]", "[masked, 25 bytes]"}
		}
		if !slices.Equal(pem.Lines, want) || pem.Masked != mask || slices.Contains(strings.Fields(cli), "--mask-secrets") != mask {
			t.Errorf("masking %v: lines %q masked %v cli_command %q", mask, pem.Lines, pem.Masked, cli)
		}
	}
}

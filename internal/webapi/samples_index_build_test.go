package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// buildGate stands in for the samples index build. Each build announces
// itself on started and then blocks until release is closed, so a test
// decides when a build ends instead of racing it; after release the real
// build runs and stores a real index.
type buildGate struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBuildGate(t *testing.T) *buildGate {
	t.Helper()
	g := &buildGate{started: make(chan struct{}, 16), release: make(chan struct{})}
	// A test that fails before releasing must not leave a build blocked.
	t.Cleanup(g.open)
	return g
}

// open lets every build waiting at the gate, and every later one, go on.
func (g *buildGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *buildGate) build(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
	g.calls.Add(1)
	g.started <- struct{}{}
	<-g.release
	return samples.BuildIndex(path, progress)
}

// awaitStart blocks until a build has started.
func (g *buildGate) awaitStart(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(30 * time.Second):
		t.Fatal("no index build started")
	}
}

// samplesBuildFixture is a search root holding a gzip log whose every
// line reads "LINE <n> ...", and a server over it whose samples index
// builds go through a gate.
type samplesBuildFixture struct {
	root    string
	gzPath  string
	manager *tasks.Manager
	gate    *buildGate
	server  *Server
	base    string
}

func newSamplesBuildFixture(t *testing.T, wait time.Duration) *samplesBuildFixture {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	// No early answer from the head of the log: every request takes the
	// path that waits for the build. The tests of the head set it.
	t.Setenv("RX_SAMPLES_HEAD_MB", "0")
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	resolved, err := paths.ValidatePathWithinRoots(root)
	if err != nil {
		t.Fatalf("validate root: %v", err)
	}

	f := &samplesBuildFixture{
		root:    resolved,
		gzPath:  filepath.Join(resolved, "app.log.gz"),
		manager: tasks.New(tasks.Config{}),
		gate:    newBuildGate(t),
	}
	f.writeLog(t, "LINE")
	f.server = NewServer(Config{
		AppVersion:        "unit-test",
		TaskManager:       f.manager,
		SamplesIndexWait:  wait,
		buildSamplesIndex: f.gate.build,
	})
	ts := httptest.NewServer(f.server)
	t.Cleanup(ts.Close)
	f.base = ts.URL
	return f
}

// writeLog (re)writes the gzip log with 300 lines "<word> <n> ...".
func (f *samplesBuildFixture) writeLog(t *testing.T, word string) {
	t.Helper()
	var text bytes.Buffer
	for n := 1; n <= 300; n++ {
		fmt.Fprintf(&text, "%s %d of the log\n", word, n)
	}
	if err := os.WriteFile(f.gzPath, compressedcopy.Encode(t, compressedcopy.Gzip, text.Bytes()), 0o600); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
}

// getSamples asks for line 5 of the gzip log with `Prefer:
// respond-async`, as a client that can follow a task does, and returns
// the status and the body.
func (f *samplesBuildFixture) getSamples(t *testing.T) (int, []byte) {
	t.Helper()
	resp, body := f.askSamples(t, http.Header{"Prefer": {"respond-async"}})
	return resp.StatusCode, body
}

// askSamples asks for line 5 of the gzip log with the given headers and
// returns the response, its body read.
func (f *samplesBuildFixture) askSamples(t *testing.T, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	query := url.Values{"path": {f.gzPath}, "lines": {"5"}, "context": {"1"}}
	req, err := http.NewRequest(http.MethodGet, f.base+"/v1/samples?"+query.Encode(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header = headers
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get samples: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// requirePending checks a 202 answer and returns the task it names.
func (f *samplesBuildFixture) requirePending(t *testing.T, status int, body []byte) rxtypes.TaskResponse {
	t.Helper()
	if status != http.StatusAccepted {
		t.Fatalf("status %d, want 202; body %s", status, body)
	}
	var task rxtypes.TaskResponse
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("decode 202 body: %v; body %s", err, body)
	}
	if task.TaskID == "" || task.Path != f.gzPath {
		t.Fatalf("202 body names task %q for %q, want a task for %q", task.TaskID, task.Path, f.gzPath)
	}
	return task
}

// requireLines checks a 200 answer for line 5 with one line of context.
func requireLines(t *testing.T, status int, body []byte, word string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", status, body)
	}
	var answer rxtypes.SamplesResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode samples: %v", err)
	}
	want := []string{word + " 4 of the log", word + " 5 of the log", word + " 6 of the log"}
	got := answer.Samples["5"]
	if len(got) != len(want) {
		t.Fatalf("samples[5] = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("samples[5] = %q, want %q", got, want)
		}
	}
}

// taskStatus reads GET /v1/tasks/{id}.
func (f *samplesBuildFixture) taskStatus(t *testing.T, taskID string) rxtypes.TaskStatusResponse {
	t.Helper()
	resp, err := http.Get(f.base + "/v1/tasks/" + taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("task status %d", resp.StatusCode)
	}
	var status rxtypes.TaskStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return status
}

// awaitTask polls the task until it ends and returns its last status.
func (f *samplesBuildFixture) awaitTask(t *testing.T, taskID string) rxtypes.TaskStatusResponse {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status := f.taskStatus(t, taskID)
		if status.Status == string(tasks.StatusCompleted) || status.Status == string(tasks.StatusFailed) {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not end", taskID)
	return rxtypes.TaskStatusResponse{}
}

// Two requests that arrive while a file's index is being built share
// that one build: both are told about the same task, and once it ends,
// asking again answers the lines without a second build.
func TestSamples_ConcurrentRequestsShareOneIndexBuild(t *testing.T) {
	f := newSamplesBuildFixture(t, 20*time.Millisecond)

	type answer struct {
		status int
		body   []byte
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			status, body := f.getSamples(t)
			answers <- answer{status, body}
		}()
	}
	first, second := <-answers, <-answers
	pendingA := f.requirePending(t, first.status, first.body)
	pendingB := f.requirePending(t, second.status, second.body)
	if pendingA.TaskID != pendingB.TaskID {
		t.Fatalf("the two requests name tasks %s and %s, want one", pendingA.TaskID, pendingB.TaskID)
	}
	if calls := f.gate.calls.Load(); calls != 1 {
		t.Fatalf("%d index builds started, want 1", calls)
	}

	running := f.taskStatus(t, pendingA.TaskID)
	if running.Operation != "index" || running.Status == string(tasks.StatusCompleted) {
		t.Fatalf("task while the build is held: operation %q status %q", running.Operation, running.Status)
	}

	f.gate.open()
	finished := f.awaitTask(t, pendingA.TaskID)
	if finished.Status != string(tasks.StatusCompleted) {
		t.Fatalf("task ended %q: %v", finished.Status, finished.Error)
	}
	if finished.Progress == nil || *finished.Progress != 1 {
		t.Errorf("progress of the finished build = %v, want 1", finished.Progress)
	}

	status, body := f.getSamples(t)
	requireLines(t, status, body, "LINE")
	if calls := f.gate.calls.Load(); calls != 1 {
		t.Fatalf("%d index builds after asking again, want 1", calls)
	}
}

// A build that ends within the wait is not seen by the caller: the
// request answers the lines, as it did when the build ran inside it.
func TestSamples_AnswersTheLinesWhenTheBuildEndsWithinTheWait(t *testing.T) {
	f := newSamplesBuildFixture(t, time.Minute)

	type answer struct {
		status int
		body   []byte
	}
	answered := make(chan answer, 1)
	go func() {
		status, body := f.getSamples(t)
		answered <- answer{status, body}
	}()
	f.gate.awaitStart(t)
	f.gate.open()
	got := <-answered
	requireLines(t, got.status, got.body, "LINE")
}

// A request that gives up waiting does not stop the build: the other
// requests waiting for it, and every later one, still get its index.
func TestSamples_ACanceledWaitLeavesTheBuildRunning(t *testing.T) {
	f := newSamplesBuildFixture(t, time.Minute)
	info, err := os.Stat(f.gzPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, err := f.server.samplesIndex.await(ctx, f.gzPath, info, nil)
		waited <- err
	}()
	f.gate.awaitStart(t)
	cancel()
	if err := <-waited; err == nil {
		t.Fatal("a canceled wait returned no error")
	}

	running := f.manager.List()
	if len(running) != 1 {
		t.Fatalf("%d tasks, want the one build", len(running))
	}
	f.gate.open()
	if finished := f.awaitTask(t, running[0].TaskID); finished.Status != string(tasks.StatusCompleted) {
		t.Fatalf("build ended %q after its waiter left: %v", finished.Status, finished.Error)
	}
	if idx, err := index.LoadForSource(f.gzPath); err != nil || idx == nil {
		t.Fatalf("no index stored after the build: %v", err)
	}
}

// An index task started by POST /v1/index builds the same line index,
// so a lookup waits for it instead of starting a build of its own.
func TestSamples_WaitsForARunningIndexTask(t *testing.T) {
	f := newSamplesBuildFixture(t, 20*time.Millisecond)
	held, _ := f.manager.Create(f.gzPath, "index")

	status, body := f.getSamples(t)
	pending := f.requirePending(t, status, body)
	if pending.TaskID != held.TaskID {
		t.Fatalf("202 names task %s, want the running index task %s", pending.TaskID, held.TaskID)
	}
	if calls := f.gate.calls.Load(); calls != 0 {
		t.Fatalf("%d builds started beside the running index task", calls)
	}
}

// A task that does not build this file's index (a compress of it) is not
// waited for: the lookup answers at once, without an index.
func TestSamples_DoesNotWaitForATaskThatBuildsNoIndex(t *testing.T) {
	f := newSamplesBuildFixture(t, time.Minute)
	f.manager.Create(f.gzPath, "compress")

	status, body := f.getSamples(t)
	requireLines(t, status, body, "LINE")
	if calls := f.gate.calls.Load(); calls != 0 {
		t.Fatalf("%d builds started while a compress holds the file", calls)
	}
}

// The build in flight indexes the file as it was when the build started.
// A request for the file as it is after a rewrite does not wait for that
// build, whose index will not describe it, and answers from the file.
func TestSamples_ARewrittenFileDoesNotWaitForTheOldBuild(t *testing.T) {
	f := newSamplesBuildFixture(t, 20*time.Millisecond)

	status, body := f.getSamples(t)
	f.requirePending(t, status, body)

	f.writeLog(t, "REWRITTEN LINE")
	status, body = f.getSamples(t)
	requireLines(t, status, body, "REWRITTEN LINE")
	if calls := f.gate.calls.Load(); calls != 1 {
		t.Fatalf("%d builds, want only the first", calls)
	}
}

// A client that does not send `Prefer: respond-async` (viewer 0.4.0, a
// script) cannot follow a task, so its request waits for the shared
// build however long it takes and answers 200, as before the build was
// a task. It still joins the one build rather than starting another.
func TestSamples_WithoutPreferWaitsPastTheDeadlineForTheLines(t *testing.T) {
	f := newSamplesBuildFixture(t, 20*time.Millisecond)

	type answer struct {
		status int
		body   []byte
	}
	answered := make(chan answer, 1)
	go func() {
		resp, body := f.askSamples(t, http.Header{})
		answered <- answer{resp.StatusCode, body}
	}()
	f.gate.awaitStart(t)

	// A second, asynchronous client joins the same build and gets the 202.
	status, body := f.getSamples(t)
	f.requirePending(t, status, body)

	select {
	case got := <-answered:
		t.Fatalf("answered %d before the build ended; body %s", got.status, got.body)
	case <-time.After(100 * time.Millisecond):
	}
	f.gate.open()
	got := <-answered
	requireLines(t, got.status, got.body, "LINE")
	if calls := f.gate.calls.Load(); calls != 1 {
		t.Fatalf("%d builds, want the one both requests shared", calls)
	}
}

// The 202 says it applied the preference, as RFC 7240 asks.
func TestSamples_The202NamesThePreferenceItApplied(t *testing.T) {
	f := newSamplesBuildFixture(t, 20*time.Millisecond)
	resp, body := f.askSamples(t, http.Header{"Prefer": {"wait=10, respond-async"}})
	f.requirePending(t, resp.StatusCode, body)
	if got := resp.Header.Get("Preference-Applied"); got != "respond-async" {
		t.Fatalf("Preference-Applied = %q, want respond-async", got)
	}

	f.gate.open()
	resp, body = f.askSamples(t, http.Header{"Prefer": {"respond-async"}})
	requireEventually200(t, f, resp, body)
}

// requireEventually200 polls until the build the fixture holds has ended
// and checks that the answer then carries no Preference-Applied header.
func requireEventually200(t *testing.T, f *samplesBuildFixture, resp *http.Response, body []byte) {
	t.Helper()
	for resp.StatusCode == http.StatusAccepted {
		var task rxtypes.TaskResponse
		_ = json.Unmarshal(body, &task)
		f.awaitTask(t, task.TaskID)
		resp, body = f.askSamples(t, http.Header{"Prefer": {"respond-async"}})
	}
	requireLines(t, resp.StatusCode, body, "LINE")
	if got := resp.Header.Get("Preference-Applied"); got != "" {
		t.Fatalf("a 200 carries Preference-Applied %q", got)
	}
}

func TestPrefersRespondAsync(t *testing.T) {
	cases := map[string]bool{
		"":                          false,
		"respond-async":             true,
		"Respond-Async":             true,
		" respond-async ":           true,
		"wait=10, respond-async":    true,
		"respond-async; foo=bar":    true,
		"return=minimal":            false,
		"respond-asynchronously":    false,
		"handling=lenient,wait=100": false,
	}
	for header, want := range cases {
		t.Run(header, func(t *testing.T) {
			if got := prefersRespondAsync(header); got != want {
				t.Fatalf("prefersRespondAsync(%q) = %v, want %v", header, got, want)
			}
		})
	}
}

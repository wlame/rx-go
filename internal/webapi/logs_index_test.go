package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainPartFile is one file of a chain a test writes: its name and how
// it is compressed (a compressedcopy format, "" for plain).
type chainPartFile struct {
	name  string
	codec string
}

// writeIndexChain writes the chain name into dir: one file per entry
// of files, oldest first, each of linesPerPart lines of the form
// `<timestamp> LINE <global> part=<k> local=<l>`, a minute after the
// part before, compressed as the entry says. A codec the host cannot
// write is replaced by plain text under the same name. The parts are a
// few hundred bytes each: far below RX_LARGE_FILE_MB, which `rx index`
// would skip them by.
func writeIndexChain(t *testing.T, dir string, files []chainPartFile, linesPerPart int, start time.Time) {
	t.Helper()
	global := 1
	for k, f := range files {
		var text bytes.Buffer
		for local := 1; local <= linesPerPart; local++ {
			at := start.Add(time.Duration(k)*time.Minute + time.Duration(local)*time.Second)
			fmt.Fprintf(&text, "%s LINE %d part=%d local=%d\n", at.Format("2006-01-02 15:04:05.000"), global, k+1, local)
			global++
		}
		body := text.Bytes()
		if f.codec != "" {
			if encoded := compressedcopy.Encode(t, f.codec, body); encoded != nil {
				body = encoded
			}
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
	}
}

// sixPartChain is the chain app.log of six parts, one per storage rx
// reads, oldest first.
var sixPartChain = []chainPartFile{
	{"app.log.5.zst", compressedcopy.Zstd},
	{"app.log.4.xz", compressedcopy.Xz},
	{"app.log.3.bz2", compressedcopy.Bzip2},
	{"app.log.2.gz", compressedcopy.Gzip},
	{"app.log.1", ""},
	{"app.log", ""},
}

// describeChainAt asks GET /v1/logs/chain for handle and returns the
// 200 answer.
func describeChainAt(t *testing.T, base, handle string) rxtypes.ChainResponse {
	t.Helper()
	status, raw := getChain(t, base, url.Values{"path": {handle}})
	if status != http.StatusOK {
		t.Fatalf("describe %s: status %d: %s", handle, status, raw)
	}
	return decodeChain(t, raw)
}

// postChainIndex asks POST /v1/logs/index with query and returns the
// status and the body.
func postChainIndex(t *testing.T, base string, query url.Values) (int, []byte) {
	t.Helper()
	resp, err := http.Post(base+"/v1/logs/index?"+query.Encode(), "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// postTask asks POST /v1/logs/index with query and decodes its 200
// answer.
func postTask(t *testing.T, base string, query url.Values) rxtypes.TaskResponse {
	t.Helper()
	status, raw := postChainIndex(t, base, query)
	return postedTask(t, status, raw)
}

// postedTask decodes a 200 answer of POST /v1/logs/index.
func postedTask(t *testing.T, status int, raw []byte) rxtypes.TaskResponse {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", status, raw)
	}
	var task rxtypes.TaskResponse
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return task
}

// awaitTaskEnd waits until the task ends and returns its status.
func awaitTaskEnd(t *testing.T, manager *tasks.Manager, taskID string) *tasks.Task {
	t.Helper()
	done, known := manager.Done(taskID)
	if !known {
		t.Fatalf("no task %s", taskID)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("task %s did not end", taskID)
	}
	task, _ := manager.Get(taskID)
	return task
}

// chainResult is the result a chain's index task completed with.
func chainResult(t *testing.T, task *tasks.Task) rxtypes.ChainIndexTaskResult {
	t.Helper()
	result, ok := task.Result.(rxtypes.ChainIndexTaskResult)
	if !ok {
		t.Fatalf("task %s result %T %+v, want a ChainIndexTaskResult", task.TaskID, task.Result, task.Result)
	}
	return result
}

// createdAt is when the stored index of path was built, "" without one.
func createdAt(t *testing.T, path string) string {
	t.Helper()
	idx, err := index.LoadForSource(path)
	if err != nil || idx == nil {
		return ""
	}
	return idx.CreatedAt
}

// chainIndexFixture is a search root and a server over it whose index
// builds for samples lookups and chain tasks go through build, at most
// maxBuilds at once.
type chainIndexFixture struct {
	root    string
	manager *tasks.Manager
	server  *Server
	base    string
}

// newChainIndexFixture starts the fixture with build as the index
// build (nil: samples.BuildIndex). When the test ends every task is
// waited for, before the cache directory is removed.
func newChainIndexFixture(t *testing.T, maxBuilds int, build samplesIndexBuilder) *chainIndexFixture {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	resolved, err := paths.ValidatePathWithinRoots(root)
	if err != nil {
		t.Fatalf("validate root: %v", err)
	}
	f := &chainIndexFixture{root: resolved, manager: tasks.New(tasks.Config{})}
	f.server = NewServer(Config{
		AppVersion: "unit-test", TaskManager: f.manager, MaxIndexBuilds: maxBuilds, buildSamplesIndex: build,
	})
	ts := httptest.NewServer(f.server)
	t.Cleanup(ts.Close)
	f.base = ts.URL
	// Cleanups run last registered first: every task ends, then the
	// server closes and the directories go.
	t.Cleanup(func() { awaitEveryTask(t, f.manager) })
	return f
}

// newGatedChainFixture is newChainIndexFixture whose builds wait at a
// gate. The gate opens when the test ends, before the tasks are waited
// for.
func newGatedChainFixture(t *testing.T, maxBuilds int) (*chainIndexFixture, *buildGate) {
	t.Helper()
	gate := newBuildGate(t)
	f := newChainIndexFixture(t, maxBuilds, gate.build)
	t.Cleanup(gate.open)
	return f, gate
}

// taskProgress reads the progress GET /v1/tasks/{id} reports.
func (f *chainIndexFixture) taskProgress(t *testing.T, taskID string) float64 {
	t.Helper()
	resp, err := http.Get(f.base + "/v1/tasks/" + taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var status rxtypes.TaskStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if status.Operation != chainIndexOperation || status.Progress == nil {
		t.Fatalf("task %s: operation %q, progress %v", taskID, status.Operation, status.Progress)
	}
	return *status.Progress
}

// unfinishedIndexBuilds counts the index tasks of single files that have
// not ended.
func unfinishedIndexBuilds(manager *tasks.Manager) int {
	n := 0
	for _, task := range manager.List() {
		if task.Operation == indexOperation && !task.IsTerminal() {
			n++
		}
	}
	return n
}

// chainStart is when the test chains' first part starts.
var chainStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// A pending chain's describe starts one index task for its frozen
// parts, shown as the handle. With RX_MAX_INDEX_BUILDS at 2, at most two
// part builds run at once and the task never has more than two of its
// parts submitted; its progress rises to 1 as they end. The parts are
// a kilobyte each, far below RX_LARGE_FILE_MB. The next describe is
// ready and names the task, completed.
func TestChainIndex_DescribeBuildsTheFrozenPartsTwoAtATime(t *testing.T) {
	f, gate := newGatedChainFixture(t, 2)
	writeIndexChain(t, f.root, sixPartChain, 20, chainStart)
	handle := filepath.Join(f.root, "app.log")

	pending := describeChainAt(t, f.base, handle)
	if pending.State != rxtypes.ChainStatePending || pending.IndexBuild == nil || pending.IndexBuild.Path != handle {
		t.Fatalf("state %s, index_build %+v; want pending with a task on %s", pending.State, pending.IndexBuild, handle)
	}
	taskID := pending.IndexBuild.TaskID
	gate.awaitStart(t)
	gate.awaitStart(t)
	progress := []float64{f.taskProgress(t, taskID)}
	for ended := 0; ended < 5; ended++ {
		if n := unfinishedIndexBuilds(f.manager); n > 2 {
			t.Fatalf("%d part builds submitted at once, want at most 2", n)
		}
		gate.releaseOne(t)
		if ended+2 < 5 {
			gate.awaitStart(t)
		}
		progress = append(progress, f.taskProgress(t, taskID))
	}
	task := awaitTaskEnd(t, f.manager, taskID)
	if task.Status != tasks.StatusCompleted {
		t.Fatalf("task %s: %s", task.Status, task.Error)
	}
	progress = append(progress, f.taskProgress(t, taskID))
	for i := 1; i < len(progress); i++ {
		if progress[i] < progress[i-1] {
			t.Fatalf("progress went down: %v", progress)
		}
	}
	if last := progress[len(progress)-1]; last != 1 {
		t.Fatalf("progress %v at the end, want 1", progress)
	}
	if most := gate.mostAtOnce.Load(); most > 2 {
		t.Fatalf("%d part builds ran at once, want at most 2", most)
	}
	if calls := gate.calls.Load(); calls != 5 {
		t.Fatalf("%d part builds, want the 5 frozen parts", calls)
	}
	wantBuilt := []string{"app.log.5.zst", "app.log.4.xz", "app.log.3.bz2", "app.log.2.gz", "app.log.1"}
	if got := chainResult(t, task).Built; !slices.Equal(got, wantBuilt) {
		t.Fatalf("built %q, want %q", got, wantBuilt)
	}

	ready := describeChainAt(t, f.base, handle)
	if ready.State != rxtypes.ChainStateReady || *ready.FrozenLineCount != 100 || ready.IndexBuild == nil ||
		ready.IndexBuild.TaskID != taskID || ready.IndexBuild.Status != string(tasks.StatusCompleted) {
		t.Fatalf("after the task: state %s, frozen lines %v, index_build %+v", ready.State, ready.FrozenLineCount, ready.IndexBuild)
	}
}

// threePartChain is the chain app.log of a gzip part, a plain part and
// the active file, oldest first.
var threePartChain = []chainPartFile{
	{"app.log.2.gz", compressedcopy.Gzip},
	{"app.log.1", ""},
	{"app.log", ""},
}

// While a chain's index task runs, a second describe and a POST, with
// force or without, join it: one task per chain.
func TestChainIndex_ASecondStartJoinsTheRunningTask(t *testing.T) {
	f, gate := newGatedChainFixture(t, 2)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")

	first := describeChainAt(t, f.base, handle).IndexBuild
	if first == nil {
		t.Fatal("a pending describe started no task")
	}
	gate.awaitStart(t)
	if again := describeChainAt(t, f.base, handle).IndexBuild; again == nil || again.TaskID != first.TaskID {
		t.Fatalf("a second describe names %+v, want task %s", again, first.TaskID)
	}
	for _, query := range []url.Values{{"path": {handle}}, {"path": {handle}, "force": {"true"}}} {
		if posted := postTask(t, f.base, query); posted.TaskID != first.TaskID || posted.Path != handle {
			t.Fatalf("POST %v: task %s on %s, want the running %s", query, posted.TaskID, posted.Path, first.TaskID)
		}
	}
	gate.open()
	if task := awaitTaskEnd(t, f.manager, first.TaskID); task.Status != tasks.StatusCompleted {
		t.Fatalf("task %s: %s", task.Status, task.Error)
	}
}

// The chain's task holds chain:<handle>, not the handle, which is the
// active file's path: POST /v1/index on the active file starts its own
// task beside the chain's, and it completes while the chain's runs.
func TestChainIndex_RunsBesideAnIndexTaskOfTheActiveFile(t *testing.T) {
	f, gate := newGatedChainFixture(t, 1)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")

	chain := describeChainAt(t, f.base, handle).IndexBuild
	gate.awaitStart(t)
	body, _ := json.Marshal(map[string]any{"path": handle, "threshold": 0})
	resp, err := http.Post(f.base+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	file := postedTask(t, resp.StatusCode, raw)
	if file.TaskID == chain.TaskID {
		t.Fatal("the index task of the active file joined the chain's")
	}
	if task := awaitTaskEnd(t, f.manager, file.TaskID); task.Status != tasks.StatusCompleted {
		t.Fatalf("the active file's task: %s %s", task.Status, task.Error)
	}
	if running, _ := f.manager.Get(chain.TaskID); running.IsTerminal() {
		t.Fatalf("the chain's task ended (%s) while its first part was held", running.Status)
	}
	gate.open()
	if task := awaitTaskEnd(t, f.manager, chain.TaskID); task.Status != tasks.StatusCompleted {
		t.Fatalf("the chain's task: %s %s", task.Status, task.Error)
	}
}

// A part that cannot be read by the time its build runs fails the
// build, and the chain's task fails naming it. The chain then describes
// as invalid, the part unreadable, and the failed task stays its
// index_build.
func TestChainIndex_APartThatCannotBeReadFailsTheTask(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	f, gate := newGatedChainFixture(t, 2)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")

	taskID := describeChainAt(t, f.base, handle).IndexBuild.TaskID
	gate.awaitStart(t)
	gate.awaitStart(t)
	unreadable := filepath.Join(f.root, "app.log.1")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	gate.open()

	task := awaitTaskEnd(t, f.manager, taskID)
	if task.Status != tasks.StatusFailed || !strings.HasPrefix(task.Error, "app.log.1: ") {
		t.Fatalf("task %s: %q, want failed naming app.log.1", task.Status, task.Error)
	}
	after := describeChainAt(t, f.base, handle)
	if after.State != rxtypes.ChainStateInvalid || len(after.Reasons) != 1 ||
		after.Reasons[0].Code != rxtypes.ChainReasonUnreadable || after.Reasons[0].Parts[0] != "app.log.1" {
		t.Fatalf("after the failure: state %s, reasons %+v", after.State, after.Reasons)
	}
	if after.IndexBuild == nil || after.IndexBuild.TaskID != taskID || after.IndexBuild.Status != string(tasks.StatusFailed) {
		t.Fatalf("index_build %+v, want the failed task %s", after.IndexBuild, taskID)
	}
}

// failingBuild fails the build of every file whose name is in names and
// builds the others as samples lookups do.
func failingBuild(names ...string) samplesIndexBuilder {
	return func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		if slices.Contains(names, filepath.Base(path)) {
			return nil, "", fmt.Errorf("cannot store the line index of %s", filepath.Base(path))
		}
		return samples.BuildIndex(path, progress)
	}
}

// A describe does not start the task again when the last one failed for
// the same files: a viewer that polls a pending chain would start a
// build that fails the same way on every poll. The describe names the
// failed task; POST /v1/logs/index starts a new one.
func TestChainIndex_DescribeDoesNotRestartATaskThatFailedForTheSameFiles(t *testing.T) {
	f := newChainIndexFixture(t, 2, failingBuild("app.log.1"))
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")

	failed := describeChainAt(t, f.base, handle).IndexBuild
	if task := awaitTaskEnd(t, f.manager, failed.TaskID); task.Status != tasks.StatusFailed {
		t.Fatalf("task %s, want failed", task.Status)
	}
	again := describeChainAt(t, f.base, handle)
	if again.State != rxtypes.ChainStatePending || again.IndexBuild == nil || again.IndexBuild.TaskID != failed.TaskID {
		t.Fatalf("a describe after the failure: state %s, index_build %+v; want pending naming %s",
			again.State, again.IndexBuild, failed.TaskID)
	}
	posted := postTask(t, f.base, url.Values{"path": {handle}})
	if posted.TaskID == failed.TaskID {
		t.Fatal("POST joined the failed task instead of starting a new one")
	}
	awaitTaskEnd(t, f.manager, posted.TaskID)
}

// POST builds every part without a current index, the active file too;
// a second POST builds none (their build times stay); with force=true
// it builds every part again.
func TestChainIndex_ForceRebuildsCurrentIndexes(t *testing.T) {
	f := newChainIndexFixture(t, 2, nil)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")
	all := []string{"app.log.2.gz", "app.log.1", "app.log"}

	post := func(query url.Values) rxtypes.ChainIndexTaskResult {
		t.Helper()
		posted := postTask(t, f.base, query)
		task := awaitTaskEnd(t, f.manager, posted.TaskID)
		if task.Status != tasks.StatusCompleted {
			t.Fatalf("POST %v: task %s %s", query, task.Status, task.Error)
		}
		return chainResult(t, task)
	}
	builtAt := func() []string {
		out := make([]string, len(all))
		for i, name := range all {
			out[i] = createdAt(t, filepath.Join(f.root, name))
		}
		return out
	}

	if first := post(url.Values{"path": {handle}}); !slices.Equal(first.Built, all) {
		t.Fatalf("first POST built %q, want %q", first.Built, all)
	}
	before := builtAt()
	if kept := post(url.Values{"path": {handle}}); len(kept.Built) != 0 || !slices.Equal(builtAt(), before) {
		t.Fatalf("a POST without force built %q; build times %q then %q", kept.Built, before, builtAt())
	}
	forced := post(url.Values{"path": {handle}, "force": {"true"}})
	if !slices.Equal(forced.Built, all) || forced.CLICommand != "rx logs index "+handle+" --force" {
		t.Fatalf("POST with force: built %q, cli_command %q", forced.Built, forced.CLICommand)
	}
	for i, at := range builtAt() {
		if at == before[i] {
			t.Errorf("%s was not built again with force", all[i])
		}
	}
	if ready := describeChainAt(t, f.base, handle); ready.State != rxtypes.ChainStateReady {
		t.Fatalf("state %s after POST", ready.State)
	}
}

// A chain of 300 parts with the build queue at its 256 entries. While
// every build is held, the task has two parts submitted, both running,
// and none in the queue: handing all 300 to the queue would fill it and
// have the 259th refused. Once the builds go on, the task submits the
// parts as slots free up and the chain becomes ready.
func TestChainIndex_ThreeHundredPartsNeverFillTheQueue(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	held := func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		<-release
		return samples.BuildIndex(path, progress)
	}
	f := newChainIndexFixture(t, 2, held)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	if f.server.samplesIndex.maxQueued != 256 {
		t.Fatalf("queue of %d, want 256", f.server.samplesIndex.maxQueued)
	}
	files := make([]chainPartFile, 0, 301)
	for n := 300; n >= 1; n-- {
		files = append(files, chainPartFile{name: fmt.Sprintf("app.log.%d", n)})
	}
	files = append(files, chainPartFile{name: "app.log"})
	writeIndexChain(t, f.root, files, 2, chainStart)
	handle := filepath.Join(f.root, "app.log")

	build := describeChainAt(t, f.base, handle).IndexBuild
	deadline := time.Now().Add(30 * time.Second)
	for unfinishedIndexBuilds(f.manager) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give a task that would hand on more parts the time to do it.
	time.Sleep(50 * time.Millisecond)
	f.server.samplesIndex.mu.Lock()
	queued, active := len(f.server.samplesIndex.queue), f.server.samplesIndex.active
	f.server.samplesIndex.mu.Unlock()
	if submitted := unfinishedIndexBuilds(f.manager); submitted != 2 || queued != 0 || active != 2 {
		t.Fatalf("while held: %d parts submitted, %d queued, %d running; want 2, 0, 2", submitted, queued, active)
	}
	once.Do(func() { close(release) })

	task := awaitTaskEnd(t, f.manager, build.TaskID)
	if task.Status != tasks.StatusCompleted || len(chainResult(t, task).Built) != 300 {
		t.Fatalf("task %s %q, built %d parts", task.Status, task.Error, len(chainResult(t, task).Built))
	}
	if ready := describeChainAt(t, f.base, handle); ready.State != rxtypes.ChainStateReady || *ready.FrozenLineCount != 600 {
		t.Fatalf("state %s, frozen lines %v", ready.State, ready.FrozenLineCount)
	}
}

// After a rename rotation (.2.gz to .3.gz, .1 to .2, the active file to
// .1, a new active file) the next describe is pending with a new
// fingerprint, its task indexes the renamed parts again, and the chain
// is ready with the new fingerprint.
func TestChainIndex_ARenameRotationIsIndexedAgain(t *testing.T) {
	f := newChainIndexFixture(t, 2, nil)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")
	awaitTaskEnd(t, f.manager, describeChainAt(t, f.base, handle).IndexBuild.TaskID)
	before := describeChainAt(t, f.base, handle)
	if before.State != rxtypes.ChainStateReady {
		t.Fatalf("state %s before the rotation", before.State)
	}

	for _, rename := range [][2]string{{"app.log.2.gz", "app.log.3.gz"}, {"app.log.1", "app.log.2"}, {"app.log", "app.log.1"}} {
		if err := os.Rename(filepath.Join(f.root, rename[0]), filepath.Join(f.root, rename[1])); err != nil {
			t.Fatal(err)
		}
	}
	writeIndexChain(t, f.root, []chainPartFile{{name: "app.log"}}, 10, chainStart.Add(3*time.Minute))

	rotated := describeChainAt(t, f.base, handle)
	if rotated.State != rxtypes.ChainStatePending || rotated.Fingerprint == before.Fingerprint || rotated.IndexBuild == nil {
		t.Fatalf("after the rotation: state %s, fingerprint %s (was %s), index_build %+v",
			rotated.State, rotated.Fingerprint, before.Fingerprint, rotated.IndexBuild)
	}
	task := awaitTaskEnd(t, f.manager, rotated.IndexBuild.TaskID)
	want := []string{"app.log.3.gz", "app.log.2", "app.log.1"}
	if got := chainResult(t, task).Built; task.Status != tasks.StatusCompleted || !slices.Equal(got, want) {
		t.Fatalf("task %s %q built %q, want %q", task.Status, task.Error, got, want)
	}
	ready := describeChainAt(t, f.base, handle)
	if ready.State != rxtypes.ChainStateReady || ready.Fingerprint != rotated.Fingerprint || *ready.FrozenLineCount != 30 {
		t.Fatalf("after the task: state %s, fingerprint %s, frozen lines %v", ready.State, ready.Fingerprint, ready.FrozenLineCount)
	}
}

// The statuses of POST /v1/logs/index: 200 with the task, 409 with the
// current description (and no task) when fingerprint differs, 404 for a
// handle that names fewer than two parts, 400 for a handle without a
// name, 403 outside the roots, 422 without path or with a malformed
// fingerprint.
func TestLogIndex_Statuses(t *testing.T) {
	f := newChainIndexFixture(t, 2, nil)
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	writeIndexChain(t, f.root, []chainPartFile{{name: "single.log"}}, 2, chainStart)
	handle := filepath.Join(f.root, "app.log")

	status, raw := postChainIndex(t, f.base, url.Values{"path": {handle}, "fingerprint": {"0000000000000000"}})
	if status != http.StatusConflict {
		t.Fatalf("an old fingerprint: status %d: %s", status, raw)
	}
	if conflict := decodeChain(t, raw); conflict.Path != handle || conflict.IndexBuild != nil || f.manager.Size() != 0 {
		t.Fatalf("409 body %s; %d tasks, want none", raw, f.manager.Size())
	}
	cases := []struct {
		label string
		query url.Values
		want  int
	}{
		{"one part", url.Values{"path": {filepath.Join(f.root, "single.log")}}, http.StatusNotFound},
		{"no name", url.Values{"path": {f.root + "/"}}, http.StatusBadRequest},
		{"outside the roots", url.Values{"path": {filepath.Join(t.TempDir(), "x.log")}}, http.StatusForbidden},
		{"no path", url.Values{}, http.StatusUnprocessableEntity},
		{"a malformed fingerprint", url.Values{"path": {handle}, "fingerprint": {"xyz"}}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		if status, raw := postChainIndex(t, f.base, tc.query); status != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.label, status, tc.want, raw)
		}
	}
	current := describeChainAt(t, f.base, handle).Fingerprint
	posted := postTask(t, f.base, url.Values{"path": {handle}, "fingerprint": {current}})
	if task := awaitTaskEnd(t, f.manager, posted.TaskID); task.Operation != chainIndexOperation || task.Path != handle {
		t.Fatalf("task %+v", task)
	}
}

// When other files' builds fill the build queue, the chain's task waits
// for a slot rather than failing: once those builds end, it indexes the
// parts and completes.
func TestChainIndex_WaitsWhileOtherBuildsFillTheQueue(t *testing.T) {
	f, gate := newGatedChainFixture(t, 1)
	builds := f.server.samplesIndex
	builds.maxQueued = 1
	for i, name := range []string{"other1.log.gz", "other2.log.gz"} {
		path := filepath.Join(f.root, name)
		writeGzipLog(t, path, "LINE")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if builds.start(path, info) == nil {
			t.Fatalf("no build of %s", name)
		}
		if i == 0 {
			gate.awaitStart(t)
		}
	}
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")

	taskID := describeChainAt(t, f.base, handle).IndexBuild.TaskID
	// The task meets the full queue now; a task that gave up would have
	// failed by the time the gate opens.
	time.Sleep(100 * time.Millisecond)
	if task, _ := f.manager.Get(taskID); task.IsTerminal() {
		t.Fatalf("the task ended (%s %q) while the queue was full", task.Status, task.Error)
	}
	gate.open()
	task := awaitTaskEnd(t, f.manager, taskID)
	if got := chainResult(t, task).Built; task.Status != tasks.StatusCompleted || !slices.Equal(got, []string{"app.log.2.gz", "app.log.1"}) {
		t.Fatalf("task %s %q, built %q", task.Status, task.Error, got)
	}
}

package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/cachefill"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The findings fixture's root holds three entries, in the walk's order:
// app.log, blob.bin (not text) and sub/other.log.

// validated is path as the search roots answer it (on macOS a temporary
// directory is under /private), the path a task shows.
func validated(t *testing.T, path string) string {
	t.Helper()
	got, err := paths.ValidatePathWithinRoots(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// startTask posts body to route and returns the 202 or 200 answer.
func (f *findingsFixture) startTask(t *testing.T, route string, body map[string]any) rxtypes.TaskResponse {
	t.Helper()
	status, raw := f.call(t, http.MethodPost, route, nil, body)
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("POST %s %v: status %d %s", route, body, status, raw)
	}
	var task rxtypes.TaskResponse
	decode(t, raw, &task)
	return task
}

// folderResult decodes the result of a completed task over a directory.
func folderResult(t *testing.T, task rxtypes.TaskStatusResponse) rxtypes.FolderTaskResult {
	t.Helper()
	if task.Status != "completed" {
		t.Fatalf("task %+v", task)
	}
	var result rxtypes.FolderTaskResult
	raw, err := json.Marshal(task.Result)
	if err != nil {
		t.Fatal(err)
	}
	decode(t, raw, &result)
	return result
}

// requireCounts fails the test unless result has the counts given.
func requireCounts(t *testing.T, label string, result rxtypes.FolderTaskResult, total, done, skipped, current, failed int) {
	t.Helper()
	got := [5]int{result.FilesTotal, result.Done, result.Skipped, result.Current, result.Failed}
	if want := [5]int{total, done, skipped, current, failed}; got != want {
		t.Errorf("%s: files_total, done, skipped, current, failed = %v, want %v (%+v)", label, got, want, result)
	}
}

// POST /v1/findings on a directory scans every file the tree lists under
// it, one at a time and in the walk's order, skips a file that is not
// text with the reason, reports files_done of files_total as it goes,
// and completes with the counts; the task and its 202 carry the command
// that does the same. A file's findings are then in the cache.
func TestFindingsFolderTask_ScansEveryListedFileOneAtATime(t *testing.T) {
	var (
		mu       sync.Mutex
		order    []string
		seen     []string
		inFlight atomic.Int32
		maxSeen  atomic.Int32
		fixture  *findingsFixture
	)
	fixture = newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			now := inFlight.Add(1)
			defer inFlight.Add(-1)
			if now > maxSeen.Load() {
				maxSeen.Store(now)
			}
			running, ok := fixture.manager.CancellableHolder(validated(t, fixture.root), "findings")
			if ok {
				status := projectTaskState(running)
				mu.Lock()
				order = append(order, filepath.Base(file.Path()))
				if status.FilesDone != nil && status.FilesTotal != nil {
					seen = append(seen, filepath.Base(file.Path())+":"+itoa(*status.FilesDone)+"/"+itoa(*status.FilesTotal))
				}
				mu.Unlock()
			}
			return findings.Scan(ctx, file, cat, sel, o)
		}
	})
	f := fixture
	root := validated(t, f.root)
	started := f.startTask(t, "/v1/findings", map[string]any{"path": f.root})
	wantCLI := "rx scan " + root + " --recursive --signatures=default"
	if started.CLICommand != wantCLI || started.Path != root {
		t.Errorf("202: %+v, want cli_command %q", started, wantCLI)
	}
	done, _ := f.finish(t, started.TaskID)
	if done.CLICommand != wantCLI || done.FilesDone == nil || *done.FilesDone != 3 || *done.FilesTotal != 3 ||
		done.Progress == nil || *done.Progress != 1 {
		t.Errorf("status at the end: %+v", done)
	}
	result := folderResult(t, done)
	requireCounts(t, "first scan", result, 3, 2, 1, 0, 0)
	if result.Path != root || result.CLICommand != wantCLI || result.Findings == nil || *result.Findings != 8 {
		t.Errorf("result %+v", result)
	}
	if len(result.SkippedFiles) != 1 || result.SkippedFiles[0].Path != filepath.Join(root, "blob.bin") ||
		!strings.HasPrefix(result.SkippedFiles[0].Reason, "not a text file") || len(result.FailedFiles) != 0 {
		t.Errorf("skipped %+v failed %+v", result.SkippedFiles, result.FailedFiles)
	}
	if !slices.Equal(order, []string{"app.log", "other.log"}) || maxSeen.Load() != 1 {
		t.Errorf("scanned %v, at most %d at once", order, maxSeen.Load())
	}
	if !slices.Equal(seen, []string{"app.log:0/3", "other.log:2/3"}) {
		t.Errorf("files_done/files_total while scanning: %v", seen)
	}
	if got := f.getFindings(t); got.State != "full" {
		t.Errorf("app.log after the folder scan: %s", got.State)
	}
}

// itoa writes n in decimal.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A second folder scan skips the files whose findings are current and
// counts them as skipped and current; force scans them again.
func TestFindingsFolderTask_SkipsCurrentFilesUnlessForced(t *testing.T) {
	var scans atomic.Int32
	f := newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			ans, err := findings.Scan(ctx, file, cat, sel, o)
			if err == nil && ans.Cached < len(ans.Checked) {
				scans.Add(1)
			}
			return ans, err
		}
	})
	run := func(force bool) rxtypes.FolderTaskResult {
		task, _ := f.finish(t, f.startTask(t, "/v1/findings", map[string]any{"path": f.root, "force": force}).TaskID)
		return folderResult(t, task)
	}
	run(false)
	scans.Store(0)
	again := run(false)
	requireCounts(t, "second scan", again, 3, 0, 3, 2, 0)
	if scans.Load() != 0 || again.Findings == nil || *again.Findings != 8 {
		t.Errorf("second scan read %d files, findings %v", scans.Load(), again.Findings)
	}
	forced := run(true)
	requireCounts(t, "forced scan", forced, 3, 2, 1, 0, 0)
	if scans.Load() != 2 {
		t.Errorf("forced scan read %d files, want 2", scans.Load())
	}
	if !strings.HasSuffix(forced.CLICommand, " --recursive --signatures=default --force") {
		t.Errorf("forced cli_command %q", forced.CLICommand)
	}
}

// A file whose scan fails is recorded with its path and error, and the
// task goes on with the next file and completes.
func TestFindingsFolderTask_AFailingFileIsRecordedAndTheTaskGoesOn(t *testing.T) {
	f := newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			if filepath.Base(file.Path()) == "app.log" {
				return nil, errors.New(file.Path() + ": the scan broke")
			}
			return findings.Scan(ctx, file, cat, sel, o)
		}
	})
	task, _ := f.finish(t, f.startTask(t, "/v1/findings", map[string]any{"path": f.root}).TaskID)
	result := folderResult(t, task)
	requireCounts(t, "a failing file", result, 3, 1, 1, 0, 1)
	app := filepath.Join(validated(t, f.root), "app.log")
	if len(result.FailedFiles) != 1 || result.FailedFiles[0] != (rxtypes.FailedFile{Path: app, Error: app + ": the scan broke"}) {
		t.Errorf("failed %+v", result.FailedFiles)
	}
}

// DELETE cancels a folder scan inside a file's scan: the file scanned
// before the cancel keeps its findings, the file being scanned caches
// nothing, and no later file is scanned. The task ends cancelled with no
// result.
func TestFindingsFolderTask_CancelInsideAFileKeepsTheFilesBefore(t *testing.T) {
	held, released := make(chan struct{}), make(chan error, 1)
	var after atomic.Int32
	f := newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			switch filepath.Base(file.Path()) {
			case "other.log":
				close(held)
				<-ctx.Done()
				ans, err := findings.Scan(ctx, file, cat, sel, o)
				released <- err
				return ans, err
			case "app.log":
				return findings.Scan(ctx, file, cat, sel, o)
			}
			after.Add(1)
			return findings.Scan(ctx, file, cat, sel, o)
		}
	})
	writeFixture(t, filepath.Join(f.root, "sub", "zz.log"), []byte("2026-01-15 12:00:00 ERROR last\n"))
	task := f.startTask(t, "/v1/findings", map[string]any{"path": f.root})
	<-held
	status, raw := f.call(t, http.MethodDelete, "/v1/tasks/"+task.TaskID, nil, nil)
	var cancelled rxtypes.TaskStatusResponse
	decode(t, raw, &cancelled)
	if status != http.StatusOK || cancelled.Status != "cancelling" || cancelled.FilesDone == nil {
		t.Fatalf("DELETE: %d %s", status, raw)
	}
	if err := <-released; err == nil {
		t.Fatal("the held scan ended without the cancel's error")
	}
	end, _ := f.finish(t, task.TaskID)
	if end.Status != "cancelled" || end.Result != nil || end.Error != nil {
		t.Errorf("task after the cancel %+v", end)
	}
	if got := f.getFindings(t); got.State != "full" {
		t.Errorf("app.log, scanned before the cancel: %s", got.State)
	}
	status, raw = f.call(t, http.MethodGet, "/v1/findings", url.Values{"path": {filepath.Join(f.root, "sub", "other.log")}}, nil)
	var other rxtypes.FindingsResponse
	decode(t, raw, &other)
	if status != http.StatusOK || other.State != "none" || after.Load() != 0 {
		t.Errorf("other.log after the cancel: %s; files scanned after it: %d", other.State, after.Load())
	}
}

// POST /v1/index on a directory indexes every text file the tree lists
// under it, skips the binary one, a file below the threshold and, on a
// second run, the current ones; force builds them again. The status has
// files_done and files_total and no bytes; the command is rx index DIR
// --recursive with the request's flags.
func TestIndexFolderTask_IndexesEveryTextFileAndSkipsCurrentOnes(t *testing.T) {
	f := newFindingsFixture(t, nil)
	root := validated(t, f.root)
	run := func(body map[string]any) (rxtypes.TaskResponse, rxtypes.TaskStatusResponse, rxtypes.FolderTaskResult) {
		started := f.startTask(t, "/v1/index", body)
		done, _ := f.finish(t, started.TaskID)
		return started, done, folderResult(t, done)
	}
	started, done, first := run(map[string]any{"path": f.root, "threshold": 0})
	wantCLI := "rx index " + root + " --recursive --threshold=0"
	if started.CLICommand != wantCLI || done.CLICommand != wantCLI || first.CLICommand != wantCLI || done.Operation != "index" {
		t.Errorf("cli_command 202 %q, status %q, result %q, want %q", started.CLICommand, done.CLICommand,
			first.CLICommand, wantCLI)
	}
	if done.FilesDone == nil || *done.FilesDone != 3 || done.BytesRead != nil || first.Findings != nil {
		t.Errorf("status %+v, result findings %v", done, first.Findings)
	}
	requireCounts(t, "first index run", first, 3, 2, 1, 0, 0)
	status, raw := f.call(t, http.MethodGet, "/v1/index", url.Values{"path": {filepath.Join(f.root, "sub", "other.log")}}, nil)
	if status != http.StatusOK {
		t.Errorf("GET /v1/index of a file under the directory: %d %s", status, raw)
	}

	_, _, again := run(map[string]any{"path": f.root, "threshold": 0})
	requireCounts(t, "second index run", again, 3, 0, 3, 2, 0)
	_, _, forced := run(map[string]any{"path": f.root, "threshold": 0, "force": true})
	requireCounts(t, "forced index run", forced, 3, 2, 1, 0, 0)

	_, _, small := run(map[string]any{"path": f.root, "threshold": 1})
	requireCounts(t, "1 MB threshold", small, 3, 0, 3, 0, 0)
	if len(small.SkippedFiles) != 3 || small.SkippedFiles[0].Reason != cachefill.BelowThresholdReason(int64(len(findingsLog)), 1<<20) {
		t.Errorf("skipped below the threshold %+v", small.SkippedFiles)
	}
}

// DELETE cancels a folder index task between files: the file built
// before the cancel keeps its index, and no later file is built.
func TestIndexFolderTask_CancelBetweenFiles(t *testing.T) {
	var calls atomic.Int32
	var once sync.Once
	firstDone, cancelled := make(chan struct{}), make(chan struct{})
	f := newFindingsFixture(t, func(c *Config) {
		c.indexFolderFile = func(ctx context.Context, e cachefill.Entry, s cachefill.IndexSettings, b *index.Progress) cachefill.FileResult {
			calls.Add(1)
			res := cachefill.IndexFile(ctx, e, s, b)
			// The first file is built; the cancel comes before the
			// next one is reached.
			once.Do(func() { close(firstDone) })
			<-cancelled
			return res
		}
	})
	task := f.startTask(t, "/v1/index", map[string]any{"path": f.root, "threshold": 0})
	<-firstDone
	status, raw := f.call(t, http.MethodDelete, "/v1/tasks/"+task.TaskID, nil, nil)
	close(cancelled)
	if status != http.StatusOK {
		t.Fatalf("DELETE: %d %s", status, raw)
	}
	if end, _ := f.finish(t, task.TaskID); end.Status != "cancelled" || end.Result != nil {
		t.Errorf("task %+v", end)
	}
	awaitEveryTask(t, f.manager)
	if calls.Load() != 1 {
		t.Errorf("%d files built, want only the first", calls.Load())
	}
	for name, want := range map[string]int{"app.log": http.StatusOK, "sub/other.log": http.StatusNotFound} {
		if status, _ := f.call(t, http.MethodGet, "/v1/index", url.Values{"path": {filepath.Join(f.root, name)}}, nil); status != want {
			t.Errorf("GET /v1/index of %s: %d, want %d", name, status, want)
		}
	}
}

// A second POST for a directory whose task runs joins it (findings) or
// answers 409 naming it (index), as for a file.
func TestFolderTasks_ASecondPostForTheDirectory(t *testing.T) {
	f := newFindingsFixture(t, nil)
	root := validated(t, f.root)
	held, _, _ := f.manager.CreateCancellable(root, "findings")
	f.manager.SetCLICommand(held.TaskID, "rx scan "+root+" --recursive --signatures=default")
	t.Cleanup(func() { _ = f.manager.Cancel(held.TaskID) })
	joined := f.startTask(t, "/v1/findings", map[string]any{"path": f.root})
	if joined.TaskID != held.TaskID || !strings.Contains(joined.Message, "Joined") ||
		joined.CLICommand != "rx scan "+root+" --recursive --signatures=default" {
		t.Errorf("second findings POST %+v", joined)
	}

	index, _, _ := f.manager.CreateCancellable(root, "index")
	t.Cleanup(func() { _ = f.manager.Cancel(index.TaskID) })
	status, raw := f.call(t, http.MethodPost, "/v1/index", nil, map[string]any{"path": f.root})
	var conflict rxtypes.TaskConflictError
	decode(t, raw, &conflict)
	if status != http.StatusConflict || conflict.TaskID != index.TaskID {
		t.Errorf("second index POST: %d %s", status, raw)
	}
}

// A task over one file carries its command too: rx index FILE for an
// index task, rx scan FILE --signatures=SEL for a findings task, with
// --mask-secrets when the server masks; files_done and files_total stay
// null.
func TestTasks_AFileTaskCarriesItsCommand(t *testing.T) {
	for _, mask := range []bool{false, true} {
		f := newFindingsFixture(t, masking(mask))
		log := validated(t, f.log)
		scan := f.startTask(t, "/v1/findings", map[string]any{"path": f.log, "signatures": "err-line", "force": true})
		wantScan := "rx scan " + log + " --signatures=err-line --force"
		if mask {
			wantScan += " --mask-secrets"
		}
		done, _ := f.finish(t, scan.TaskID)
		if scan.CLICommand != wantScan || done.CLICommand != wantScan || done.FilesDone != nil || done.FilesTotal != nil {
			t.Errorf("mask=%v findings task: 202 %q, status %+v, want %q", mask, scan.CLICommand, done, wantScan)
		}
		index := f.startTask(t, "/v1/index", map[string]any{"path": f.log, "threshold": 0})
		wantIndex := "rx index " + log + " --threshold=0"
		if indexed, _ := f.finish(t, index.TaskID); index.CLICommand != wantIndex || indexed.CLICommand != wantIndex {
			t.Errorf("index task: 202 %q, status %q, want %q", index.CLICommand, indexed.CLICommand, wantIndex)
		}
	}
}

// postStatus posts body to route and returns the status and the raw
// answer.
func (f *findingsFixture) postStatus(t *testing.T, route string, body map[string]any) (int, []byte) {
	t.Helper()
	return f.call(t, http.MethodPost, route, nil, body)
}

// awaitStatus polls task id until its status is want.
func (f *findingsFixture) awaitStatus(t *testing.T, id, want string) rxtypes.TaskStatusResponse {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if task := f.taskStatus(t, id); task.Status == want {
			return task
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("task %s never reached %s", id, want)
	return rxtypes.TaskStatusResponse{}
}

// A cancelled index task over a directory keeps the directory until its
// worker has stopped: DELETE answers cancelling, a POST for the
// directory answers 409 naming the task while the build of the current
// file still runs, and the task ends cancelled once it stops. Five
// POST+DELETE rounds never run two builds at once.
func TestIndexFolderTask_APostDeleteLoopNeverRunsTwoBuilds(t *testing.T) {
	var inFlight, maxSeen atomic.Int32
	started, release := make(chan struct{}, 1), make(chan struct{})
	f := newFindingsFixture(t, func(c *Config) {
		c.indexFolderFile = func(ctx context.Context, e cachefill.Entry, s cachefill.IndexSettings, b *index.Progress) cachefill.FileResult {
			now := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				seen := maxSeen.Load()
				if now <= seen || maxSeen.CompareAndSwap(seen, now) {
					break
				}
			}
			started <- struct{}{}
			// A build that goes on for a while after the cancel.
			<-ctx.Done()
			<-release
			return cachefill.IndexFile(ctx, e, s, b)
		}
	})
	seen := map[string]bool{}
	for round := range 5 {
		task := f.startTask(t, "/v1/index", map[string]any{"path": f.root, "threshold": 0})
		if seen[task.TaskID] {
			t.Fatalf("round %d: the task of an earlier round again", round)
		}
		seen[task.TaskID] = true
		<-started
		status, raw := f.call(t, http.MethodDelete, "/v1/tasks/"+task.TaskID, nil, nil)
		var cancelling rxtypes.TaskStatusResponse
		decode(t, raw, &cancelling)
		if status != http.StatusOK || cancelling.Status != "cancelling" || cancelling.CompletedAt != nil {
			t.Fatalf("round %d DELETE: %d %s", round, status, raw)
		}
		status, raw = f.postStatus(t, "/v1/index", map[string]any{"path": f.root, "threshold": 0})
		var conflict rxtypes.TaskConflictError
		decode(t, raw, &conflict)
		if status != http.StatusConflict || conflict.TaskID != task.TaskID || !strings.Contains(conflict.Detail, "cancel") {
			t.Fatalf("round %d POST while the build stops: %d %s", round, status, raw)
		}
		if again := f.taskStatus(t, task.TaskID); again.Status != "cancelling" {
			t.Fatalf("round %d: status %s while the build runs", round, again.Status)
		}
		release <- struct{}{}
		if end := f.awaitStatus(t, task.TaskID, "cancelled"); end.Result != nil || end.Error != nil || end.CompletedAt == nil {
			t.Errorf("round %d end: %+v", round, end)
		}
	}
	if maxSeen.Load() != 1 {
		t.Errorf("%d builds ran at once for one directory", maxSeen.Load())
	}
}

// A cancelled findings task over a directory keeps the directory until
// its scan has stopped: a POST meanwhile answers 409 rather than joining
// a task that is ending, and one after the end starts a new task.
func TestFindingsFolderTask_APostWhileItIsCancellingAnswers409(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	f := newFindingsFixture(t, func(c *Config) {
		c.scanFindings = func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
			sel []*signatures.Signature, o findings.ScanOptions) (*findings.Answer, error) {
			hold := false
			once.Do(func() { hold = true })
			if hold {
				started <- struct{}{}
				<-ctx.Done()
				<-release
			}
			return findings.Scan(ctx, file, cat, sel, o)
		}
	})
	task := f.startTask(t, "/v1/findings", map[string]any{"path": f.root})
	<-started
	if status, raw := f.call(t, http.MethodDelete, "/v1/tasks/"+task.TaskID, nil, nil); status != http.StatusOK ||
		!strings.Contains(string(raw), `"cancelling"`) {
		t.Fatalf("DELETE: %d %s", status, raw)
	}
	status, raw := f.postStatus(t, "/v1/findings", map[string]any{"path": f.root})
	var conflict rxtypes.TaskConflictError
	decode(t, raw, &conflict)
	if status != http.StatusConflict || conflict.TaskID != task.TaskID {
		t.Fatalf("POST while the scan stops: %d %s", status, raw)
	}
	close(release)
	f.awaitStatus(t, task.TaskID, "cancelled")
	next := f.startTask(t, "/v1/findings", map[string]any{"path": f.root})
	if next.TaskID == task.TaskID {
		t.Fatal("the POST after the end joined the cancelled task")
	}
	if done, _ := f.finish(t, next.TaskID); done.Status != "completed" {
		t.Errorf("the next scan %+v", done)
	}
}

package webapi

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainIndexTasksIn counts the index tasks of log chains in the task
// table, ended or not.
func chainIndexTasksIn(manager *tasks.Manager) int {
	n := 0
	for _, task := range manager.List() {
		if task.Operation == chainIndexOperation {
			n++
		}
	}
	return n
}

// finishOtherTasks fills the task table with n finished tasks of other
// files, as other clients' requests would.
func finishOtherTasks(manager *tasks.Manager, n int) {
	for i := 0; i < n; i++ {
		task, _ := manager.Create(fmt.Sprintf("/elsewhere/%d.log", i), indexOperation)
		manager.Complete(task.TaskID, nil)
	}
}

// One describe of a pending chain of 300 parts builds every part, each
// as a task of its own, and leaves another client's finished task in the
// table: the part builds are capped among themselves, not with it.
func TestChainIndex_ThreeHundredPartsLeaveAnotherClientsTaskInTheTable(t *testing.T) {
	f := newChainIndexFixture(t, 2, nil)
	followed, _ := f.manager.Create(filepath.Join(f.root, "followed.log"), "compress")
	f.manager.Complete(followed.TaskID, nil)
	files := make([]chainPartFile, 0, 301)
	for n := 300; n >= 1; n-- {
		files = append(files, chainPartFile{name: fmt.Sprintf("app.log.%d", n)})
	}
	files = append(files, chainPartFile{name: "app.log"})
	writeIndexChain(t, f.root, files, 2, chainStart)

	build := describeChainAt(t, f.base, filepath.Join(f.root, "app.log")).IndexBuild
	task := awaitTaskEnd(t, f.manager, build.TaskID)

	if task.Status != tasks.StatusCompleted || len(chainResult(t, task).Built) != 300 {
		t.Fatalf("task %s %q, want 300 parts built", task.Status, task.Error)
	}
	if _, known := f.manager.Get(followed.TaskID); !known {
		t.Fatalf("the chain's part builds dropped another client's finished task (table of %d)", f.manager.Size())
	}
}

// A chain whose index task failed keeps that failure after the task has
// left the task table (256 other tasks ended after it): a describe of
// the pending chain starts no task, and index_build names the failed
// task with the part and the build's error.
func TestChainIndex_AFailureOutlivesItsTaskInTheTable(t *testing.T) {
	f := newChainIndexFixture(t, 2, failingBuild("app.log.1"))
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")
	failed := describeChainAt(t, f.base, handle).IndexBuild
	if task := awaitTaskEnd(t, f.manager, failed.TaskID); task.Status != tasks.StatusFailed {
		t.Fatalf("task %s, want failed", task.Status)
	}

	finishOtherTasks(f.manager, tasks.DefaultMaxTasks)
	if _, known := f.manager.Get(failed.TaskID); known {
		t.Fatal("the failed task is still in the table; the test needs it gone")
	}
	again := describeChainAt(t, f.base, handle)

	if again.State != rxtypes.ChainStatePending || again.IndexBuild == nil || again.IndexBuild.TaskID != failed.TaskID ||
		again.IndexBuild.Status != string(tasks.StatusFailed) {
		t.Fatalf("after the task left the table: state %s, index_build %+v; want pending naming the failed %s",
			again.State, again.IndexBuild, failed.TaskID)
	}
	if !strings.Contains(again.IndexBuild.Message, "app.log.1: build index: cannot store the line index of app.log.1") {
		t.Fatalf("message %q does not say which part failed and why", again.IndexBuild.Message)
	}
	if n := chainIndexTasksIn(f.manager); n != 0 {
		t.Fatalf("%d chain index tasks in the table, want none started", n)
	}
}

// The failure is kept as long as a finished task stays in the table
// (RX_TASK_TTL_MINUTES): past that, a describe starts the task again.
func TestChainIndex_AFailureIsForgottenAfterTheTaskTTL(t *testing.T) {
	f := newChainIndexFixture(t, 2, failingBuild("app.log.1"))
	writeIndexChain(t, f.root, threePartChain, 10, chainStart)
	handle := filepath.Join(f.root, "app.log")
	failed := describeChainAt(t, f.base, handle).IndexBuild
	awaitTaskEnd(t, f.manager, failed.TaskID)

	later := time.Now().Add(f.manager.TTL() + time.Minute)
	f.server.chainIndex.mu.Lock()
	f.server.chainIndex.now = func() time.Time { return later }
	f.server.chainIndex.mu.Unlock()
	again := describeChainAt(t, f.base, handle).IndexBuild

	if again == nil || again.TaskID == failed.TaskID {
		t.Fatalf("a describe past the TTL names %+v, want a new task", again)
	}
	awaitTaskEnd(t, f.manager, again.TaskID)
}

// An ended task's record describes the chain's files as they were: once
// the fingerprint differs, the record is dropped, and a chain that is
// not pending names no task. A record of the same files names the task
// with its end, whether or not the table still holds it.
func TestChainIndexTasks_ARecordOfOtherFilesIsDropped(t *testing.T) {
	manager := tasks.New(tasks.Config{})
	c := newChainIndexTasks(manager, nil, newSamplesIndexBuilds(manager, nil, nil, 1))
	handle := "/logs/app.log"
	describe := func(fingerprint string) *rxtypes.SamplesIndexBuild {
		return c.forDescription(&logchain.Description{Response: &rxtypes.ChainResponse{
			Path: handle, Fingerprint: fingerprint, State: rxtypes.ChainStateReady,
		}})
	}
	c.records[chainTaskKey(handle)] = chainTaskRecord{
		taskID: "ended-task", fingerprint: "00000000000000aa", startedAt: time.Now(),
		end: &chainTaskEnd{status: tasks.StatusCompleted, at: time.Now()},
	}

	if same := describe("00000000000000aa"); same == nil || same.TaskID != "ended-task" || same.Status != string(tasks.StatusCompleted) {
		t.Fatalf("the same files: index_build %+v, want the ended task", same)
	}
	if other := describe("00000000000000bb"); other != nil {
		t.Fatalf("other files: index_build %+v, want none", other)
	}
	if _, kept := c.records[chainTaskKey(handle)]; kept {
		t.Fatal("the record of other files was kept")
	}
}

// A part's build that failed and then left the task table before the
// chain's task read it still fails the chain, naming the part: the end
// comes from the watch taken when the part was submitted, not from the
// table. A build that completed counts the part as built the same way.
func TestChainIndexRun_APartBuildThatLeftTheTableKeepsItsOutcome(t *testing.T) {
	cases := []struct {
		name        string
		end         func(m *tasks.Manager, id string)
		wantBuilt   bool
		wantFailure string
	}{
		{"failed", func(m *tasks.Manager, id string) { m.Fail(id, "build index: boom") }, false, "app.log.1: build index: boom"},
		{"completed", func(m *tasks.Manager, id string) { m.Complete(id, nil) }, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := tasks.New(tasks.Config{MaxTasks: 1})
			part, _ := manager.CreateSubtask("/logs/app.log.1", indexOperation)
			build := chainPartOf(manager, part.TaskID, true)
			tc.end(manager, part.TaskID)
			other, _ := manager.CreateSubtask("/logs/other.log", indexOperation)
			manager.Complete(other.TaskID, nil)
			if _, known := manager.Get(part.TaskID); known {
				t.Fatal("the part's task is still in the table; the test needs it gone")
			}
			run := &chainIndexRun{tasks: manager, start: chainTaskStart{parts: []logchain.Part{{Name: "app.log.1"}}}}

			built, failure := run.outcome(partWait{position: 0, build: build})

			if built != tc.wantBuilt || failure != tc.wantFailure {
				t.Fatalf("outcome = %v, %q; want %v, %q", built, failure, tc.wantBuilt, tc.wantFailure)
			}
		})
	}
}

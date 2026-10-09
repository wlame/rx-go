package webapi

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/tasks"
)

// startPanickingChainTask starts the index task of a chain whose only
// part has no stat from its listing, so the task goroutine panics when
// it submits the part. runDetached fails the task in the table, and the
// run never records its end with the chain. It returns the task's ID
// once the task has ended, and the chain's key.
func startPanickingChainTask(t *testing.T, f *chainIndexFixture) (string, string) {
	t.Helper()
	handle := filepath.Join(f.root, "app.log")
	key := chainTaskKey(handle)
	task, _ := f.server.chainIndex.start(chainTaskStart{
		handle: handle, key: key, fingerprint: "00000000000000aa",
		parts: []logchain.Part{{Name: "app.log.1", Path: filepath.Join(f.root, "app.log.1")}},
	})
	end := awaitTaskEnd(t, f.manager, task.TaskID)
	if end.Status != tasks.StatusFailed || !strings.Contains(end.Error, "panic") {
		t.Fatalf("task %s %q, want failed by a panic", end.Status, end.Error)
	}
	return task.TaskID, key
}

// A chain index task whose goroutine panicked leaves its record without
// an end. While the task table still holds the task, pruning takes the
// end from the table, so the record is capped and expires like any
// ended one.
func TestChainIndexTasks_APanickedTaskTakesItsEndFromTheTable(t *testing.T) {
	f := newChainIndexFixture(t, 1, nil)
	c := f.server.chainIndex
	_, key := startPanickingChainTask(t, f)

	c.mu.Lock()
	c.pruneLocked()
	record, kept := c.records[key]
	c.mu.Unlock()

	if !kept || record.end == nil || record.end.status != tasks.StatusFailed || !strings.Contains(record.end.err, "panic") {
		t.Fatalf("record %+v kept=%v, want the failed end from the table", record, kept)
	}
}

// Once the task table has dropped a panicked chain task, its end is
// lost: pruning drops its record, whether or not the chain is ever
// described again.
func TestChainIndexTasks_APanickedTaskGoneFromTheTableIsPruned(t *testing.T) {
	f := newChainIndexFixture(t, 1, nil)
	c := f.server.chainIndex
	taskID, key := startPanickingChainTask(t, f)
	finishOtherTasks(f.manager, tasks.DefaultMaxTasks)
	if _, known := f.manager.Get(taskID); known {
		t.Fatal("the panicked task is still in the table; the test needs it gone")
	}

	c.mu.Lock()
	c.pruneLocked()
	_, kept := c.records[key]
	c.mu.Unlock()

	if kept {
		t.Fatal("the record of a panicked task the table no longer holds was kept")
	}
}

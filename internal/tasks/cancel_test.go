package tasks

import (
	"errors"
	"testing"
)

// A cancellable task holds its operation's own key for the path: a task
// of another operation on the same path is no conflict, a second
// cancellable task of the operation joins the first, and its context is
// live until the task is cancelled.
func TestManager_CreateCancellable_JoinsTheRunningTaskOfItsOperation(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	scan, ctx, isNew := m.CreateCancellable("/var/log/app.log", "findings")
	if !isNew || scan.Path != "/var/log/app.log" || scan.Operation != "findings" || scan.Status != StatusQueued {
		t.Fatalf("CreateCancellable = %+v new=%v, want a new queued findings task on the path", scan, isNew)
	}
	if ctx.Err() != nil {
		t.Fatalf("the context of a new task is done: %v", ctx.Err())
	}

	index, isNew := m.Create("/var/log/app.log", "index")
	if !isNew || index.TaskID == scan.TaskID {
		t.Fatalf("an index task on the scanned path was refused: %+v new=%v", index, isNew)
	}
	if holder, ok := m.CancellableHolder("/var/log/app.log", "findings"); !ok || holder.TaskID != scan.TaskID {
		t.Fatalf("CancellableHolder = %v, %v; want %s", holder, ok, scan.TaskID)
	}
	if _, ok := m.CancellableHolder("/var/log/app.log", "index"); ok {
		t.Fatal("CancellableHolder found a cancellable index task")
	}
	again, againCtx, isNew := m.CreateCancellable("/var/log/app.log", "findings")
	if isNew || again.TaskID != scan.TaskID {
		t.Fatalf("a second scan of the path: %s new=%v, want the running %s", again.TaskID, isNew, scan.TaskID)
	}
	if againCtx == nil {
		t.Fatal("a joined task gave no context")
	}

	m.Complete(scan.TaskID, nil)
	if next, _, isNew := m.CreateCancellable("/var/log/app.log", "findings"); !isNew || next.TaskID == scan.TaskID {
		t.Fatalf("after the end: %s new=%v, want a new task", next.TaskID, isNew)
	}
}

// Cancel ends a queued or running cancellable task at once: its context
// is canceled, its status is cancelled for good (the worker's later
// Fail or Complete does not change it), its done channel is closed, and
// its key is free for a new task.
func TestManager_Cancel_EndsTheTaskForGood(t *testing.T) {
	for _, running := range []bool{false, true} {
		m := New(Config{Logger: silentLogger()})
		task, ctx, _ := m.CreateCancellable("/var/log/app.log", "findings")
		if running {
			m.MarkRunning(task.TaskID)
		}
		watch, _ := m.Watch(task.TaskID)

		if err := m.Cancel(task.TaskID); err != nil {
			t.Fatalf("running=%v: Cancel = %v", running, err)
		}
		if ctx.Err() == nil {
			t.Fatalf("running=%v: the task's context is live after Cancel", running)
		}
		m.Fail(task.TaskID, "context canceled")
		m.Complete(task.TaskID, "late result")
		got, _ := m.Get(task.TaskID)
		if got.Status != StatusCancelled || got.Error != "" || got.Result != nil || got.CompletedAt == nil {
			t.Fatalf("running=%v: task after Cancel and late updates = %+v, want cancelled with no error and no result", running, got)
		}
		if !got.IsTerminal() {
			t.Fatalf("running=%v: a cancelled task is not terminal", running)
		}
		if !isClosed(watch.Done()) || watch.End().Status != StatusCancelled {
			t.Fatalf("running=%v: watch end = %+v, want cancelled", running, watch.End())
		}
		if _, ok := m.Holder(cancellableKey("findings", "/var/log/app.log")); ok {
			t.Fatalf("running=%v: the key is still held after Cancel", running)
		}
	}
}

// Cancel refuses what it cannot cancel: an unknown task, a task of an
// operation that was not created cancellable, and a task that has
// already ended.
func TestManager_Cancel_Refusals(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	index, _ := m.Create("/var/log/app.log", "index")
	ended, _, _ := m.CreateCancellable("/var/log/other.log", "findings")
	m.Complete(ended.TaskID, nil)

	cases := []struct {
		name string
		id   string
		want error
	}{
		{"unknown", "no-such-task", ErrTaskNotFound},
		{"not cancellable", index.TaskID, ErrNotCancellable},
		{"already ended", ended.TaskID, ErrTaskEnded},
	}
	for _, tc := range cases {
		if err := m.Cancel(tc.id); !errors.Is(err, tc.want) {
			t.Errorf("%s: Cancel = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got, _ := m.Get(index.TaskID); got.Status != StatusQueued {
		t.Fatalf("a refused Cancel changed the index task: %+v", got)
	}
	if got, _ := m.Get(ended.TaskID); got.Status != StatusCompleted {
		t.Fatalf("a refused Cancel changed the ended task: %+v", got)
	}
}

// A task that ends by itself releases its context, so nothing waits on
// it after the end.
func TestManager_CreateCancellable_EndReleasesTheContext(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, ctx, _ := m.CreateCancellable("/var/log/app.log", "findings")
	m.Fail(task.TaskID, "boom")
	if ctx.Err() == nil {
		t.Fatal("the context of a failed task is still live")
	}
	if got, _ := m.Get(task.TaskID); got.Status != StatusFailed || got.Error != "boom" {
		t.Fatalf("task = %+v, want failed with its error", got)
	}
}

// A task reports the bytes its worker read and their total through
// ReportBytes, read each time the task is.
func TestManager_ReportBytes_IsReadThroughTheTask(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _, _ := m.CreateCancellable("/x", "findings")
	if got, _ := m.Get(task.TaskID); got.Bytes() != nil {
		t.Fatalf("bytes = %+v before any source, want nil", got.Bytes())
	}
	read := int64(10)
	m.ReportBytes(task.TaskID, func() (int64, int64) { return read, 40 })
	got, _ := m.Get(task.TaskID)
	if b := got.Bytes(); b == nil || b.Read != 10 || b.Total != 40 {
		t.Fatalf("bytes = %+v, want 10 of 40", b)
	}
	read = 40
	if b := got.Bytes(); b.Read != 40 {
		t.Fatalf("bytes = %+v, want 40 of 40", b)
	}
	if m.ReportBytes("no-such-task", func() (int64, int64) { return 0, 0 }) {
		t.Fatal("ReportBytes found a task that does not exist")
	}
}

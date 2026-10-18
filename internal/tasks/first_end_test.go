package tasks

import (
	"testing"
)

// The first end of a task is final: a later Fail, Complete or
// MarkRunning on a completed or failed task is ignored (the call still
// returns true, the task exists), so Get and a watcher's End agree, the
// end time is the first one's, and the path lock is released once.
func TestManager_TheFirstEndIsFinal(t *testing.T) {
	t.Run("complete then fail", func(t *testing.T) {
		m := New(Config{Logger: silentLogger()})
		task, _ := m.Create("/a", "index")
		watch, _ := m.Watch(task.TaskID)
		if !m.Complete(task.TaskID, "the result") {
			t.Fatal("Complete returned false")
		}
		first, _ := m.Get(task.TaskID)
		if !m.Fail(task.TaskID, "too late") {
			t.Error("Fail of an ended task returned false, want true: the task exists")
		}
		got, _ := m.Get(task.TaskID)
		if got.Status != StatusCompleted || got.Error != "" || got.Result != "the result" {
			t.Errorf("after a late Fail: %+v, want completed with its result and no error", got)
		}
		if got.CompletedAt == nil || first.CompletedAt == nil || !got.CompletedAt.Equal(*first.CompletedAt) {
			t.Errorf("completed_at moved from %v to %v", first.CompletedAt, got.CompletedAt)
		}
		if end := watch.End(); end.Status != StatusCompleted || end.Error != "" {
			t.Errorf("Watch.End = %+v, want the first end", end)
		}
		if got := m.ActivePathLockCount(); got != 0 {
			t.Errorf("locks = %d, want 0", got)
		}
	})
	t.Run("fail then complete", func(t *testing.T) {
		m := New(Config{Logger: silentLogger()})
		task, _ := m.Create("/a", "index")
		m.Fail(task.TaskID, "the error")
		m.Complete(task.TaskID, "a result")
		got, _ := m.Get(task.TaskID)
		if got.Status != StatusFailed || got.Error != "the error" || got.Result != nil {
			t.Errorf("after a late Complete: %+v, want failed with its error and no result", got)
		}
	})
	t.Run("mark running after an end", func(t *testing.T) {
		m := New(Config{Logger: silentLogger()})
		task, _ := m.Create("/a", "index")
		m.Complete(task.TaskID, nil)
		// The path is free again: another task takes it. A second
		// release by the late update would free that task's lock.
		other, created := m.Create("/a", "index")
		if !created || other.TaskID == task.TaskID {
			t.Fatalf("the path was not free after the end: %+v, created %v", other, created)
		}
		m.MarkRunning(task.TaskID)
		got, _ := m.Get(task.TaskID)
		if got.Status != StatusCompleted {
			t.Errorf("after a late MarkRunning: status %s, want completed", got.Status)
		}
		done, _ := m.Done(task.TaskID)
		select {
		case <-done:
		default:
			t.Error("done is open again after a late MarkRunning")
		}
		if got := m.ActivePathLockCount(); got != 1 {
			t.Errorf("locks = %d, want the other task's one", got)
		}
	})
}

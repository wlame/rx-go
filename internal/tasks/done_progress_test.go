package tasks

import (
	"testing"
)

// isClosed reports whether ch is closed, without blocking.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A waiter that holds a task's done channel learns that the task ended
// whichever way it ended, and only then.
func TestManager_Done_ClosesWhenTheTaskEnds(t *testing.T) {
	endings := map[string]func(m *Manager, id string){
		"completed": func(m *Manager, id string) { m.Complete(id, map[string]any{"ok": true}) },
		"failed":    func(m *Manager, id string) { m.Fail(id, "boom") },
	}
	for name, end := range endings {
		t.Run(name, func(t *testing.T) {
			m := New(Config{Logger: silentLogger()})
			task, _ := m.Create("/x", "index")
			done, ok := m.Done(task.TaskID)
			if !ok {
				t.Fatal("Done did not find the task it just created")
			}
			m.MarkRunning(task.TaskID)
			if isClosed(done) {
				t.Fatal("done closed while the task runs")
			}
			end(m, task.TaskID)
			if !isClosed(done) {
				t.Fatal("done still open after the task ended")
			}
		})
	}
}

// A second terminal update (a panic recovered after Complete, say) must
// not close the channel twice, which would panic the server.
func TestManager_Done_SurvivesASecondTerminalUpdate(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _ := m.Create("/x", "index")
	m.Complete(task.TaskID, map[string]any{"ok": true})
	m.Fail(task.TaskID, "late failure")
	done, _ := m.Done(task.TaskID)
	if !isClosed(done) {
		t.Fatal("done is open for a finished task")
	}
}

func TestManager_Done_UnknownTask(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	if _, ok := m.Done("no-such-task"); ok {
		t.Fatal("Done found a task that does not exist")
	}
}

// A task reports no progress until its worker gives it a source, and
// then reports what the source says each time it is read.
func TestManager_ReportProgress_IsReadThroughTheTask(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _ := m.Create("/x", "index")
	if got, _ := m.Get(task.TaskID); got.Progress() != nil {
		t.Fatalf("progress = %v before any source, want nil", *got.Progress())
	}

	fraction := 0.25
	m.ReportProgress(task.TaskID, func() (float64, bool) { return fraction, true })
	got, _ := m.Get(task.TaskID)
	if p := got.Progress(); p == nil || *p != 0.25 {
		t.Fatalf("progress = %v, want 0.25", p)
	}
	fraction = 0.5
	if p := got.Progress(); p == nil || *p != 0.5 {
		t.Fatalf("progress = %v after the source moved, want 0.5", p)
	}
}

// A source that does not know its total yet reports no progress.
func TestManager_ReportProgress_UnknownIsNil(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _ := m.Create("/x", "index")
	m.ReportProgress(task.TaskID, func() (float64, bool) { return 0, false })
	got, _ := m.Get(task.TaskID)
	if p := got.Progress(); p != nil {
		t.Fatalf("progress = %v, want nil", *p)
	}
}

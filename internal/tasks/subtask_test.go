package tasks

import (
	"fmt"
	"testing"
	"time"
)

// finishTasks creates n tasks with create, ends each one at once and
// returns their IDs, oldest end first. The ends are a millisecond apart,
// since the end time orders which finished task the cap drops first.
func finishTasks(m *Manager, n int, create func(path, operation string) (*Task, bool)) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		task, _ := create(fmt.Sprintf("/logs/%d-%d.log", time.Now().UnixNano(), i), "index")
		m.Complete(task.TaskID, nil)
		ids = append(ids, task.TaskID)
		time.Sleep(time.Millisecond)
	}
	return ids
}

// Subtasks do part of another task's work, and a task may make
// thousands of them. Past the cap, creating one drops the oldest
// finished subtasks only: a finished task a client asked for stays.
func TestManager_CreateSubtask_DropsOnlyFinishedSubtasksPastTheCap(t *testing.T) {
	m := New(Config{Logger: silentLogger(), MaxTasks: 3})
	followed, _ := m.Create("/logs/followed.log", "compress")
	m.Complete(followed.TaskID, nil)

	subtasks := finishTasks(m, 5, m.CreateSubtask)

	if _, ok := m.Get(followed.TaskID); !ok {
		t.Fatal("five finished subtasks dropped the finished task a client follows")
	}
	for _, id := range subtasks[:2] {
		if _, ok := m.Get(id); ok {
			t.Errorf("subtask %s, among the oldest finished, was kept", id)
		}
	}
	for _, id := range subtasks[2:] {
		if _, ok := m.Get(id); !ok {
			t.Errorf("subtask %s, among the newest finished, was dropped", id)
		}
	}
	if got := m.Size(); got != 4 {
		t.Fatalf("Size = %d, want the followed task and 3 subtasks", got)
	}
}

// Tasks a client asks for are capped among themselves: creating one past
// the cap drops the oldest finished of them, never a subtask.
func TestManager_Create_NeverDropsASubtask(t *testing.T) {
	m := New(Config{Logger: silentLogger(), MaxTasks: 2})
	subtask := finishTasks(m, 1, m.CreateSubtask)[0]

	asked := finishTasks(m, 3, m.Create)

	if _, ok := m.Get(subtask); !ok {
		t.Fatal("tasks a client asked for dropped a subtask")
	}
	if _, ok := m.Get(asked[0]); ok {
		t.Error("the oldest finished task a client asked for was kept past the cap")
	}
	if got := m.Size(); got != 3 {
		t.Fatalf("Size = %d, want 2 asked-for tasks and the subtask", got)
	}
}

// A subtask holds its path as any task does: a second task for the path
// joins it while it runs.
func TestManager_CreateSubtask_HoldsItsPath(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	subtask, isNew := m.CreateSubtask("/logs/app.log.1", "index")
	if !isNew || subtask.Path != "/logs/app.log.1" || subtask.Operation != "index" {
		t.Fatalf("CreateSubtask = %+v new=%v", subtask, isNew)
	}
	if other, isNew := m.Create("/logs/app.log.1", "index"); isNew || other.TaskID != subtask.TaskID {
		t.Fatalf("a task for the subtask's path: %s new=%v, want the running %s", other.TaskID, isNew, subtask.TaskID)
	}
	if again, isNew := m.CreateSubtask("/logs/app.log.1", "index"); isNew || again.TaskID != subtask.TaskID {
		t.Fatalf("a second subtask for the path: %s new=%v, want the running %s", again.TaskID, isNew, subtask.TaskID)
	}
}

// A watch says how its task ended once the task's done channel closes,
// even after the cap has dropped the task from the table: the end is
// kept on the watch, not looked up.
func TestManager_Watch_KeepsTheEndAfterTheTaskLeftTheTable(t *testing.T) {
	endings := []struct {
		name string
		end  func(m *Manager, id string)
		want End
	}{
		{"completed", func(m *Manager, id string) { m.Complete(id, map[string]any{"ok": true}) }, End{Status: StatusCompleted}},
		{"failed", func(m *Manager, id string) { m.Fail(id, "boom") }, End{Status: StatusFailed, Error: "boom"}},
	}
	for _, tc := range endings {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Config{Logger: silentLogger(), MaxTasks: 1})
			task, _ := m.CreateSubtask("/logs/app.log.1", "index")
			watch, ok := m.Watch(task.TaskID)
			if !ok {
				t.Fatal("Watch did not find the task it was given")
			}
			if isClosed(watch.Done()) {
				t.Fatal("done closed before the task ended")
			}
			m.MarkRunning(task.TaskID)
			tc.end(m, task.TaskID)
			finishTasks(m, 1, m.CreateSubtask)
			if _, known := m.Get(task.TaskID); known {
				t.Fatal("the cap kept the task; the test needs it dropped")
			}

			if !isClosed(watch.Done()) {
				t.Fatal("done still open after the task ended")
			}
			if got := watch.End(); got != tc.want {
				t.Fatalf("End() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The end is how the task first ended: a later update does not rewrite
// what a watcher reads.
func TestManager_Watch_EndIsTheFirstEnd(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _ := m.Create("/logs/app.log", "index")
	watch, _ := m.Watch(task.TaskID)
	m.Fail(task.TaskID, "boom")
	m.Update(task.TaskID, StatusCompleted, "", nil)

	if got, want := watch.End(), (End{Status: StatusFailed, Error: "boom"}); got != want {
		t.Fatalf("End() = %+v, want the first end %+v", got, want)
	}
}

func TestManager_Watch_UnknownTask(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	if _, ok := m.Watch("no-such-task"); ok {
		t.Fatal("Watch found a task that does not exist")
	}
}

func TestManager_TTL_IsTheConfiguredRetention(t *testing.T) {
	m := New(Config{Logger: silentLogger(), TTL: 7 * time.Minute})
	if got := m.TTL(); got != 7*time.Minute {
		t.Fatalf("TTL() = %v, want 7m", got)
	}
}

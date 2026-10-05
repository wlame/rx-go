package tasks

import "testing"

// A task can hold several paths. While it runs, a new task for any of
// them is refused with the running task and the path it holds; the
// paths it does not hold stay free.
func TestManager_CreateHolding_RefusesEveryHeldPath(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	first, held, isNew := m.CreateHolding("compress", "/logs/app.log", "/logs/app.log.zst")
	if !isNew || held != "" {
		t.Fatalf("first CreateHolding: isNew=%v held=%q, want a new task", isNew, held)
	}
	if first.Path != "/logs/app.log" {
		t.Errorf("Path = %q, want the first path held", first.Path)
	}

	cases := []struct {
		name     string
		paths    []string
		wantHeld string
	}{
		{"same output", []string{"/logs/app.log.gz", "/logs/app.log.zst"}, "/logs/app.log.zst"},
		{"index of the output", []string{"/logs/app.log.zst"}, "/logs/app.log.zst"},
		{"same input", []string{"/logs/app.log", "/out/app.log.zst"}, "/logs/app.log"},
	}
	for _, tc := range cases {
		running, held, isNew := m.CreateHolding("index", tc.paths...)
		if isNew || running.TaskID != first.TaskID || held != tc.wantHeld {
			t.Errorf("%s: got task %s held %q new=%v, want task %s held %q",
				tc.name, running.TaskID, held, isNew, first.TaskID, tc.wantHeld)
		}
	}
	if _, _, isNew := m.CreateHolding("compress", "/logs/other.log", "/logs/other.log.zst"); !isNew {
		t.Error("a task holding other paths was refused")
	}
	if got := m.ActivePathLockCount(); got != 4 {
		t.Errorf("ActivePathLockCount = %d, want 4 (two tasks, two paths each)", got)
	}
}

// Every path a task holds is released when it ends, and when the
// sweeper drops it.
func TestManager_CreateHolding_ReleasesEveryPath(t *testing.T) {
	end := map[string]func(m *Manager, id string){
		"complete": func(m *Manager, id string) { m.Complete(id, map[string]any{}) },
		"fail":     func(m *Manager, id string) { m.Fail(id, "boom") },
	}
	for name, finish := range end {
		t.Run(name, func(t *testing.T) {
			m := New(Config{Logger: silentLogger()})
			task, _, _ := m.CreateHolding("compress", "/a.log", "/a.log.zst")

			finish(m, task.TaskID)

			if got := m.ActivePathLockCount(); got != 0 {
				t.Errorf("ActivePathLockCount = %d after the task ended, want 0", got)
			}
			for _, path := range []string{"/a.log", "/a.log.zst"} {
				if _, _, isNew := m.CreateHolding("index", path); !isNew {
					t.Errorf("%s is still held after the task ended", path)
				}
			}
		})
	}
}

// A path given twice is held once.
func TestManager_CreateHolding_SamePathTwiceIsHeldOnce(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _, isNew := m.CreateHolding("compress", "/a.log", "/a.log")
	if !isNew {
		t.Fatal("CreateHolding refused a path given twice")
	}
	if got := m.ActivePathLockCount(); got != 1 {
		t.Errorf("ActivePathLockCount = %d, want 1", got)
	}
	m.Complete(task.TaskID, map[string]any{})
	if got := m.ActivePathLockCount(); got != 0 {
		t.Errorf("ActivePathLockCount = %d after the task ended, want 0", got)
	}
}

// Holder names the unfinished task that holds a path, and creates
// nothing: a path that is free, or whose task has ended, has none.
func TestManager_Holder_NamesTheRunningTaskOnly(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _, _ := m.CreateHolding("compress", "/logs/app.log", "/logs/app.log.zst")
	for _, path := range []string{"/logs/app.log", "/logs/app.log.zst"} {
		if holder, ok := m.Holder(path); !ok || holder.TaskID != task.TaskID {
			t.Errorf("Holder(%s) = %v, %v; want task %s", path, holder, ok, task.TaskID)
		}
	}
	if holder, ok := m.Holder("/logs/other.log"); ok {
		t.Errorf("Holder of a free path = %s, want none", holder.TaskID)
	}
	m.Complete(task.TaskID, nil)
	if holder, ok := m.Holder("/logs/app.log"); ok {
		t.Errorf("Holder after the task ended = %s, want none", holder.TaskID)
	}
	if size := m.Size(); size != 1 {
		t.Errorf("Size = %d, want 1: Holder creates nothing", size)
	}
}

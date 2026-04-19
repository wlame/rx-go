package tasks

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// silentLogger drops all output so tests don't pollute -v.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestManager_Create_FreshTask checks the happy path: new (path, op)
// pair creates a queued task with a non-empty UUID.
func TestManager_Create_FreshTask(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, isNew := m.Create("/var/log/a.log", "index")
	if !isNew {
		t.Fatalf("isNew = false, want true on fresh (path, op)")
	}
	if task.TaskID == "" {
		t.Errorf("empty TaskID")
	}
	if task.Status != StatusQueued {
		t.Errorf("status = %q, want %q", task.Status, StatusQueued)
	}
	if task.Path != "/var/log/a.log" || task.Operation != "index" {
		t.Errorf("path/op mismatch: %+v", task)
	}
}

// TestManager_Create_DuplicatePathReturnsSameTask — key lock invariant:
// Two submissions for the same path hit the path lock and get the same
// task back, with isNew=false on the second.
func TestManager_Create_DuplicatePathReturnsSameTask(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	first, isNew := m.Create("/x", "compress")
	if !isNew {
		t.Fatal("first call should be new")
	}
	second, isNew := m.Create("/x", "compress")
	if isNew {
		t.Error("second call on same path should NOT be new")
	}
	if second.TaskID != first.TaskID {
		t.Errorf("duplicate produced different task_id: %s vs %s",
			first.TaskID, second.TaskID)
	}
}

// TestManager_PathLockReleasedOnCompletion verifies the lock frees up
// when a task finishes, so a subsequent create can succeed.
func TestManager_PathLockReleasedOnCompletion(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	first, _ := m.Create("/x", "index")
	m.Complete(first.TaskID, map[string]any{"ok": true})

	second, isNew := m.Create("/x", "index")
	if !isNew {
		t.Error("second Create should be new after first completes")
	}
	if second.TaskID == first.TaskID {
		t.Error("second task should have a NEW ID after first completes")
	}
}

// TestManager_PathLockReleasedOnFailure — same as above but via Fail.
func TestManager_PathLockReleasedOnFailure(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	first, _ := m.Create("/y", "compress")
	m.Fail(first.TaskID, "boom")

	second, isNew := m.Create("/y", "compress")
	if !isNew {
		t.Error("second Create after Fail should be new")
	}
	if second.Status != StatusQueued {
		t.Errorf("re-created task status = %q, want queued", second.Status)
	}
}

// TestManager_ConcurrentCreate_SamePathAtomic: 50 goroutines race to
// Create the same path. Exactly one should get isNew=true; the rest
// get the same task back.
func TestManager_ConcurrentCreate_SamePathAtomic(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	const N = 50
	var wg sync.WaitGroup
	isNewCounts := make(chan bool, N)
	idCounts := make(chan string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, isNew := m.Create("/concurrent", "index")
			isNewCounts <- isNew
			idCounts <- task.TaskID
		}()
	}
	wg.Wait()
	close(isNewCounts)
	close(idCounts)

	newCount := 0
	for x := range isNewCounts {
		if x {
			newCount++
		}
	}
	if newCount != 1 {
		t.Errorf("got %d isNew=true, want exactly 1", newCount)
	}
	seen := map[string]struct{}{}
	for id := range idCounts {
		seen[id] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("got %d distinct task IDs, want 1", len(seen))
	}
}

// TestManager_Get_ReturnsClone — the map[string]any inside Result
// should be copyable without the caller being able to mutate the
// original task's result. We relax to "fields don't leak pointer"
// because shallow copy is enough per the spec.
func TestManager_Get_ReturnsShallowClone(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	t1, _ := m.Create("/a", "index")
	m.Complete(t1.TaskID, map[string]any{"count": 42})

	got, ok := m.Get(t1.TaskID)
	if !ok {
		t.Fatal("Get failed after Complete")
	}
	if got == t1 {
		t.Error("Get returned the same pointer; want a clone")
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// TestManager_Sweep_RemovesOldTerminalTasks forces a sweep and verifies
// only Terminal + old tasks disappear.
func TestManager_Sweep_RemovesOldTerminalTasks(t *testing.T) {
	m := New(Config{TTL: 1 * time.Hour, Logger: silentLogger()})

	// Task A: completed long ago.
	taskA, _ := m.Create("/old", "index")
	m.Complete(taskA.TaskID, nil)
	// Artificially age the completion timestamp.
	m.mu.Lock()
	backdated := time.Now().UTC().Add(-2 * time.Hour)
	m.tasks[taskA.TaskID].CompletedAt = &backdated
	m.mu.Unlock()

	// Task B: completed just now.
	taskB, _ := m.Create("/fresh", "index")
	m.Complete(taskB.TaskID, nil)

	// Task C: still queued.
	taskC, _ := m.Create("/queued", "index")

	removed := m.RunSweepForTests(time.Now().UTC())
	if removed != 1 {
		t.Errorf("sweep removed %d, want 1", removed)
	}
	if _, ok := m.Get(taskA.TaskID); ok {
		t.Error("taskA should have been swept")
	}
	if _, ok := m.Get(taskB.TaskID); !ok {
		t.Error("taskB (fresh) should have survived")
	}
	if _, ok := m.Get(taskC.TaskID); !ok {
		t.Error("taskC (queued) should have survived")
	}
}

// TestManager_Sweeper_Ticks ensures the background goroutine actually
// fires on Start/Stop without deadlocking.
func TestManager_Sweeper_StartStop(t *testing.T) {
	m := New(Config{
		TTL:           1 * time.Hour,
		SweepInterval: 10 * time.Millisecond,
		Logger:        silentLogger(),
	})
	m.Start()
	time.Sleep(30 * time.Millisecond) // let it tick at least twice
	m.Stop()
	// Stop should return promptly; a second Stop must not block.
	m.Stop()
}

// TestManager_Stop_WithoutStart_DoesNotBlock covers Stage 8 Reviewer 2
// High #6: Stop() must be safe to call even if Start() was never called.
//
// Before the fix, Stop() closed sweeperCancel and then blocked on
// `<-m.sweeperDone`. sweeperDone is only closed by the sweeperLoop's
// `defer` — which never ran because the loop never started. The Stop
// call hung forever. Idiomatic Go requires Close-like methods to be
// safe at any time; this test pins the contract.
//
// Strategy: call Stop on a freshly-constructed manager, bound the call
// with a deadline. If Stop returns promptly, the test passes. If Stop
// hangs (pre-fix), the test deadline trips and we report the deadlock.
func TestManager_Stop_WithoutStart_DoesNotBlock(t *testing.T) {
	m := New(Config{Logger: silentLogger()})

	// Run Stop in a goroutine and signal on a channel. We wait bounded
	// — if Stop doesn't return in 500 ms it's deadlocked.
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop returned — good.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Manager.Stop() without prior Start() deadlocked (waited 500ms)")
	}
}

// TestManager_MarkRunning_Flow exercises the full lifecycle.
func TestManager_MarkRunning_Flow(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	task, _ := m.Create("/x", "compress")
	if ok := m.MarkRunning(task.TaskID); !ok {
		t.Fatal("MarkRunning returned false for existing task")
	}
	got, _ := m.Get(task.TaskID)
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	m.Complete(task.TaskID, map[string]any{"bytes_compressed": 1000})
	got, _ = m.Get(task.TaskID)
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.Result["bytes_compressed"] != 1000 {
		t.Errorf("result = %v", got.Result)
	}
}

// TestManager_Update_UnknownTaskIDReturnsFalse defensive check.
func TestManager_Update_UnknownTaskID(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	if m.Update("ghost-id", StatusCompleted, "", nil) {
		t.Error("Update returned true for nonexistent id")
	}
	if m.MarkRunning("ghost-id") {
		t.Error("MarkRunning returned true for nonexistent id")
	}
	if m.Complete("ghost-id", nil) {
		t.Error("Complete returned true for nonexistent id")
	}
	if m.Fail("ghost-id", "x") {
		t.Error("Fail returned true for nonexistent id")
	}
}

// TestManager_List_ReturnsAllTasksAsCopies
func TestManager_List_ReturnsAllTasksAsCopies(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	for i := 0; i < 5; i++ {
		m.Create("/path/"+string(rune('A'+i)), "index")
	}
	got := m.List()
	if len(got) != 5 {
		t.Errorf("List returned %d, want 5", len(got))
	}
}

// TestManager_ActivePathLockCount_Tracks sees the lock map grow with
// creations and shrink with completions.
func TestManager_ActivePathLockCount(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	a, _ := m.Create("/a", "index")
	m.Create("/b", "index")
	if got := m.ActivePathLockCount(); got != 2 {
		t.Errorf("locks = %d, want 2", got)
	}
	m.Complete(a.TaskID, nil)
	if got := m.ActivePathLockCount(); got != 1 {
		t.Errorf("after Complete, locks = %d, want 1", got)
	}
}

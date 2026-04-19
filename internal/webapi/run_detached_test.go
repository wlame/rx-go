package webapi

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/tasks"
)

// silentTaskLogger returns a slog.Logger that discards output. Keeps
// test console clean when intentionally causing panics.
func silentTaskLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunDetached_NormalCompletion verifies the happy path: the helper
// runs the work function and returns once it completes. No task-manager
// interaction beyond what the work function itself does.
func TestRunDetached_NormalCompletion(t *testing.T) {
	mgr := tasks.New(tasks.Config{Logger: silentTaskLogger()})
	task, _ := mgr.Create("/happy", "index")

	var called bool
	done := make(chan struct{})
	go runDetached(mgr, task.TaskID, "index", silentTaskLogger(), func() {
		called = true
		mgr.MarkRunning(task.TaskID)
		mgr.Complete(task.TaskID, map[string]any{"ok": true})
		close(done)
	})

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runDetached's work function never executed")
	}
	if !called {
		t.Fatal("work function was not invoked")
	}

	got, _ := mgr.Get(task.TaskID)
	if got.Status != tasks.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// TestRunDetached_PanicTransitionsTaskToFailed covers Stage 8
// Reviewer 3 High #1: a panic inside a detached goroutine must NOT
// crash the server. runDetached must:
//
//   - Catch the panic with defer+recover.
//   - Log it via slog.
//   - Mark the task as Failed with an error string including the
//     panic message so clients can diagnose.
//   - Allow the goroutine to exit cleanly.
//
// Before the fix there was no runDetached helper — bare `go runTask(...)`
// meant an unrecovered panic killed the process. The test uses a
// recover() trampoline at test boundary: if the goroutine panics
// without being caught, the test process itself aborts and the test
// harness records the goroutine's panic stacktrace.
func TestRunDetached_PanicTransitionsTaskToFailed(t *testing.T) {
	mgr := tasks.New(tasks.Config{Logger: silentTaskLogger()})
	task, _ := mgr.Create("/panicky", "index")

	// Capture log output so we can assert the panic was logged.
	var logMu sync.Mutex
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logBuf.Write(p)
	}), nil))

	done := make(chan struct{})
	go func() {
		runDetached(mgr, task.TaskID, "index", logger, func() {
			mgr.MarkRunning(task.TaskID)
			panic("intentional test panic: boom")
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runDetached did not return after panic — server would have died")
	}

	// Task must now be Failed with the panic message surfaced.
	got, _ := mgr.Get(task.TaskID)
	if got.Status != tasks.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "boom") {
		t.Errorf("Error = %q, expected to contain the panic message 'boom'", got.Error)
	}
	if !strings.Contains(got.Error, "panic") {
		t.Errorf("Error = %q, expected to contain the word 'panic'", got.Error)
	}

	// Log must mention the task and the panic.
	logMu.Lock()
	logText := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logText, task.TaskID) {
		t.Errorf("log output missing task_id %q: %s", task.TaskID, logText)
	}
	if !strings.Contains(logText, "panic") {
		t.Errorf("log output missing 'panic' keyword: %s", logText)
	}
}

// TestRunDetached_PanicDoesNotAffectOtherTasks is the "server keeps
// running" smoke test. A panicking task transitions to Failed; a
// subsequent healthy task completes normally and is reflected in the
// manager's state.
func TestRunDetached_PanicDoesNotAffectOtherTasks(t *testing.T) {
	mgr := tasks.New(tasks.Config{Logger: silentTaskLogger()})

	// Task A: panics.
	taskA, _ := mgr.Create("/a", "index")
	doneA := make(chan struct{})
	go func() {
		runDetached(mgr, taskA.TaskID, "index", silentTaskLogger(), func() {
			mgr.MarkRunning(taskA.TaskID)
			panic("crash A")
		})
		close(doneA)
	}()
	<-doneA

	// Task B: completes normally AFTER A panicked.
	taskB, _ := mgr.Create("/b", "compress")
	doneB := make(chan struct{})
	go func() {
		runDetached(mgr, taskB.TaskID, "compress", silentTaskLogger(), func() {
			mgr.MarkRunning(taskB.TaskID)
			mgr.Complete(taskB.TaskID, nil)
		})
		close(doneB)
	}()
	select {
	case <-doneB:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("task B never completed — panic in A corrupted manager state?")
	}

	gotA, _ := mgr.Get(taskA.TaskID)
	gotB, _ := mgr.Get(taskB.TaskID)
	if gotA.Status != tasks.StatusFailed {
		t.Errorf("A.status = %q, want failed", gotA.Status)
	}
	if gotB.Status != tasks.StatusCompleted {
		t.Errorf("B.status = %q, want completed", gotB.Status)
	}
}

// writerFunc adapts a function to an io.Writer. Used above to capture
// slog output into a buffer while avoiding a dependency on a bigger
// test-helper package.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

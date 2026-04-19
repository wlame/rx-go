package webapi

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/wlame/rx-go/internal/tasks"
)

// runDetached wraps the body of a background (detached-from-request)
// task goroutine with a panic-recovery boundary. Any panic inside `work`
// is:
//
//  1. Recovered so it does NOT propagate to runtime.goexit and crash
//     the entire HTTP server process. The stdlib panic-handler kills
//     the program on an uncaught goroutine panic (unlike the request
//     goroutines, which the chi/huma recover middleware protects).
//  2. Logged at error level with the task_id, operation, panic value,
//     and a full stack trace for diagnosis.
//  3. Reflected in the task manager by marking the task Failed with
//     a user-visible error string that includes the panic message.
//     Clients polling GET /v1/tasks/{id} will see Status=failed and
//     a diagnostic Error field.
//
// PATH-LOCK INVARIANT: mgr.Fail routes through tasks.Manager.Update,
// which releases the per-path lock once the task reaches a terminal
// state (see internal/tasks/manager.go — the Update function checks
// task.IsTerminal() and deletes the entry from pathLocks iff the
// current task still holds it). A retry request targeting the same
// path therefore succeeds without operator intervention: the failed-
// task entry remains visible in the task list for diagnosis, but the
// lock is free. If you ever refactor tasks.Manager to split Fail
// from lock release, this contract MUST be preserved or the server
// will leak locks on panicking jobs.
//
// This addresses Stage 8 Reviewer 3 High #1: before this helper existed,
// a bare `go runIndexTask(...)` call could crash the whole server when
// index.Build hit a malformed file that triggered a runtime panic
// (slice-out-of-bounds, nil deref from a corrupted input, etc.).
//
// USAGE: replace
//
//	go runIndexTask(mgr, id, ...)
//
// with
//
//	go runDetached(mgr, id, "index", logger, func() {
//	    runIndexTask(mgr, id, ...)
//	})
//
// The cost is negligible (one deferred func call + one closure) and the
// reliability gain is substantial — any future detached goroutine
// should use this wrapper.
func runDetached(mgr *tasks.Manager, taskID, operation string, logger *slog.Logger, work func()) {
	defer func() {
		r := recover()
		if r == nil {
			// Normal completion — work() returned. Nothing to do.
			return
		}
		// Capture the stack trace at the panic site (not the recover
		// site) for the log. debug.Stack() walks runtime.Callers from
		// the defer frame, which is where the panic is currently
		// propagating, so the trace points at the true culprit.
		stack := debug.Stack()

		// Compose a user-visible error string. Include the panic
		// payload verbatim so the failure reason is obvious in the
		// client's poll response.
		errMsg := fmt.Sprintf("panic during %s task: %v", operation, r)

		// Mark the task as failed. If the task is already terminal for
		// some reason (shouldn't happen — work() usually panics before
		// calling Complete) the manager silently ignores the update.
		mgr.Fail(taskID, errMsg)

		// Log everything — operators rely on structured logs to diagnose
		// unexpected panics.
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("background_task_panic",
			"task_id", taskID,
			"operation", operation,
			"panic", fmt.Sprintf("%v", r),
			"stack", string(stack),
		)
	}()
	work()
}

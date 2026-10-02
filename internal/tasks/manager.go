// Package tasks implements the background-task manager that backs
// POST /v1/index, POST /v1/compress, the index builds GET /v1/samples
// waits for and the index task of a log chain — long-running
// operations that the HTTP layer
// wants to run asynchronously and have the client poll via
// GET /v1/tasks/{id}.
//
// Features from the Python port (rx-python/src/rx/task_manager.py):
//
//   - In-memory task store keyed by task_id (UUID v4).
//   - Path locking: while a task runs, it holds its path, and no other
//     task can start for that path, whatever its operation; a duplicate
//     submission returns the SAME task ID (idempotent POST). A task can
//     hold more than one path (CreateHolding): a compression holds its
//     input and its output. A task can also hold a key that is not its
//     path (CreateKeyed): a log chain's index task holds a key made of
//     `chain:`, its directory's device and inode, and its name.
//   - Sweeper goroutine: every 5 minutes, removes completed/failed
//     tasks older than RX_TASK_TTL_MINUTES (default 60).
//   - A cap on the finished tasks (DefaultMaxTasks): past it, creating a
//     task drops the oldest finished ones; unfinished tasks are not
//     counted. Subtasks (CreateSubtask), the
//     part builds of a log chain's index task, are capped apart, so a
//     task that makes thousands of them never drops another one.
//   - A watch (Watch) that keeps how a task ended, for a caller that
//     reads it after the cap or the sweeper may have dropped the task.
//   - Cancellable tasks (CreateCancellable): a signature scan runs under
//     a context its task keeps, and Cancel stops it and ends the task
//     as cancelled (DELETE /v1/tasks/{task_id}). Such a task holds a key
//     of its operation and path, so it runs beside an index build or a
//     compression of the same file.
//
// Task store backing: Python uses an asyncio.Lock with dict mutation.
// This package uses one sync.Mutex guarding a map[string]*Task for the
// tasks and a map[string]string for the path locks. A sync.Map would
// work too, but the "is this path already locked?" check-and-insert
// has to be atomic, and one lock around plain maps is simpler to
// reason about.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/wlame/rx-go/internal/config"
)

// ============================================================================
// Tunables
// ============================================================================

// DefaultTTL is how long a finished (completed/failed) task stays in
// memory before the sweeper removes it. Mirrors Python's 60 minutes.
// Override via RX_TASK_TTL_MINUTES.
const DefaultTTL = 60 * time.Minute

// DefaultMaxTasks is how many finished tasks of one class the manager
// keeps at most: tasks a client asked for, and subtasks
// (CreateSubtask). A finished task keeps its whole result, the line
// index included, until the sweeper removes it after the TTL; without a
// cap, a burst of requests inside one TTL would grow the table without
// bound. Past the cap, creating a task drops the oldest finished tasks
// of its own class. Running and queued tasks are never dropped and do
// not count toward the cap: their number is bounded where they are
// started (one per path, the samples build queue), and counting them
// would let a burst of them push out every finished task a client still
// polls.
const DefaultMaxTasks = 256

// DefaultSweepInterval is how often the sweeper goroutine wakes up.
// Python uses 5 minutes; we match.
const DefaultSweepInterval = 5 * time.Minute

// ttlFromEnv returns the effective TTL: RX_TASK_TTL_MINUTES, from one
// minute to one week, or 60 minutes.
func ttlFromEnv() time.Duration {
	return config.TaskTTL()
}

// ============================================================================
// Task types
// ============================================================================

// Status is the lifecycle state of a background task.
type Status string

// Task lifecycle:
//
//	Queued -> Running -> (Completed | Failed)
//	Queued or Running -> Cancelled (Cancel, cancellable tasks only)
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	// StatusCancelled is the end of a cancellable task stopped by
	// Cancel. It is final: a later Update of the task changes nothing.
	StatusCancelled Status = "cancelled"
)

// Task holds the metadata and result of one background job. All
// fields except TaskID are mutable after creation; concurrent access
// is serialized through Manager's lock.
type Task struct {
	TaskID      string
	Path        string
	Operation   string // "index" | "compress" | "chain_index" | "findings"
	Status      Status
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       string
	// Result is the value the worker completed the task with (an
	// rxtypes.IndexTaskResult, rxtypes.CompressTaskResult or
	// rxtypes.ChainIndexTaskResult); nil until then. The manager stores
	// it without looking inside.
	Result any

	// done is closed when the task first reaches a terminal status.
	// Go note: a closed channel is a broadcast. Every goroutine blocked
	// in a receive on it (`<-done`, or a `select` case) wakes at once,
	// and every later receive returns immediately, so any number of
	// waiters can watch one task without registering anywhere. Shared by
	// the clones Get returns, which is what lets a caller wait on it
	// outside the manager's lock.
	done chan struct{}

	// progress, when set, says how far the task's work has got; see
	// ReportProgress. Shared by the clones too.
	progress ProgressFunc

	// bytes, when set, says how many bytes the task's work has read and
	// of how many; see ReportBytes. Shared by the clones too.
	bytes ByteProgressFunc

	// ctx and cancel are set for a cancellable task (CreateCancellable)
	// and nil for any other: the worker runs under ctx, and Cancel (or
	// the task's end) calls cancel. Set at creation and never changed,
	// so the clones may share them.
	//
	// Go note: a context.Context is how Go passes "stop now" down a call
	// chain. Calling its CancelFunc closes ctx.Done() and makes ctx.Err()
	// non-nil; every function that was given ctx and checks it (the
	// signature scan between chunks, a wait in the scan queue) then
	// returns early with that error.
	ctx    context.Context
	cancel context.CancelFunc

	// settled says the task's work is past the point where it can stop
	// (Settle): Cancel refuses it from then on.
	settled bool

	// held lists every key the task holds in the manager's path locks,
	// each once: its paths, Path first (CreateHolding), or the one key
	// of a keyed task (CreateKeyed), which need not be Path. Set at
	// creation and never changed, so the clones may share it.
	held []string

	// subtask says the task does part of another task's work
	// (CreateSubtask): it counts toward the cap of the subtasks, not of
	// the tasks a client asked for. Set at creation and never changed.
	subtask bool

	// end is how the task first ended, written under the manager's lock
	// just before done is closed and never after. Shared by the clones
	// and by every Watch of the task, which read it once done is closed:
	// a watcher learns the end even after the task left the table.
	end *End
}

// End is how a task ended: its terminal status and its error, as they
// stood when it first reached a terminal status.
type End struct {
	Status Status
	Error  string
}

// Watch follows one task to its end. It holds the task's done channel
// and its end, not the task's place in the table, so it still says how
// the task ended after the cap or the sweeper has dropped the task.
//
// Go note: the zero Watch has a nil done channel, and a receive from a
// nil channel blocks for ever; take a Watch only from Manager.Watch.
type Watch struct {
	done <-chan struct{}
	end  *End
}

// Done returns a channel that is closed when the task ends, completed,
// failed or cancelled; Manager.Done gives the same channel.
func (w Watch) Done() <-chan struct{} { return w.done }

// End returns how the task ended. Call it only once Done's channel is
// closed: before that the end is not written yet, and reading it while
// the manager writes it would be a data race.
//
// Go note: the end is written before the channel is closed, and the Go
// memory model orders a channel's close before every receive that
// returns because of it, so a reader that has received from Done sees
// the whole end without taking a lock.
func (w Watch) End() End { return *w.end }

// ProgressFunc reports how far a task's work has got, as a fraction
// from 0 to 1, and false while that is not known. It is called from the
// goroutine that serves a status request while the worker goroutine
// keeps working, so it must be safe to call concurrently with the work
// (an atomic counter read, typically).
type ProgressFunc func() (fraction float64, known bool)

// Progress returns the fraction of the task's work done, or nil when
// the task reports none.
func (t *Task) Progress() *float64 {
	if t.progress == nil {
		return nil
	}
	fraction, known := t.progress()
	if !known {
		return nil
	}
	return &fraction
}

// ByteProgressFunc reports how many bytes a task's work has read so far
// and how many it will read in all. Like ProgressFunc it is called from
// the goroutine that serves a status request while the worker keeps
// working, so it must be safe to call concurrently with the work.
type ByteProgressFunc func() (read, total int64)

// ByteProgress is how many bytes a task's work has read, of how many.
type ByteProgress struct {
	Read, Total int64
}

// Bytes returns the bytes the task's work has read and their total, or
// nil when the task reports none.
func (t *Task) Bytes() *ByteProgress {
	if t.bytes == nil {
		return nil
	}
	read, total := t.bytes()
	return &ByteProgress{Read: read, Total: total}
}

// IsTerminal reports whether the task is done (completed, failed or
// cancelled). Used by the sweeper and by HTTP 409 logic.
func (t *Task) IsTerminal() bool {
	return t.Status == StatusCompleted || t.Status == StatusFailed || t.Status == StatusCancelled
}

// ============================================================================
// Manager
// ============================================================================

// Manager owns the task store and the path-lock map. Safe for
// concurrent use. Construct via New; call Start to begin the sweeper
// and Stop to halt it on shutdown.
type Manager struct {
	mu sync.Mutex
	// tasks: task_id -> *Task. Guarded by mu.
	tasks map[string]*Task
	// pathLocks: normalized_path -> task_id. Guarded by mu.
	// Locks are released when a task transitions to Terminal.
	pathLocks map[string]string

	// maxTasks caps the finished tasks of each class; see DefaultMaxTasks.
	maxTasks int

	// Sweeper control.
	ttl           time.Duration
	sweepInterval time.Duration
	sweeperCancel chan struct{}
	sweeperDone   chan struct{}
	sweeperOnce   sync.Once
	// started is flipped to true by Start() before launching the
	// sweeper goroutine. Stop() consults this flag to decide whether
	// to wait on sweeperDone. Without the flag, Stop() on a manager
	// whose Start() was never called would deadlock waiting on a
	// channel that's only closed by the (never-running) sweeperLoop.
	//.
	started atomic.Bool

	logger *slog.Logger
}

// Config passes optional Manager settings.
type Config struct {
	TTL           time.Duration // finished task retention; 0 = env/default
	SweepInterval time.Duration // sweeper interval; 0 = default
	MaxTasks      int           // finished tasks kept per class; 0 = DefaultMaxTasks
	Logger        *slog.Logger
}

// New constructs a Manager. Does not start the sweeper — call Start.
func New(cfg Config) *Manager {
	if cfg.TTL <= 0 {
		cfg.TTL = ttlFromEnv()
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = DefaultSweepInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxTasks <= 0 {
		cfg.MaxTasks = DefaultMaxTasks
	}
	return &Manager{
		maxTasks:      cfg.MaxTasks,
		tasks:         map[string]*Task{},
		pathLocks:     map[string]string{},
		ttl:           cfg.TTL,
		sweepInterval: cfg.SweepInterval,
		sweeperCancel: make(chan struct{}),
		sweeperDone:   make(chan struct{}),
		logger:        cfg.Logger,
	}
}

// Start launches the sweeper goroutine. Idempotent: subsequent calls
// are no-ops. Sets the `started` flag BEFORE kicking off the goroutine
// so a racing Stop() will always observe the running state.
func (m *Manager) Start() {
	m.sweeperOnce.Do(func() {
		m.started.Store(true)
		go m.sweeperLoop()
	})
}

// Stop halts the sweeper and waits for it to exit. Safe to call even
// if Start was never called — we short-circuit on the `started` flag
// to avoid blocking on `sweeperDone`, which only closes from inside
// the sweeper loop's `defer`. Without this guard a fresh
// New()-but-never-Start()-ed Manager would deadlock on Stop().
//
// Also safe to call multiple times: the sweeperCancel channel acts
// as a one-shot signal; the second Stop sees the already-closed
// channel and returns.
func (m *Manager) Stop() {
	if !m.started.Load() {
		// Start() was never called — the sweeper goroutine doesn't
		// exist, so there's nothing to wait on. Return immediately.
		return
	}
	select {
	case <-m.sweeperCancel:
		// Another Stop() beat us to closing sweeperCancel. Fall
		// through to the wait on sweeperDone below — the sweeper
		// loop is already winding down (or already exited).
	default:
		close(m.sweeperCancel)
	}
	<-m.sweeperDone
}

// ============================================================================
// Task lifecycle
// ============================================================================

// Create registers a new task for (path, operation). If a running
// task for the same path already exists, returns that task with
// isNew=false. Otherwise creates a fresh Task with a new UUID and
// StatusQueued, returns isNew=true.
//
// Callers (the HTTP handler) inspect isNew to decide whether to 202
// or 409.
func (m *Manager) Create(path, operation string) (*Task, bool) {
	task, _, isNew := m.CreateHolding(operation, path)
	return task, isNew
}

// CreateHolding registers a new task for operation that holds every
// path in held; the first one becomes the task's Path. When a running
// task already holds any of them, nothing is created and nothing is
// held: it returns that task, the path it holds, and false. Otherwise
// it returns the new task (StatusQueued), "" and true.
//
// The check of every path and the taking of every lock happen under
// one acquisition of the manager's lock, so of two callers racing for
// a shared path exactly one gets it, and a caller never ends up holding
// some of its paths but not the others.
//
// A path given twice is held once. held must name at least one path.
func (m *Manager) CreateHolding(operation string, held ...string) (*Task, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held = uniquePaths(held)
	return m.createLocked(newTask{operation: operation, path: held[0], held: held})
}

// CreateSubtask is Create for a task that does part of another task's
// work: the build of one part's line index for a log chain's index
// task. It holds path like any task, and a running task that holds path
// is returned with false, as Create does.
//
// A subtask counts toward a cap of its own (DefaultMaxTasks finished
// subtasks), so a chain of thousands of parts, each built as a subtask,
// drops only older finished subtasks from the table, never a task a
// client asked for and may still be polling.
func (m *Manager) CreateSubtask(path, operation string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, _, isNew := m.createLocked(newTask{operation: operation, path: path, held: []string{path}, subtask: true})
	return task, isNew
}

// CreateKeyed registers a new task for operation that holds key in the
// path locks and shows path as its Path. When a running task already
// holds key it returns that task and false, as Create does.
//
// It is for a task whose work is not one file's: the index task of a log
// chain holds a key that starts with `chain:`, so it never collides with
// a task on the chain's active file, whose path equals the handle, while
// GET /v1/tasks/{id} still shows the handle.
func (m *Manager) CreateKeyed(operation, path, key string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, _, isNew := m.createLocked(newTask{operation: operation, path: path, held: []string{key}})
	return task, isNew
}

// CreateCancellable registers a new task for operation on path that
// Cancel can stop, and returns it with the context its worker runs
// under. When a task of the same operation already runs for path, it
// returns that task, its context and false: the caller joins it.
//
// The task holds a key of its operation and path (cancellableKey), not
// the path itself, so it runs beside a task of another operation on the
// same file: a signature scan reads the file and writes only its own
// cache, and an index build or a compression of the file does not stop
// it, nor it them.
//
// The context is canceled by Cancel, and also when the task ends by
// Complete or Fail, so a worker never holds a live context after its
// task is over. Only findings tasks are created this way.
func (m *Manager) CreateCancellable(path, operation string) (*Task, context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, _, isNew := m.createLocked(newTask{
		operation: operation, path: path, held: []string{cancellableKey(operation, path)}, cancellable: true,
	})
	return task, task.ctx, isNew
}

// CancellableHolder returns the unfinished cancellable task of
// operation for path, and false when none runs. It creates nothing: a
// caller that cannot start a task (no place for one) uses it to join
// the task already there.
func (m *Manager) CancellableHolder(path, operation string) (*Task, bool) {
	return m.Holder(cancellableKey(operation, path))
}

// cancellableKey is the key a cancellable task of operation holds for
// path in the path locks. The operation's name and a colon come first,
// so it never equals a path (an absolute path starts with "/") or a log
// chain's key (which starts with "chain:").
func cancellableKey(operation, path string) string {
	return operation + ":" + path
}

// ErrNotCancellable is returned by Cancel for a task that was not
// created cancellable: an index, compress or chain_index task.
var ErrNotCancellable = errors.New("the task cannot be cancelled")

// ErrTaskEnded is returned by Cancel for a task that has already ended,
// or whose work is past the point where it can stop (Settle).
var ErrTaskEnded = errors.New("the task has already ended")

// Settle marks the point past which the cancellable task taskID cannot
// stop: its worker has its answer and is about to write it. From then
// on Cancel refuses the task with ErrTaskEnded, so a task never ends
// cancelled after its work wrote anything. It returns false for a task
// that is already cancelled (the worker must then write nothing) and
// for an unknown task, and true otherwise, also when called again.
func (m *Manager) Settle(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || task.Status == StatusCancelled {
		return false
	}
	task.settled = true
	return true
}

// Cancel stops the cancellable task taskID: it cancels the task's
// context, which stops its worker's wait or work, and ends the task at
// once with StatusCancelled, which no later Update changes. Its key is
// released, so a new task for the same path may start at once (it waits
// for the stopping worker wherever the work itself serializes).
//
// It returns ErrTaskNotFound for an unknown task, ErrNotCancellable
// for a task not created by CreateCancellable, and ErrTaskEnded for a
// task that has already ended or settled (Settle); the task is not
// changed then.
func (m *Manager) Cancel(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	switch {
	case !ok:
		return ErrTaskNotFound
	case task.cancel == nil:
		return ErrNotCancellable
	case task.IsTerminal() || task.settled:
		return ErrTaskEnded
	}
	task.cancel()
	m.endLocked(task, StatusCancelled)
	return nil
}

// newTask is what creating a task takes: its operation, the path it
// shows, the keys it holds in the path locks (unique, at least one), its
// class, and whether Cancel can stop it.
type newTask struct {
	operation   string
	path        string
	held        []string
	subtask     bool
	cancellable bool
}

// createLocked creates the task spec describes, or returns the running
// task that holds one of its keys, that key and false. The caller holds
// m.mu.
func (m *Manager) createLocked(spec newTask) (*Task, string, bool) {
	for _, key := range spec.held {
		if running := m.runningHolderLocked(key); running != nil {
			return running, key, false
		}
	}

	task := &Task{
		TaskID:    uuid.NewString(),
		Path:      spec.path,
		Operation: spec.operation,
		Status:    StatusQueued,
		StartedAt: time.Now().UTC(),
		done:      make(chan struct{}),
		held:      spec.held,
		subtask:   spec.subtask,
		end:       &End{},
	}
	if spec.cancellable {
		// Go note: context.WithCancel returns a child of Background that
		// only cancel (or nothing else) ends. Background never ends by
		// itself, so the task's context lives until Cancel or the task's
		// end calls cancel.
		task.ctx, task.cancel = context.WithCancel(context.Background())
	}
	m.tasks[task.TaskID] = task
	for _, key := range spec.held {
		m.pathLocks[key] = task.TaskID
	}
	m.dropOldestFinishedLocked(spec.subtask)
	return task, "", true
}

// runningHolderLocked returns the unfinished task that holds path, or
// nil. A lock left by a task that has ended (or been dropped) is stale
// and is removed. The caller holds m.mu.
func (m *Manager) runningHolderLocked(path string) *Task {
	holderID, ok := m.pathLocks[path]
	if !ok {
		return nil
	}
	if holder, known := m.tasks[holderID]; known && !holder.IsTerminal() {
		return holder
	}
	delete(m.pathLocks, path)
	return nil
}

// releasePathsLocked drops every path lock task still holds. A path
// that another task has taken since is left to that task. The caller
// holds m.mu.
func (m *Manager) releasePathsLocked(task *Task) {
	for _, path := range task.held {
		if holder, ok := m.pathLocks[path]; ok && holder == task.TaskID {
			delete(m.pathLocks, path)
		}
	}
}

// uniquePaths returns paths without repeats, in their first order.
func uniquePaths(paths []string) []string {
	unique := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			unique = append(unique, path)
		}
	}
	return unique
}

// dropOldestFinishedLocked brings the finished tasks of one class
// (subtasks, or the tasks a client asked for) back to the cap, the task
// just created counted as one more to finish: it removes the oldest
// finished tasks of the class (oldest completion first) until, with the
// new task, there are at most m.maxTasks of them. So a class whose tasks
// end one after another never keeps more than m.maxTasks finished ones.
// The other class is not touched.
//
// INVARIANT: the other unfinished tasks are neither dropped nor counted.
// Their workers still report to them, and however many there are (a
// full samples build queue, many log chains waiting to be indexed), they
// never push a finished task out. The caller holds m.mu, and the new
// task is in m.tasks, unfinished.
func (m *Manager) dropOldestFinishedLocked(subtask bool) {
	finished := make([]*Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		if task.subtask == subtask && task.IsTerminal() && task.CompletedAt != nil {
			finished = append(finished, task)
		}
	}
	// The + 1 is the place the new task will take once it ends.
	n := len(finished) + 1 - m.maxTasks
	if n <= 0 {
		return
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].CompletedAt.Before(*finished[j].CompletedAt)
	})
	for _, task := range finished[:n] {
		delete(m.tasks, task.TaskID)
	}
}

// Update mutates a task's status / error / result. Releases the path
// lock when the task transitions to Terminal.
//
// Callers pass non-zero values for fields they want to change; zero
// values leave the field untouched. A helper variant Update*() could
// be added per-field if this becomes unwieldy.
//
// A cancelled task is not changed: Cancel ended it, and its worker's
// own Fail or Complete, which comes once the work has stopped, is
// ignored, so the task stays cancelled with no error and no result.
func (m *Manager) Update(taskID string, status Status, errMsg string, result any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return false
	}
	if task.Status == StatusCancelled {
		return true
	}
	// Whether the task had already ended before this update: only the
	// first transition to a terminal status closes done, since closing
	// a closed channel panics.
	wasTerminal := task.IsTerminal()
	if status != "" {
		task.Status = status
	}
	if errMsg != "" {
		task.Error = errMsg
	}
	if result != nil {
		task.Result = result
	}
	if task.IsTerminal() && !wasTerminal {
		m.closeEndLocked(task)
	}
	if task.IsTerminal() {
		now := time.Now().UTC()
		task.CompletedAt = &now
		m.releasePathsLocked(task)
		if task.cancel != nil {
			// The work is over: release the context's resources.
			task.cancel()
		}
	}
	return true
}

// endLocked ends task with status: the end time, the end every watcher
// reads, the done channel closed and the keys released. The caller
// holds m.mu, and task has not ended yet.
func (m *Manager) endLocked(task *Task, status Status) {
	task.Status = status
	now := time.Now().UTC()
	task.CompletedAt = &now
	m.closeEndLocked(task)
	m.releasePathsLocked(task)
}

// closeEndLocked writes how task ended and closes its done channel. The
// end is written before done closes, so every watcher woken by the
// close reads it complete (see Watch.End). The caller holds m.mu, and
// task has just reached a terminal status for the first time.
func (m *Manager) closeEndLocked(task *Task) {
	*task.end = End{Status: task.Status, Error: task.Error}
	close(task.done)
}

// MarkRunning is a convenience for the worker goroutine that picks up
// a queued task and begins execution.
func (m *Manager) MarkRunning(taskID string) bool {
	return m.Update(taskID, StatusRunning, "", nil)
}

// Complete marks a task successful with the given result.
func (m *Manager) Complete(taskID string, result any) bool {
	return m.Update(taskID, StatusCompleted, "", result)
}

// Fail marks a task as failed with the given error message.
func (m *Manager) Fail(taskID string, errMsg string) bool {
	return m.Update(taskID, StatusFailed, errMsg, nil)
}

// Done returns a channel that is closed when the task ends, completed,
// failed or cancelled, and false when no such task exists. A receive from it
// blocks until then; a task that has already ended returns a channel
// that is already closed.
//
// This is how a caller waits for a task it did not start: an HTTP
// request that needs the result of a running build selects on this
// channel and on its own deadline, and whichever comes first decides
// its answer. The task keeps running either way.
func (m *Manager) Done(taskID string) (<-chan struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return nil, false
	}
	return task.done, true
}

// Watch returns a watch of the task taskID, and false when no such task
// is in the table. The watch says when the task ends and how, and keeps
// saying it after the task has left the table.
//
// A caller that must learn how a task ended takes the watch while the
// task is known to be in the table (it has just been created, or it is
// running: neither the cap nor the sweeper drops an unfinished task),
// and reads the end once the watch's done channel is closed.
func (m *Manager) Watch(taskID string) (Watch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return Watch{}, false
	}
	return Watch{done: task.done, end: task.end}, true
}

// TTL returns how long a finished task stays in the table before the
// sweeper removes it (RX_TASK_TTL_MINUTES, or Config.TTL). A caller that
// keeps a task's outcome beside the table keeps it as long.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Holder returns the unfinished task that holds path, and false when
// none does. It creates nothing: a caller that must not start a task
// uses it to join the one already there.
func (m *Manager) Holder(path string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	holder := m.runningHolderLocked(path)
	if holder == nil {
		return nil, false
	}
	clone := *holder
	return &clone, true
}

// ReportProgress gives a task the function its status reads progress
// from. The worker calls it once, when it starts; every status request
// after that calls fn. Returns false when no such task exists.
func (m *Manager) ReportProgress(taskID string, fn ProgressFunc) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return false
	}
	task.progress = fn
	return true
}

// ReportBytes gives a task the function its status reads the bytes read
// and their total from, as ReportProgress gives the fraction. The worker
// calls it once, when it starts. Returns false when no such task exists.
func (m *Manager) ReportBytes(taskID string, fn ByteProgressFunc) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return false
	}
	task.bytes = fn
	return true
}

// Get returns a task by ID. Returns (nil, false) if not found.
// The returned *Task is a SHALLOW COPY so the caller can't mutate
// our internal state from outside the lock.
func (m *Manager) Get(taskID string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return nil, false
	}
	clone := *task
	return &clone, true
}

// List returns all tasks as a slice of copies. Ordered by StartedAt
// (oldest first); callers sort differently if needed. Useful for
// /v1/tasks debugging endpoint.
func (m *Manager) List() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		clone := *t
		out = append(out, &clone)
	}
	return out
}

// ============================================================================
// Sweeper
// ============================================================================

// sweeperLoop runs until Stop is called. Each tick, it scans the task
// store for terminal tasks older than TTL and removes them, along with
// any stale path lock still pointing at them.
//
// This is the Go analog of rx-python/src/rx/task_manager.py::cleanup_old_tasks,
// except that module requires manual invocation; rx-go schedules it
// automatically.
func (m *Manager) sweeperLoop() {
	defer close(m.sweeperDone)
	tick := time.NewTicker(m.sweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-m.sweeperCancel:
			return
		case <-tick.C:
			n := m.sweep(time.Now().UTC())
			if n > 0 {
				m.logger.Info("tasks_swept", "removed", n)
			}
		}
	}
}

// sweep removes terminal tasks older than TTL. Returns the number of
// tasks removed. Exported via RunSweepForTests to let tests force a
// pass without waiting for the ticker.
func (m *Manager) sweep(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed int
	for id, task := range m.tasks {
		if !task.IsTerminal() || task.CompletedAt == nil {
			continue
		}
		if now.Sub(*task.CompletedAt) > m.ttl {
			delete(m.tasks, id)
			// Drop any lock that still points at the task.
			m.releasePathsLocked(task)
			removed++
		}
	}
	return removed
}

// RunSweepForTests forces a sweeper pass. Real code shouldn't call
// this — the ticker handles it.
func (m *Manager) RunSweepForTests(now time.Time) int {
	return m.sweep(now)
}

// ============================================================================
// Introspection helpers
// ============================================================================

// ActivePathLockCount returns how many paths are currently locked.
// Used by tests and /v1/health.
func (m *Manager) ActivePathLockCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pathLocks)
}

// Size returns the total task count (all statuses).
func (m *Manager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tasks)
}

// ============================================================================
// ErrTaskNotFound is a sentinel for the HTTP handler's 404 path.
// ============================================================================

// ErrTaskNotFound indicates the requested task_id doesn't exist in the
// store (either never created, or expired and swept).
var ErrTaskNotFound = fmt.Errorf("task not found")

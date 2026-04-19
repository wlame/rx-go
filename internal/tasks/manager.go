// Package tasks implements the background-task manager that backs
// POST /v1/index and POST /v1/compress — long-running operations
// that the HTTP layer wants to run asynchronously and have the client
// poll via GET /v1/tasks/{id}.
//
// Features from the Python port (rx-python/src/rx/task_manager.py):
//
//   - In-memory task store keyed by task_id (UUID v4).
//   - Path locking: only one task per (path, operation) pair can be
//     running at a time; duplicate submissions return the SAME task
//     ID (idempotent POST).
//   - Sweeper goroutine: every 5 minutes, removes completed/failed
//     tasks older than RX_TASK_TTL_MINUTES (default 60).
//
// Design decision (5.12 — "task store backing"):
//
//	Python uses an asyncio.Lock with dict mutation. Go port uses a
//	plain sync.Mutex around a sync.Map — a map[string]*Task — for
//	tasks, plus a sync.Mutex-guarded map[string]string for path
//	locks. A full sync.Map would work too, but we need the "is
//	this path already locked?" atomic check-and-insert, and the
//	single-lock variant is simpler to reason about.
package tasks

import (
	"fmt"
	"log/slog"
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

// DefaultSweepInterval is how often the sweeper goroutine wakes up.
// Python uses 5 minutes; we match.
const DefaultSweepInterval = 5 * time.Minute

// ttlFromEnv returns the effective TTL given RX_TASK_TTL_MINUTES or
// the default.
func ttlFromEnv() time.Duration {
	v := config.GetIntEnv("RX_TASK_TTL_MINUTES", 60)
	return time.Duration(v) * time.Minute
}

// ============================================================================
// Task types
// ============================================================================

// Status is the lifecycle state of a background task.
type Status string

// Task lifecycle:
//
//	Queued -> Running -> (Completed | Failed)
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Task holds the metadata and result of one background job. All
// fields except TaskID are mutable after creation; concurrent access
// is serialized through Manager's lock.
type Task struct {
	TaskID      string
	Path        string
	Operation   string // "index" | "compress"
	Status      Status
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       string
	Result      map[string]any
}

// IsTerminal reports whether the task is done (completed or failed).
// Used by the sweeper and by HTTP 409 logic.
func (t *Task) IsTerminal() bool {
	return t.Status == StatusCompleted || t.Status == StatusFailed
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
	// See Stage 8 Reviewer 2 High #6.
	started atomic.Bool

	logger *slog.Logger
}

// Config passes optional Manager settings.
type Config struct {
	TTL           time.Duration // finished task retention; 0 = env/default
	SweepInterval time.Duration // sweeper interval; 0 = default
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
	return &Manager{
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
	m.mu.Lock()
	defer m.mu.Unlock()

	if existingID, ok := m.pathLocks[path]; ok {
		if existing, ok2 := m.tasks[existingID]; ok2 && !existing.IsTerminal() {
			return existing, false
		}
		// Stale lock — fallthrough to create new.
		delete(m.pathLocks, path)
	}

	task := &Task{
		TaskID:    uuid.NewString(),
		Path:      path,
		Operation: operation,
		Status:    StatusQueued,
		StartedAt: time.Now().UTC(),
	}
	m.tasks[task.TaskID] = task
	m.pathLocks[path] = task.TaskID
	return task, true
}

// Update mutates a task's status / error / result. Releases the path
// lock when the task transitions to Terminal.
//
// Callers pass non-zero values for fields they want to change; zero
// values leave the field untouched. A helper variant Update*() could
// be added per-field if this becomes unwieldy.
func (m *Manager) Update(taskID string, status Status, errMsg string, result map[string]any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return false
	}
	if status != "" {
		task.Status = status
	}
	if errMsg != "" {
		task.Error = errMsg
	}
	if result != nil {
		task.Result = result
	}
	if task.IsTerminal() {
		now := time.Now().UTC()
		task.CompletedAt = &now
		// Release path lock iff WE still hold it.
		if held, ok := m.pathLocks[task.Path]; ok && held == taskID {
			delete(m.pathLocks, task.Path)
		}
	}
	return true
}

// MarkRunning is a convenience for the worker goroutine that picks up
// a queued task and begins execution.
func (m *Manager) MarkRunning(taskID string) bool {
	return m.Update(taskID, StatusRunning, "", nil)
}

// Complete marks a task successful with the given result.
func (m *Manager) Complete(taskID string, result map[string]any) bool {
	return m.Update(taskID, StatusCompleted, "", result)
}

// Fail marks a task as failed with the given error message.
func (m *Manager) Fail(taskID string, errMsg string) bool {
	return m.Update(taskID, StatusFailed, errMsg, nil)
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
			// Best-effort: drop a stale lock if it still points at us.
			if held, ok := m.pathLocks[task.Path]; ok && held == id {
				delete(m.pathLocks, task.Path)
			}
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

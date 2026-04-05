package api

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// TaskState represents the lifecycle state of a background task.
type TaskState string

const (
	TaskStatePending   TaskState = "pending"
	TaskStateRunning   TaskState = "running"
	TaskStateCompleted TaskState = "completed"
	TaskStateFailed    TaskState = "failed"
)

// Task tracks a background operation (index, compress) with its lifecycle state.
type Task struct {
	ID          string
	Operation   string // "index" or "compress"
	Path        string
	State       TaskState
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       *string
	Result      map[string]interface{}
}

// TaskStore is a thread-safe in-memory store for background task tracking.
// It uses sync.RWMutex for concurrent read access and exclusive writes.
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

// NewTaskStore creates an empty task store.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[string]*Task),
	}
}

// Create registers a new task in pending state and returns its ID.
func (ts *TaskStore) Create(operation, path string) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	id := uuid.New().String()
	ts.tasks[id] = &Task{
		ID:        id,
		Operation: operation,
		Path:      path,
		State:     TaskStatePending,
		StartedAt: time.Now(),
	}
	return id
}

// Get returns the task with the given ID, or nil if not found.
func (ts *TaskStore) Get(id string) *Task {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	t, ok := ts.tasks[id]
	if !ok {
		return nil
	}
	// Return a copy to avoid data races on the caller's side.
	cpy := *t
	return &cpy
}

// SetRunning transitions a task to the running state.
func (ts *TaskStore) SetRunning(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if t, ok := ts.tasks[id]; ok {
		t.State = TaskStateRunning
	}
}

// Complete transitions a task to the completed state with its result payload.
func (ts *TaskStore) Complete(id string, result map[string]interface{}) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if t, ok := ts.tasks[id]; ok {
		t.State = TaskStateCompleted
		now := time.Now()
		t.CompletedAt = &now
		t.Result = result
	}
}

// Fail transitions a task to the failed state with an error message.
func (ts *TaskStore) Fail(id string, errMsg string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if t, ok := ts.tasks[id]; ok {
		t.State = TaskStateFailed
		now := time.Now()
		t.CompletedAt = &now
		t.Error = &errMsg
	}
}

// Package requeststore maintains an in-memory record of recent trace
// requests — the "request history" feature used by the rx-viewer
// frontend to show a recent-activity list.
//
// Parity: rx-python/src/rx/request_store.py (threading.Lock over a
// plain dict with manual capacity management).
//
// Capacity policy: FIFO eviction at MAX_STORED_REQUESTS (default 100k).
// When the store is full and a new record arrives, the OLDEST COMPLETED
// entry is evicted. In-progress entries are never evicted — that would
// break the `/v1/trace/history` view for concurrent requests.
//
// Thread-safety: a single sync.Mutex protects the map. A fancier
// concurrent map would let us do lock-free reads, but List() already
// has to snapshot a slice of the entries (sort by started_at), so
// the simpler lock is fine.
package requeststore

import (
	"sort"
	"sync"
	"time"
)

// ============================================================================
// Tunables
// ============================================================================

// DefaultMaxEntries is how many records the store holds before FIFO
// eviction kicks in. 100k matches Python's MAX_STORED_REQUESTS — at
// ~1 KB per entry, this is ~100 MB resident memory worst-case, which
// is acceptable for long-lived rx serve processes.
const DefaultMaxEntries = 100_000

// ============================================================================
// RequestInfo
// ============================================================================

// RequestInfo is the stored shape. Mirrors the subset of
// rx-python/src/rx/models.py::RequestInfo that the HTTP layer writes
// and reads. Types tweaked for Go idiom — pointer for optional
// CompletedAt, int64 for counters so `sync/atomic` can be used by the
// dispatcher if it wants to avoid the map mutex on counter bumps.
type RequestInfo struct {
	RequestID         string     `json:"request_id"`
	Paths             []string   `json:"paths"`
	Patterns          []string   `json:"patterns"`
	MaxResults        *int       `json:"max_results,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	TotalMatches      int64      `json:"total_matches"`
	TotalFilesScanned int64      `json:"total_files_scanned"`
	TotalFilesSkipped int64      `json:"total_files_skipped"`
	TotalTimeMS       int64      `json:"total_time_ms"`

	// Hook counters — incremented by the dispatcher on each webhook
	// result. Stored per-request so the frontend can show the hook
	// success rate alongside the trace result.
	HookOnFileSuccess     int64 `json:"hook_on_file_success"`
	HookOnFileFailed      int64 `json:"hook_on_file_failed"`
	HookOnMatchSuccess    int64 `json:"hook_on_match_success"`
	HookOnMatchFailed     int64 `json:"hook_on_match_failed"`
	HookOnCompleteSuccess int64 `json:"hook_on_complete_success"`
	HookOnCompleteFailed  int64 `json:"hook_on_complete_failed"`
}

// IsComplete reports whether the request has been marked finished.
func (r *RequestInfo) IsComplete() bool { return r.CompletedAt != nil }

// ============================================================================
// Store
// ============================================================================

// Store holds the requests. Construct via New. Safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	entries    map[string]*RequestInfo
	maxEntries int
}

// Config passes optional knobs to New.
type Config struct {
	MaxEntries int // <=0 means use default
}

// New constructs an empty Store.
func New(cfg Config) *Store {
	max := cfg.MaxEntries
	if max <= 0 {
		max = DefaultMaxEntries
	}
	return &Store{
		entries:    make(map[string]*RequestInfo, 1024),
		maxEntries: max,
	}
}

// ============================================================================
// Mutators
// ============================================================================

// Add inserts a request. If the store is at capacity, the oldest
// COMPLETED request is evicted first; if none are completed, the
// insertion is kept (capacity can overshoot by at most in-flight
// request count).
//
// Add is idempotent — re-adding a request with the same ID overwrites
// the existing entry.
func (s *Store) Add(r *RequestInfo) {
	if r == nil || r.RequestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.entries[r.RequestID]; !exists && len(s.entries) >= s.maxEntries {
		s.evictOldestCompletedLocked()
	}
	s.entries[r.RequestID] = r
}

// Get returns the stored request by ID, or nil if not found.
// Returns a pointer to the STORED entry — callers that want to mutate
// it should go through Update / IncrementHook to take the lock.
func (s *Store) Get(requestID string) *RequestInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.entries[requestID]; ok {
		clone := *r
		return &clone
	}
	return nil
}

// Update applies a callback under the lock. The callback receives the
// stored *RequestInfo and may mutate it in place. Returns true if the
// request existed and was updated, false otherwise.
//
// Example:
//
//	store.Update(id, func(r *RequestInfo) {
//	    now := time.Now()
//	    r.CompletedAt = &now
//	    r.TotalMatches = int64(total)
//	})
func (s *Store) Update(requestID string, fn func(*RequestInfo)) bool {
	if fn == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.entries[requestID]
	if !ok {
		return false
	}
	fn(r)
	return true
}

// IncrementHook bumps the relevant success/failure counter for the
// given event kind ("on_file", "on_match", "on_complete"). No-op if
// the request isn't in the store (e.g. evicted during a long scan).
func (s *Store) IncrementHook(requestID, kind string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.entries[requestID]
	if !ok {
		return
	}
	switch kind {
	case "on_file":
		if success {
			r.HookOnFileSuccess++
		} else {
			r.HookOnFileFailed++
		}
	case "on_match":
		if success {
			r.HookOnMatchSuccess++
		} else {
			r.HookOnMatchFailed++
		}
	case "on_complete":
		if success {
			r.HookOnCompleteSuccess++
		} else {
			r.HookOnCompleteFailed++
		}
	}
}

// ============================================================================
// Queries
// ============================================================================

// List returns a snapshot of up to `limit` requests, sorted by
// StartedAt descending (most recent first). If includeCompleted is
// false, in-progress requests only.
//
// limit <= 0 returns all matching entries.
func (s *Store) List(limit int, includeCompleted bool) []*RequestInfo {
	s.mu.Lock()
	entries := make([]*RequestInfo, 0, len(s.entries))
	for _, r := range s.entries {
		if !includeCompleted && r.IsComplete() {
			continue
		}
		clone := *r
		entries = append(entries, &clone)
	}
	s.mu.Unlock()

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].StartedAt.After(entries[j].StartedAt)
	})

	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries
}

// Size returns the current entry count.
func (s *Store) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Stats returns counters suitable for /v1/health exposure.
func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var completed, inProgress int
	for _, r := range s.entries {
		if r.IsComplete() {
			completed++
		} else {
			inProgress++
		}
	}
	return StoreStats{
		TotalRequests:      len(s.entries),
		CompletedRequests:  completed,
		InProgressRequests: inProgress,
		MaxCapacity:        s.maxEntries,
	}
}

// StoreStats is the Stats() return type — mirrors Python's
// get_store_stats output keys.
type StoreStats struct {
	TotalRequests      int `json:"total_requests"`
	CompletedRequests  int `json:"completed_requests"`
	InProgressRequests int `json:"in_progress_requests"`
	MaxCapacity        int `json:"max_capacity"`
}

// ClearOld removes completed requests older than maxAge. Returns the
// number of requests removed. Distinct from the tasks sweeper — this
// is called manually by operators (or by a future hook) and is not
// scheduled automatically; the capacity-based eviction covers the
// common case.
func (s *Store) ClearOld(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed int
	for id, r := range s.entries {
		if r.CompletedAt != nil && r.CompletedAt.Before(cutoff) {
			delete(s.entries, id)
			removed++
		}
	}
	return removed
}

// ============================================================================
// Eviction helper
// ============================================================================

// evictOldestCompletedLocked removes up to 10% of COMPLETED entries
// (or at least 100, matching Python's min-cap) by oldest CompletedAt.
// Caller must hold s.mu.
func (s *Store) evictOldestCompletedLocked() {
	type kv struct {
		id string
		c  time.Time
	}
	var completed []kv
	for id, r := range s.entries {
		if r.IsComplete() {
			completed = append(completed, kv{id, *r.CompletedAt})
		}
	}
	if len(completed) == 0 {
		// Nothing to evict — in-progress entries only. Allow overshoot.
		return
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].c.Before(completed[j].c)
	})
	toRemove := len(completed) / 10
	if toRemove < 100 {
		toRemove = 100
	}
	if toRemove > len(completed) {
		toRemove = len(completed)
	}
	for i := 0; i < toRemove; i++ {
		delete(s.entries, completed[i].id)
	}
}

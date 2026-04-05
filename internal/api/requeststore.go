package api

import (
	"sort"
	"sync"
	"time"
)

// MaxStoredRequests is the maximum number of requests kept in the in-memory store.
// When the store reaches capacity, the oldest entries are evicted.
const MaxStoredRequests = 100_000

// RequestInfo tracks metadata about a single trace request for observability.
type RequestInfo struct {
	RequestID    string    `json:"request_id"`
	Paths        []string  `json:"paths"`
	Patterns     []string  `json:"patterns"`
	StartTime    time.Time `json:"start_time"`
	Duration     float64   `json:"duration_seconds"` // 0 until the request completes
	TotalMatches int       `json:"total_matches"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// RequestStore is a thread-safe in-memory store for tracking trace requests.
// It enforces a capacity limit by evicting the oldest entries when full.
type RequestStore struct {
	mu       sync.RWMutex
	requests map[string]*RequestInfo
	// order tracks insertion order so we can evict the oldest entries.
	order []string
}

// NewRequestStore creates an empty request store.
func NewRequestStore() *RequestStore {
	return &RequestStore{
		requests: make(map[string]*RequestInfo, 256),
		order:    make([]string, 0, 256),
	}
}

// Store adds or replaces a request entry. If the store is at capacity, the oldest
// entries are evicted first (oldest 10%, minimum 100).
func (rs *RequestStore) Store(info RequestInfo) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if len(rs.requests) >= MaxStoredRequests {
		rs.evictOldest()
	}

	// If this ID already exists, just update in place without re-adding to order slice.
	if _, exists := rs.requests[info.RequestID]; exists {
		cp := info
		rs.requests[info.RequestID] = &cp
		return
	}

	cp := info
	rs.requests[info.RequestID] = &cp
	rs.order = append(rs.order, info.RequestID)
}

// Get returns a copy of the RequestInfo for the given ID, and true if found.
func (rs *RequestStore) Get(id string) (RequestInfo, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	info, ok := rs.requests[id]
	if !ok {
		return RequestInfo{}, false
	}
	return *info, true
}

// List returns the most recent requests (sorted by start_time descending), up to limit.
func (rs *RequestStore) List(limit int) []RequestInfo {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	// Collect all entries.
	all := make([]RequestInfo, 0, len(rs.requests))
	for _, info := range rs.requests {
		all = append(all, *info)
	}

	// Sort by start time descending (most recent first).
	sort.Slice(all, func(i, j int) bool {
		return all[i].StartTime.After(all[j].StartTime)
	})

	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}

	return all
}

// Len returns the current number of stored requests.
func (rs *RequestStore) Len() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.requests)
}

// Stats returns store statistics as a map for the health endpoint.
func (rs *RequestStore) Stats() map[string]interface{} {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	completed := 0
	for _, info := range rs.requests {
		if info.CompletedAt != nil {
			completed++
		}
	}

	return map[string]interface{}{
		"total_requests":       len(rs.requests),
		"completed_requests":   completed,
		"in_progress_requests": len(rs.requests) - completed,
		"max_capacity":         MaxStoredRequests,
	}
}

// evictOldest removes the oldest 10% of entries (minimum 100).
// Must be called with rs.mu held for writing.
func (rs *RequestStore) evictOldest() {
	toRemove := len(rs.requests) / 10
	if toRemove < 100 {
		toRemove = 100
	}
	if toRemove > len(rs.order) {
		toRemove = len(rs.order)
	}

	// Remove the oldest entries (front of the order slice).
	evicted := rs.order[:toRemove]
	rs.order = rs.order[toRemove:]

	for _, id := range evicted {
		delete(rs.requests, id)
	}
}

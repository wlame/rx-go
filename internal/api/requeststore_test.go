package api

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestStore_StoreAndGet(t *testing.T) {
	rs := NewRequestStore()

	now := time.Now()
	info := RequestInfo{
		RequestID:    "req-001",
		Paths:        []string{"/tmp/test.log"},
		Patterns:     []string{"ERROR"},
		StartTime:    now,
		TotalMatches: 42,
	}

	rs.Store(info)

	got, ok := rs.Get("req-001")
	require.True(t, ok)
	assert.Equal(t, "req-001", got.RequestID)
	assert.Equal(t, []string{"/tmp/test.log"}, got.Paths)
	assert.Equal(t, []string{"ERROR"}, got.Patterns)
	assert.Equal(t, 42, got.TotalMatches)
}

func TestRequestStore_Get_NotFound(t *testing.T) {
	rs := NewRequestStore()

	_, ok := rs.Get("nonexistent")
	assert.False(t, ok)
}

func TestRequestStore_Store_UpdateExisting(t *testing.T) {
	rs := NewRequestStore()

	rs.Store(RequestInfo{
		RequestID:    "req-001",
		TotalMatches: 0,
		StartTime:    time.Now(),
	})

	// Update with completion info.
	now := time.Now()
	rs.Store(RequestInfo{
		RequestID:    "req-001",
		TotalMatches: 50,
		Duration:     1.5,
		CompletedAt:  &now,
		StartTime:    time.Now(),
	})

	got, ok := rs.Get("req-001")
	require.True(t, ok)
	assert.Equal(t, 50, got.TotalMatches)
	assert.Equal(t, 1.5, got.Duration)
	assert.NotNil(t, got.CompletedAt)

	// Count should still be 1 (update, not duplicate).
	assert.Equal(t, 1, rs.Len())
}

func TestRequestStore_List_SortedByStartTime(t *testing.T) {
	rs := NewRequestStore()

	base := time.Now()
	for i := 0; i < 5; i++ {
		rs.Store(RequestInfo{
			RequestID: fmt.Sprintf("req-%03d", i),
			StartTime: base.Add(time.Duration(i) * time.Second),
		})
	}

	// List all — should be sorted most recent first.
	all := rs.List(0)
	require.Len(t, all, 5)
	assert.Equal(t, "req-004", all[0].RequestID)
	assert.Equal(t, "req-000", all[4].RequestID)
}

func TestRequestStore_List_WithLimit(t *testing.T) {
	rs := NewRequestStore()

	for i := 0; i < 10; i++ {
		rs.Store(RequestInfo{
			RequestID: fmt.Sprintf("req-%03d", i),
			StartTime: time.Now().Add(time.Duration(i) * time.Millisecond),
		})
	}

	limited := rs.List(3)
	assert.Len(t, limited, 3)
}

func TestRequestStore_CapacityLimit_EvictsOldest(t *testing.T) {
	rs := NewRequestStore()

	// Fill to capacity.
	for i := 0; i < MaxStoredRequests; i++ {
		rs.Store(RequestInfo{
			RequestID: fmt.Sprintf("req-%06d", i),
			StartTime: time.Now(),
		})
	}
	assert.Equal(t, MaxStoredRequests, rs.Len())

	// Add one more — should trigger eviction.
	rs.Store(RequestInfo{
		RequestID: "req-overflow",
		StartTime: time.Now(),
	})

	// Store should now be under capacity.
	assert.Less(t, rs.Len(), MaxStoredRequests)

	// The new entry should be present.
	_, ok := rs.Get("req-overflow")
	assert.True(t, ok)

	// The very first entry should have been evicted (it's the oldest).
	_, ok = rs.Get("req-000000")
	assert.False(t, ok, "oldest entry should have been evicted")
}

func TestRequestStore_ConcurrentAccess(t *testing.T) {
	rs := NewRequestStore()

	var wg sync.WaitGroup
	concurrency := 100

	// Writers: each goroutine stores a unique request.
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rs.Store(RequestInfo{
				RequestID: fmt.Sprintf("concurrent-%04d", id),
				StartTime: time.Now(),
			})
		}(i)
	}

	// Readers: each goroutine reads a random request.
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rs.Get(fmt.Sprintf("concurrent-%04d", id))
		}(i)
	}

	// List calls.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs.List(10)
		}()
	}

	wg.Wait()

	// All writers should have succeeded.
	assert.Equal(t, concurrency, rs.Len())
}

func TestRequestStore_Stats(t *testing.T) {
	rs := NewRequestStore()

	now := time.Now()
	rs.Store(RequestInfo{RequestID: "a", StartTime: now})
	rs.Store(RequestInfo{RequestID: "b", StartTime: now, CompletedAt: &now, Duration: 0.5})
	rs.Store(RequestInfo{RequestID: "c", StartTime: now, CompletedAt: &now, Duration: 1.0})

	stats := rs.Stats()
	assert.Equal(t, 3, stats["total_requests"])
	assert.Equal(t, 2, stats["completed_requests"])
	assert.Equal(t, 1, stats["in_progress_requests"])
	assert.Equal(t, MaxStoredRequests, stats["max_capacity"])
}

func TestRequestStore_Len(t *testing.T) {
	rs := NewRequestStore()
	assert.Equal(t, 0, rs.Len())

	rs.Store(RequestInfo{RequestID: "a", StartTime: time.Now()})
	assert.Equal(t, 1, rs.Len())

	rs.Store(RequestInfo{RequestID: "b", StartTime: time.Now()})
	assert.Equal(t, 2, rs.Len())
}

func TestRequestStore_GetReturnsCopy(t *testing.T) {
	rs := NewRequestStore()

	rs.Store(RequestInfo{
		RequestID:    "req-copy",
		TotalMatches: 10,
		StartTime:    time.Now(),
	})

	// Get a copy and modify it.
	got, ok := rs.Get("req-copy")
	require.True(t, ok)
	got.TotalMatches = 999

	// Original should be unchanged.
	original, ok := rs.Get("req-copy")
	require.True(t, ok)
	assert.Equal(t, 10, original.TotalMatches)
}

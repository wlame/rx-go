package requeststore

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func mkReq(id string, completed bool) *RequestInfo {
	r := &RequestInfo{
		RequestID: id,
		Paths:     []string{"/x"},
		Patterns:  []string{"p"},
		StartedAt: time.Now(),
	}
	if completed {
		now := time.Now()
		r.CompletedAt = &now
	}
	return r
}

func TestStore_Add_Get(t *testing.T) {
	s := New(Config{})
	r := mkReq("a", false)
	s.Add(r)
	got := s.Get("a")
	if got == nil {
		t.Fatal("Get returned nil after Add")
	}
	if got.RequestID != "a" {
		t.Errorf("request_id = %q", got.RequestID)
	}
	if got == r {
		t.Error("Get returned the stored pointer; want a clone")
	}
}

func TestStore_Get_Missing(t *testing.T) {
	s := New(Config{})
	if s.Get("missing") != nil {
		t.Error("Get returned non-nil for unknown id")
	}
}

func TestStore_Add_NilOrEmptyIDIsNoop(t *testing.T) {
	s := New(Config{})
	s.Add(nil)
	s.Add(&RequestInfo{}) // empty ID
	if s.Size() != 0 {
		t.Errorf("Size = %d, want 0 after no-op Add calls", s.Size())
	}
}

func TestStore_Update_AtomicMutation(t *testing.T) {
	s := New(Config{})
	s.Add(mkReq("a", false))
	ok := s.Update("a", func(r *RequestInfo) {
		r.TotalMatches = 100
	})
	if !ok {
		t.Fatal("Update returned false on existing id")
	}
	got := s.Get("a")
	if got.TotalMatches != 100 {
		t.Errorf("TotalMatches = %d, want 100", got.TotalMatches)
	}
}

func TestStore_Update_Missing(t *testing.T) {
	s := New(Config{})
	if s.Update("ghost", func(*RequestInfo) {}) {
		t.Error("Update returned true for missing id")
	}
	if s.Update("any", nil) {
		t.Error("Update returned true for nil callback")
	}
}

func TestStore_IncrementHook_CountersBump(t *testing.T) {
	s := New(Config{})
	s.Add(mkReq("r", false))
	s.IncrementHook("r", "on_file", true)
	s.IncrementHook("r", "on_file", false)
	s.IncrementHook("r", "on_match", true)
	s.IncrementHook("r", "on_complete", false)

	got := s.Get("r")
	if got.HookOnFileSuccess != 1 || got.HookOnFileFailed != 1 {
		t.Errorf("on_file counters: %+v", got)
	}
	if got.HookOnMatchSuccess != 1 {
		t.Errorf("on_match success = %d", got.HookOnMatchSuccess)
	}
	if got.HookOnCompleteFailed != 1 {
		t.Errorf("on_complete failed = %d", got.HookOnCompleteFailed)
	}
}

func TestStore_IncrementHook_MissingIsNoop(t *testing.T) {
	s := New(Config{})
	// Must not panic on ghost id.
	s.IncrementHook("ghost", "on_file", true)
}

func TestStore_List_SortedDescByStartedAt(t *testing.T) {
	s := New(Config{})
	base := time.Now()
	for i := 0; i < 3; i++ {
		r := &RequestInfo{
			RequestID: fmt.Sprintf("req-%d", i),
			StartedAt: base.Add(time.Duration(i) * time.Second),
		}
		s.Add(r)
	}
	got := s.List(0, true)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].RequestID != "req-2" {
		t.Errorf("first = %q, want req-2 (most recent)", got[0].RequestID)
	}
}

func TestStore_List_Limit(t *testing.T) {
	s := New(Config{})
	for i := 0; i < 10; i++ {
		s.Add(&RequestInfo{
			RequestID: fmt.Sprintf("id-%d", i),
			StartedAt: time.Now().Add(time.Duration(i) * time.Second),
		})
	}
	got := s.List(3, true)
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func TestStore_List_ExcludeCompleted(t *testing.T) {
	s := New(Config{})
	s.Add(mkReq("done", true))
	s.Add(mkReq("active", false))
	got := s.List(0, false)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].RequestID != "active" {
		t.Errorf("got %q, want active", got[0].RequestID)
	}
}

func TestStore_Stats(t *testing.T) {
	s := New(Config{MaxEntries: 10})
	s.Add(mkReq("a", true))
	s.Add(mkReq("b", false))
	st := s.Stats()
	if st.TotalRequests != 2 || st.CompletedRequests != 1 || st.InProgressRequests != 1 {
		t.Errorf("stats = %+v", st)
	}
	if st.MaxCapacity != 10 {
		t.Errorf("MaxCapacity = %d, want 10", st.MaxCapacity)
	}
}

// TestStore_ClearOld removes completed entries older than cutoff.
func TestStore_ClearOld(t *testing.T) {
	s := New(Config{})
	// A: completed 2 hours ago
	past := time.Now().Add(-2 * time.Hour)
	s.Add(&RequestInfo{
		RequestID: "old", StartedAt: past, CompletedAt: &past,
	})
	// B: completed now
	now := time.Now()
	s.Add(&RequestInfo{
		RequestID: "fresh", StartedAt: now, CompletedAt: &now,
	})
	// C: in-progress (never removed by ClearOld regardless of age).
	s.Add(&RequestInfo{RequestID: "active", StartedAt: past})

	n := s.ClearOld(1 * time.Hour)
	if n != 1 {
		t.Errorf("ClearOld removed %d, want 1", n)
	}
	if s.Get("old") != nil {
		t.Error("old should have been cleared")
	}
	if s.Get("fresh") == nil {
		t.Error("fresh should survive")
	}
	if s.Get("active") == nil {
		t.Error("in-progress should always survive ClearOld")
	}
}

// TestStore_ClearOld_NoOpOnZero — sanity check negative/zero maxAge.
func TestStore_ClearOld_NoOpOnZero(t *testing.T) {
	s := New(Config{})
	s.Add(mkReq("x", true))
	if got := s.ClearOld(0); got != 0 {
		t.Errorf("ClearOld(0) removed %d, want 0", got)
	}
}

// TestStore_FIFO_EvictionOnCapacity forces capacity and verifies
// the oldest COMPLETED entry gets evicted first.
func TestStore_FIFO_EvictionOnCapacity(t *testing.T) {
	s := New(Config{MaxEntries: 100})
	// Fill beyond capacity with completed entries so eviction kicks in.
	for i := 0; i < 200; i++ {
		// Ensure CompletedAt is staggered so "oldest" is deterministic.
		done := time.Now().Add(time.Duration(i) * time.Millisecond)
		s.Add(&RequestInfo{
			RequestID:   fmt.Sprintf("r-%03d", i),
			StartedAt:   done,
			CompletedAt: &done,
		})
	}
	// After eviction, store is no larger than ~maxEntries. Exact count
	// depends on eviction policy (10% batches), but it must be below 200.
	if size := s.Size(); size >= 200 {
		t.Errorf("Size = %d after 200 Adds; eviction didn't fire", size)
	}
	// The very first completed request must have been evicted.
	if s.Get("r-000") != nil {
		t.Error("oldest completed entry r-000 should have been evicted")
	}
}

// TestStore_FIFO_DoesNotEvictInProgress confirms eviction skips
// non-completed entries.
func TestStore_FIFO_DoesNotEvictInProgress(t *testing.T) {
	s := New(Config{MaxEntries: 5})
	// Add 5 in-progress entries.
	for i := 0; i < 5; i++ {
		s.Add(&RequestInfo{
			RequestID: fmt.Sprintf("active-%d", i),
			StartedAt: time.Now(),
		})
	}
	// Try to overshoot with another in-progress entry.
	s.Add(&RequestInfo{RequestID: "active-5", StartedAt: time.Now()})
	// None of the original 5 should be gone — no completed candidates.
	for i := 0; i < 5; i++ {
		if s.Get(fmt.Sprintf("active-%d", i)) == nil {
			t.Errorf("active-%d should not have been evicted", i)
		}
	}
}

// TestStore_ConcurrentAccess — no data races under stress.
func TestStore_ConcurrentAccess(t *testing.T) {
	s := New(Config{MaxEntries: 1000})
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := fmt.Sprintf("g%d-%d", g, i)
				s.Add(&RequestInfo{RequestID: id, StartedAt: time.Now()})
				s.IncrementHook(id, "on_file", i%2 == 0)
				s.Get(id)
			}
		}(g)
	}
	wg.Wait()
}

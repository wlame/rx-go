package analyzer

import (
	"strings"
	"sync"
	"testing"
)

// resetRegistry unfreezes + clears between tests so each starts clean.
func resetRegistry(t *testing.T) {
	t.Helper()
	unfreezeForTest()
}

// registerTracking registers a tracking detector with the given name.
func registerTracking(name string) {
	RegisterLineDetector(func() LineDetector { return newTrackingDetector(name) })
}

func TestRegisterLineDetector_BeforeFreeze_Works(t *testing.T) {
	resetRegistry(t)
	registerTracking("one")
	if Len() != 1 {
		t.Errorf("expected Len=1, got %d", Len())
	}
}

func TestRegisterLineDetector_AfterFreeze_Panics(t *testing.T) {
	resetRegistry(t)
	Freeze()
	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected panic; got none")
			return
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "Register") {
			t.Errorf("unexpected panic payload: %v", r)
		}
	}()
	registerTracking("two")
}

func TestIsFrozen(t *testing.T) {
	resetRegistry(t)
	if IsFrozen() {
		t.Error("expected unfrozen after reset")
	}
	Freeze()
	if !IsFrozen() {
		t.Error("expected frozen after Freeze")
	}
}

func TestFreeze_Idempotent(t *testing.T) {
	resetRegistry(t)
	Freeze()
	Freeze() // should not panic
	Freeze()
	if !IsFrozen() {
		t.Error("still expected frozen")
	}
}

func TestSnapshot_ReturnsCopy(t *testing.T) {
	resetRegistry(t)
	registerTracking("a")
	registerTracking("b")

	s := Snapshot()
	if len(s) != 2 {
		t.Fatalf("expected 2, got %d", len(s))
	}
	// Mutating the snapshot must not affect the registry.
	s[0] = nil
	s2 := Snapshot()
	if s2[0] == nil {
		t.Errorf("registry leaked its backing slice to the caller")
	}
}

// Concurrent Read stress: once frozen, many goroutines reading the
// registry must be race-free. `go test -race` is the actual check.
func TestRegistry_ConcurrentReadsAfterFreeze(t *testing.T) {
	resetRegistry(t)
	for i := 0; i < 5; i++ {
		registerTracking(string(rune('a' + i)))
	}
	Freeze()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = LineDetectorSnapshot()
				_ = Snapshot()
				_ = Len()
			}
		}()
	}
	wg.Wait()
}

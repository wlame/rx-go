package analyzer

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// resetRegistry unfreezes + clears between tests so each starts clean.
func resetRegistry(t *testing.T) {
	t.Helper()
	unfreezeForTest()
}

func TestRegister_BeforeFreeze_Works(t *testing.T) {
	resetRegistry(t)
	Register(NoopAnalyzer{NameValue: "one", VersionValue: "1.0.0"})
	if Len() != 1 {
		t.Errorf("expected Len=1, got %d", Len())
	}
}

func TestRegister_AfterFreeze_Panics(t *testing.T) {
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
	Register(NoopAnalyzer{NameValue: "two", VersionValue: "1.0.0"})
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

func TestApplicableFor_EmptyRegistry(t *testing.T) {
	resetRegistry(t)
	got := ApplicableFor(Input{Path: "/tmp/a.log"})
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

// supportsGated is a Supports func that returns true only if MimeHint
// matches a configured substring. Used for filtering tests.
type supportsGated struct {
	NoopAnalyzer
	needle string
}

func (s supportsGated) Supports(_ string, mime string, _ int64) bool {
	return strings.Contains(mime, s.needle)
}

func TestApplicableFor_FiltersByMimeHint(t *testing.T) {
	resetRegistry(t)
	Register(supportsGated{
		NoopAnalyzer: NoopAnalyzer{NameValue: "text-only", VersionValue: "1.0.0"},
		needle:       "text/",
	})
	Register(supportsGated{
		NoopAnalyzer: NoopAnalyzer{NameValue: "json-only", VersionValue: "1.0.0"},
		needle:       "application/json",
	})

	got := ApplicableFor(Input{Path: "/tmp/a.log", MimeHint: "text/plain"})
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d: %v", len(got), got)
	}
	if got[0].Name() != "text-only" {
		t.Errorf("wrong analyzer: got %q", got[0].Name())
	}
}

func TestSnapshot_ReturnsCopy(t *testing.T) {
	resetRegistry(t)
	Register(NoopAnalyzer{NameValue: "a", VersionValue: "1.0.0"})
	Register(NoopAnalyzer{NameValue: "b", VersionValue: "1.0.0"})

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

func TestNoopAnalyzer_ContractRoundtrip(t *testing.T) {
	a := NoopAnalyzer{NameValue: "demo", VersionValue: "2.1.0"}
	if a.Name() != "demo" {
		t.Errorf("Name = %q", a.Name())
	}
	if a.Version() != "2.1.0" {
		t.Errorf("Version = %q", a.Version())
	}
	if !a.Supports("/any", "any", 0) {
		t.Errorf("Supports should be true for the noop analyzer")
	}
	rep, err := a.Analyze(context.Background(), Input{Path: "/any"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Name != "demo" || rep.Version != "2.1.0" {
		t.Errorf("report fields mismatch: %+v", rep)
	}
}

// Concurrent Read stress: once frozen, many goroutines reading the
// registry must be race-free. `go test -race` is the actual check.
func TestRegistry_ConcurrentReadsAfterFreeze(t *testing.T) {
	resetRegistry(t)
	for i := 0; i < 5; i++ {
		Register(NoopAnalyzer{NameValue: string(rune('a' + i)), VersionValue: "1.0.0"})
	}
	Freeze()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = ApplicableFor(Input{Path: "/tmp/any"})
				_ = Snapshot()
				_ = Len()
			}
		}()
	}
	wg.Wait()
}

func TestCachePath(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", "/tmp/testcache")
	got := CachePath("drain3", "1.2.3", "/var/log/app.log")
	// Expected:
	//   /tmp/testcache/rx/analyzers/drain3/v1.2.3/<16hex>_app.log.json
	if !strings.HasPrefix(got, "/tmp/testcache/rx/analyzers/drain3/v1.2.3/") {
		t.Errorf("wrong prefix: %q", got)
	}
	if !strings.HasSuffix(got, "_app.log.json") {
		t.Errorf("wrong suffix: %q", got)
	}
}

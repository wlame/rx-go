package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Fill-in tests for paths not hit by the main dispatcher_test.go file
// ============================================================================

func TestDispatcher_OnMatch_BuildsCorrectPayload(t *testing.T) {
	seen := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := NewDispatcher(DispatcherConfig{
		Env:       HookEnv{OnMatchURL: srv.URL},
		RequestID: "rid-7",
		Workers:   1,
		Logger:    newSilentLogger(),
	})
	d.OnMatch(context.Background(), "/var/log/x.log", trace.MatchInfo{
		Pattern:    "error",
		Offset:     1234,
		LineNumber: 42,
	})
	d.Close()
	d.Wait()

	q := <-seen
	if q.Get("event") != "match_found" {
		t.Errorf("event = %q", q.Get("event"))
	}
	if q.Get("pattern") != "error" {
		t.Errorf("pattern = %q", q.Get("pattern"))
	}
	if q.Get("line_number") != "42" {
		t.Errorf("line_number = %q", q.Get("line_number"))
	}
	if q.Get("offset") != "1234" {
		t.Errorf("offset = %q", q.Get("offset"))
	}
}

func TestDispatcher_OnComplete_BuildsCorrectPayload(t *testing.T) {
	var hit int32
	seen := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
		seen <- r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := NewDispatcher(DispatcherConfig{
		Env:       HookEnv{OnCompleteURL: srv.URL},
		RequestID: "req-complete",
		Workers:   1,
		Logger:    newSilentLogger(),
	})
	d.OnComplete(&rxtypes.TraceResponse{
		Path:         []string{"/var/log/a.log", "/var/log/b.log"},
		Patterns:     map[string]string{"p1": "foo", "p2": "bar"},
		Matches:      []rxtypes.Match{{}, {}, {}},
		ScannedFiles: []string{"a", "b"},
		SkippedFiles: []string{"c"},
		Time:         1.5,
	})
	d.Close()
	d.Wait()

	if atomic.LoadInt32(&hit) != 1 {
		t.Fatalf("on_complete hits = %d, want 1", hit)
	}
	q := <-seen
	if q.Get("event") != "trace_complete" {
		t.Errorf("event = %q", q.Get("event"))
	}
	if q.Get("paths") != "/var/log/a.log,/var/log/b.log" {
		t.Errorf("paths = %q", q.Get("paths"))
	}
	if q.Get("patterns") != "foo,bar" {
		t.Errorf("patterns = %q", q.Get("patterns"))
	}
	if q.Get("total_files_scanned") != "2" {
		t.Errorf("total_files_scanned = %q", q.Get("total_files_scanned"))
	}
	if q.Get("total_time_ms") != "1500" {
		t.Errorf("total_time_ms = %q", q.Get("total_time_ms"))
	}
}

func TestDispatcher_OnComplete_NilResponseNoOp(t *testing.T) {
	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
	}))
	t.Cleanup(srv.Close)
	d := NewDispatcher(DispatcherConfig{
		Env:    HookEnv{OnCompleteURL: srv.URL},
		Logger: newSilentLogger(),
	})
	d.OnComplete(nil)
	d.Close()
	d.Wait()
	if atomic.LoadInt32(&hit) != 0 {
		t.Errorf("unexpected POST with nil response: %d", hit)
	}
}

func TestValidateConfig_RejectsBadEntry(t *testing.T) {
	if err := ValidateConfig(HookConfig{OnFileURL: "ftp://bad"}); err == nil {
		t.Error("want validation error")
	}
	if err := ValidateConfig(HookConfig{}); err != nil {
		t.Errorf("empty config should validate; got %v", err)
	}
}

func TestJoinStrings(t *testing.T) {
	if got := joinStrings(nil, ","); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := joinStrings([]string{"a"}, ","); got != "a" {
		t.Errorf("single: got %q", got)
	}
	if got := joinStrings([]string{"a", "b", "c"}, ","); got != "a,b,c" {
		t.Errorf("multi: got %q", got)
	}
}

func TestJoinMap_PatternIDOrder(t *testing.T) {
	m := map[string]string{"p3": "c", "p1": "a", "p2": "b"}
	got := joinMap(m, ",")
	if got != "a,b,c" {
		t.Errorf("joinMap = %q, want a,b,c", got)
	}
	// Empty map.
	if got := joinMap(nil, ","); got != "" {
		t.Errorf("empty map: got %q", got)
	}
}

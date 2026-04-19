package hooks

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// newSilentLogger drops all log output so test noise stays clean.
func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDispatcher_FireOnFile_PostsToConfiguredURL confirms the
// happy path — a configured OnFile URL gets hit with the Python-
// compatible query-param payload.
func TestDispatcher_FireOnFile_PostsToConfiguredURL(t *testing.T) {
	var (
		hits int32
		got  chan *http.Request
	)
	got = make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		got <- r
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := NewDispatcher(DispatcherConfig{
		Env:        HookEnv{OnFileURL: srv.URL},
		RequestID:  "req-42",
		Workers:    1,
		QueueDepth: 4,
		Logger:     newSilentLogger(),
	})

	d.OnFile(context.Background(), "/var/log/test.log",
		trace.FileInfo{FileSizeBytes: 1024, ScanTimeMS: 50, MatchesCount: 3})

	d.Close()
	d.Wait()

	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("server got %d hits, want 1", hits)
	}
	req := <-got
	q := req.URL.Query()
	if q.Get("event") != "file_scanned" {
		t.Errorf("event = %q, want file_scanned", q.Get("event"))
	}
	if q.Get("request_id") != "req-42" {
		t.Errorf("request_id = %q", q.Get("request_id"))
	}
	if q.Get("file_path") != "/var/log/test.log" {
		t.Errorf("file_path = %q", q.Get("file_path"))
	}
	if q.Get("file_size_bytes") != "1024" {
		t.Errorf("file_size_bytes = %q, want 1024", q.Get("file_size_bytes"))
	}
	if q.Get("matches_count") != "3" {
		t.Errorf("matches_count = %q", q.Get("matches_count"))
	}
}

// TestDispatcher_UnsetURL_NoOp skips enqueueing entirely when the
// corresponding URL is empty. Confirms we don't POST to "".
func TestDispatcher_UnsetURL_NoOp(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)
	d := NewDispatcher(DispatcherConfig{
		Env:    HookEnv{}, // all URLs empty
		Logger: newSilentLogger(),
	})

	d.OnFile(context.Background(), "/x", trace.FileInfo{})
	d.OnMatch(context.Background(), "/x", trace.MatchInfo{})

	d.Close()
	d.Wait()

	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("expected no HTTP calls, got %d", hits)
	}
}

// TestDispatcher_FailingURL_LogsAndContinues verifies fire-and-forget:
// a 500 response doesn't crash, and subsequent events still go through.
func TestDispatcher_FailingURL_LogsAndContinues(t *testing.T) {
	var got int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	d := NewDispatcher(DispatcherConfig{
		Env:     HookEnv{OnFileURL: srv.URL},
		Workers: 1,
		Logger:  newSilentLogger(),
	})
	d.OnFile(context.Background(), "/a", trace.FileInfo{})
	d.OnFile(context.Background(), "/b", trace.FileInfo{})
	d.OnFile(context.Background(), "/c", trace.FileInfo{})

	d.Close()
	d.Wait()

	// All three POSTs attempted; all failed but dispatcher didn't stall.
	if atomic.LoadInt32(&got) != 3 {
		t.Errorf("got %d server hits, want 3", got)
	}
}

// TestDispatcher_TimeoutDoesNotBlockProducer ensures a slow hook
// target does NOT stall the trace engine.
func TestDispatcher_TimeoutDoesNotBlockProducer(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hang until test closes it
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	d := NewDispatcher(DispatcherConfig{
		Env:        HookEnv{OnFileURL: srv.URL},
		Workers:    1,
		QueueDepth: 1,
		Timeout:    50 * time.Millisecond,
		Logger:     newSilentLogger(),
	})

	start := time.Now()
	// First event ties up the single worker immediately.
	d.OnFile(context.Background(), "/x", trace.FileInfo{})
	// Second event goes on the queue.
	d.OnFile(context.Background(), "/y", trace.FileInfo{})
	// Third event — queue now full (depth=1, one in-flight, one queued),
	// so this is dropped.
	d.OnFile(context.Background(), "/z", trace.FileInfo{})

	producerLatency := time.Since(start)
	if producerLatency > 100*time.Millisecond {
		t.Errorf("producer blocked %v (> 100 ms); fire-and-forget violated",
			producerLatency)
	}
	d.Close()
	d.Wait()
}

// TestDispatcher_RespectsDisableCustom confirms overrides are dropped
// when the security flag is set.
func TestDispatcher_RespectsDisableCustom(t *testing.T) {
	var envHit, overrideHit int32
	envSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&envHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	overrideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&overrideHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(envSrv.Close)
	t.Cleanup(overrideSrv.Close)

	override := overrideSrv.URL
	d := NewDispatcher(DispatcherConfig{
		Env: HookEnv{
			OnFileURL:     envSrv.URL,
			DisableCustom: true,
		},
		RequestOverrides: HookOverrides{OnFileURL: &override},
		Workers:          1,
		Logger:           newSilentLogger(),
	})

	d.OnFile(context.Background(), "/x", trace.FileInfo{})

	d.Close()
	d.Wait()

	if atomic.LoadInt32(&envHit) != 1 {
		t.Errorf("env URL hits = %d, want 1", envHit)
	}
	if atomic.LoadInt32(&overrideHit) != 0 {
		t.Errorf("override URL hits = %d, want 0 (DisableCustom)", overrideHit)
	}
}

// TestDispatcher_ImplementsTraceHookFirer — compile-time assertion is
// already in dispatcher.go via `var _ trace.HookFirer = ...`. We add
// a test that uses it directly so the import actually survives a
// dead-code eliminator pass.
func TestDispatcher_ImplementsTraceHookFirer(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{Logger: newSilentLogger()})
	var hf trace.HookFirer = d
	hf.OnFile(context.Background(), "/a", trace.FileInfo{})
	hf.OnMatch(context.Background(), "/a", trace.MatchInfo{})
	d.Close()
	d.Wait()
}

// TestDispatcher_EnqueueAfterClose_NoPanic covers Stage 8 Reviewer 2
// High #5: the dispatcher's Close() uses a sync.Once to close the
// queue channel. Before the fix, a subsequent enqueue() call would
// attempt `d.queue <- ev` on a closed channel and panic ("send on
// closed channel"). This scenario is realistic because the trace
// engine's hook-fire goroutines can outlive the HTTP request that
// owns the dispatcher, especially at shutdown time.
//
// Expected post-fix behavior: enqueue after Close is a silent no-op
// (the event is effectively dropped). The dispatcher does NOT panic,
// and the event MAY be recorded as dropped for observability.
//
// To reliably trigger the race, we call Close() first and then
// immediately invoke one of the public trace.HookFirer methods that
// internally call enqueue. If enqueue panics, the test fails; we
// use a recover() trampoline to catch it cleanly.
func TestDispatcher_EnqueueAfterClose_NoPanic(t *testing.T) {
	// No network server needed — we never reach the HTTP layer because
	// the queue is closed and workers have exited. We only need a valid
	// URL so enqueue is attempted at all (empty URL early-returns in
	// OnFile/OnMatch).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := NewDispatcher(DispatcherConfig{
		Env: HookEnv{
			OnFileURL:     srv.URL,
			OnMatchURL:    srv.URL,
			OnCompleteURL: srv.URL,
		},
		Workers:    1,
		QueueDepth: 4,
		Logger:     newSilentLogger(),
	})

	// Close immediately — the channel is now closed.
	d.Close()
	d.Wait()

	// Now try to enqueue. Pre-fix: the select's `case d.queue <- ev`
	// panics because sends to a closed channel panic (they don't fall
	// through to `default`). Post-fix: the closed-flag guard short-
	// circuits before the send.
	//
	// Recover any panic; the test asserts that enqueue never panics.
	assertNoPanic := func(name string, fn func()) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s panicked after Close: %v", name, r)
			}
		}()
		fn()
	}

	assertNoPanic("OnFile", func() {
		d.OnFile(context.Background(), "/after-close.log",
			trace.FileInfo{FileSizeBytes: 1, ScanTimeMS: 1, MatchesCount: 0})
	})
	assertNoPanic("OnMatch", func() {
		d.OnMatch(context.Background(), "/after-close.log",
			trace.MatchInfo{Pattern: "x", Offset: 0, LineNumber: 1})
	})
	assertNoPanic("OnComplete", func() {
		d.OnComplete(&rxtypes.TraceResponse{
			Path:         []string{"/after-close.log"},
			Patterns:     map[string]string{"p1": "x"},
			ScannedFiles: []string{},
			SkippedFiles: []string{},
			Matches:      []rxtypes.Match{},
		})
	})
}

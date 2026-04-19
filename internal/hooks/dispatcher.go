package hooks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Dispatcher
// ============================================================================

// Dispatcher enqueues hook events onto a buffered channel and dispatches
// them from a pool of worker goroutines. Implements trace.HookFirer.
//
// Fire-and-forget semantics (user decision 6.9.2): Enqueue returns as
// soon as the event is on the channel. If the channel is full (pool
// drained), we drop the event, log a warning, and bump a metric —
// better to drop a notification than block the search pipeline.
//
// Dispatcher is safe for concurrent use. The zero value is NOT valid;
// always construct via NewDispatcher.
type Dispatcher struct {
	cfg        DispatcherConfig
	httpClient *http.Client
	queue      chan hookEvent
	wg         sync.WaitGroup
	stopped    chan struct{}
	stopOnce   sync.Once
	logger     *slog.Logger
	// closed is flipped to true BEFORE the queue channel is closed
	// by Close(). enqueue() checks this flag first and short-circuits,
	// avoiding a "send on closed channel" panic if a trace goroutine
	// fires a hook event after Close has been initiated.
	//
	// Using atomic.Bool instead of a mutex keeps the fast path
	// (running dispatcher) branch-prediction-friendly and lock-free.
	// Memory ordering: Close() calls Store(true) before close(queue);
	// enqueue() calls Load() before the send. Go's atomic operations
	// establish a happens-before relationship so any enqueue that
	// observes closed=false also observes the still-open channel.
	closed atomic.Bool
}

// DispatcherConfig holds the knobs for a Dispatcher.
type DispatcherConfig struct {
	// Env is the process-wide hook env.
	Env HookEnv
	// RequestOverrides applies only to events fired by the trace
	// engine instance that owns this Dispatcher (one per request in
	// the HTTP path; one global dispatcher for the CLI).
	RequestOverrides HookOverrides
	// RequestID is injected into every payload's request_id field.
	// Set per-trace-request in the HTTP layer; empty in CLI.
	RequestID string
	// QueueDepth — size of the buffered channel. 0 uses the default.
	QueueDepth int
	// Workers — number of goroutines. 0 uses the default.
	Workers int
	// Timeout for each HTTP POST. 0 uses DefaultTimeout.
	Timeout time.Duration
	// Logger — slog for structured output. nil uses slog.Default().
	Logger *slog.Logger
}

// hookEvent is a single unit of work on the queue. Which URL to hit
// and the query parameters to send are pre-resolved here so the
// worker goroutines don't need to re-read config.
type hookEvent struct {
	kind      string // "on_file" | "on_match" | "on_complete"
	url       string // already resolved (env or override)
	params    url.Values
	requestID string
}

// NewDispatcher constructs a Dispatcher and starts the worker pool.
// Callers must call Close when done to drain the queue and stop the
// goroutines. Using defer d.Close() at the HTTP-handler level ties
// the dispatcher lifetime to a request.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	d := &Dispatcher{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.Timeout},
		queue:      make(chan hookEvent, cfg.QueueDepth),
		stopped:    make(chan struct{}),
		logger:     cfg.Logger,
	}
	for i := 0; i < cfg.Workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// Close signals the worker pool to drain remaining queued events and
// stop. Safe to call multiple times; only the first has effect.
//
// Close does NOT cancel in-flight HTTP requests — they run to their
// 3-second timeout. This ensures any already-queued event gets a
// chance to fire, matching user intent for "fire-and-forget but don't
// drop pre-queued events at shutdown".
//
// ORDERING NOTE (Stage 8 High #5 fix): we flip the `closed` flag
// BEFORE closing the channel. Any enqueue() call that races with
// Close will either:
//
//   - See closed=false and safely send to the still-open channel
//     (possibly winning the race and having its event processed), OR
//   - See closed=true and short-circuit, avoiding a send-on-closed
//     panic.
//
// Because Load/Store on atomic.Bool is sequentially consistent, no
// goroutine can observe closed=false while also observing the channel
// as closed. See TestDispatcher_EnqueueAfterClose_NoPanic.
func (d *Dispatcher) Close() {
	d.stopOnce.Do(func() {
		d.closed.Store(true)
		close(d.queue)
	})
	d.wg.Wait()
	close(d.stopped)
}

// Wait blocks until all queued events have been processed AND Close
// has been called. Tests use this to observe all expected POSTs
// deterministically.
func (d *Dispatcher) Wait() { <-d.stopped }

// ============================================================================
// trace.HookFirer implementation
// ============================================================================

// OnFile satisfies trace.HookFirer. Enqueues an on_file event with
// Python-compatible query-param payload.
func (d *Dispatcher) OnFile(_ context.Context, path string, info trace.FileInfo) {
	cfg := EffectiveHooks(d.cfg.Env, d.cfg.RequestOverrides)
	if cfg.OnFileURL == "" {
		return
	}
	payload := rxtypes.FileScannedPayload{
		Event:         rxtypes.HookEventFileScanned,
		RequestID:     d.cfg.RequestID,
		FilePath:      path,
		FileSizeBytes: info.FileSizeBytes,
		ScanTimeMS:    info.ScanTimeMS,
		MatchesCount:  info.MatchesCount,
	}
	d.enqueue(hookEvent{
		kind:      "on_file",
		url:       cfg.OnFileURL,
		params:    payloadToParams(payload),
		requestID: d.cfg.RequestID,
	})
}

// OnMatch satisfies trace.HookFirer. Enqueues an on_match event per
// matched line (can be very hot). Skipped entirely if URL unset.
func (d *Dispatcher) OnMatch(_ context.Context, path string, m trace.MatchInfo) {
	cfg := EffectiveHooks(d.cfg.Env, d.cfg.RequestOverrides)
	if cfg.OnMatchURL == "" {
		return
	}
	ln := m.LineNumber
	payload := rxtypes.MatchFoundPayload{
		Event:      rxtypes.HookEventMatchFound,
		RequestID:  d.cfg.RequestID,
		FilePath:   path,
		Pattern:    m.Pattern,
		Offset:     m.Offset,
		LineNumber: &ln,
	}
	d.enqueue(hookEvent{
		kind:      "on_match",
		url:       cfg.OnMatchURL,
		params:    payloadToParams(payload),
		requestID: d.cfg.RequestID,
	})
}

// OnComplete is NOT part of trace.HookFirer (the interface only has
// OnFile / OnMatch), but is exposed here for the HTTP layer to fire
// the trace-complete webhook when the engine returns.
func (d *Dispatcher) OnComplete(resp *rxtypes.TraceResponse) {
	cfg := EffectiveHooks(d.cfg.Env, d.cfg.RequestOverrides)
	if cfg.OnCompleteURL == "" || resp == nil {
		return
	}
	payload := rxtypes.TraceCompletePayload{
		Event:             rxtypes.HookEventTraceComplete,
		RequestID:         d.cfg.RequestID,
		Paths:             joinStrings(resp.Path, ","),
		Patterns:          joinMap(resp.Patterns, ","),
		TotalFilesScanned: len(resp.ScannedFiles),
		TotalFilesSkipped: len(resp.SkippedFiles),
		TotalMatches:      len(resp.Matches),
		TotalTimeMS:       int(resp.Time * 1000),
	}
	d.enqueue(hookEvent{
		kind:      "on_complete",
		url:       cfg.OnCompleteURL,
		params:    payloadToParams(payload),
		requestID: d.cfg.RequestID,
	})
}

// ============================================================================
// Worker loop
// ============================================================================

// worker consumes events from the queue until it's closed. Each event
// is one HTTP GET (Python uses GET so it's trivially idempotent from
// the webhook target's perspective — POSTing query params changes the
// semantics).
//
// NOTE: Python's hooks use GET with query parameters, not POST.
// rx-python/src/rx/hooks.py::_call_hook_internal does:
//
//	response = client.get(url, params=params)
//
// so the Go port uses http.MethodGet for exact parity.
func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for ev := range d.queue {
		d.fire(ev)
	}
}

// fire executes a single hook POST (technically GET per parity note).
// Any failure is logged and metered; we never return an error because
// the caller has already moved on.
func (d *Dispatcher) fire(ev hookEvent) {
	start := time.Now()
	target := ev.url
	if len(ev.params) > 0 {
		// The params slice is already URL-encoded; append with correct
		// separator depending on existing query.
		sep := "?"
		if u, err := url.Parse(ev.url); err == nil && u.RawQuery != "" {
			sep = "&"
		}
		target = ev.url + sep + ev.params.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		d.recordFailure(ev.kind, err, start, ev.requestID)
		return
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		d.recordFailure(ev.kind, err, start, ev.requestID)
		return
	}
	// Drain the body so keep-alive can reuse the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		d.recordFailure(ev.kind,
			fmt.Errorf("non-2xx status %d", resp.StatusCode),
			start, ev.requestID)
		return
	}
	prometheus.RecordHook(ev.kind, "success")
	// Successful hook calls stay silent at default log level — they're
	// expected and high-volume. Use Debug for observability.
	d.logger.Debug("hook_fired",
		"kind", ev.kind,
		"status", resp.StatusCode,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"request_id", ev.requestID,
	)
}

// recordFailure centralizes the "hook failed" bookkeeping.
func (d *Dispatcher) recordFailure(kind string, err error, start time.Time, requestID string) {
	prometheus.RecordHook(kind, "failure")
	d.logger.Warn("hook_failed",
		"kind", kind,
		"error", err.Error(),
		"elapsed_ms", time.Since(start).Milliseconds(),
		"request_id", requestID,
	)
}

// enqueue puts an event on the channel. If the channel is full we
// drop the event and log a warning — fire-and-forget means we
// absolutely MUST NOT block the trace pipeline on a slow webhook.
//
// POST-CLOSE GUARD (Stage 8 High #5): if Close() has already been
// called, short-circuit. This handles the race where a trace engine's
// still-running goroutine fires an OnFile / OnMatch after the HTTP
// layer has shut down the dispatcher. Without the guard, the `case
// d.queue <- ev` branch panics ("send on closed channel"); unlike
// receives, sends on a closed channel do NOT fall through to the
// select's default clause.
func (d *Dispatcher) enqueue(ev hookEvent) {
	if d.closed.Load() {
		// Dispatcher is shutting down or already shut down. Record
		// the drop for observability and return quietly. This keeps
		// the trace engine's cleanup path race-free during server
		// shutdown.
		prometheus.RecordHook(ev.kind, "dropped")
		return
	}
	select {
	case d.queue <- ev:
	default:
		// Queue full — drop event. This is rare in practice but would
		// happen if a webhook target is completely unreachable and all
		// workers are timed out; better to drop than to stall the scan.
		prometheus.RecordHook(ev.kind, "dropped")
		d.logger.Warn("hook_queue_full_dropped",
			"kind", ev.kind,
			"request_id", ev.requestID,
		)
	}
}

// ============================================================================
// Helpers for payload → query param
// ============================================================================
//
// Python sends hook payloads as URL query parameters via httpx's
// `params=` argument. We do the same: reflect on the known payload
// types and flatten into url.Values.

// payloadToParams is a small, case-by-case converter. There are only
// three payload shapes, so a type switch is cleaner than reflection.
func payloadToParams(p any) url.Values {
	out := url.Values{}
	switch v := p.(type) {
	case rxtypes.FileScannedPayload:
		out.Set("event", v.Event)
		out.Set("request_id", v.RequestID)
		out.Set("file_path", v.FilePath)
		out.Set("file_size_bytes", strconv.FormatInt(v.FileSizeBytes, 10))
		out.Set("scan_time_ms", strconv.Itoa(v.ScanTimeMS))
		out.Set("matches_count", strconv.Itoa(v.MatchesCount))
	case rxtypes.MatchFoundPayload:
		out.Set("event", v.Event)
		out.Set("request_id", v.RequestID)
		out.Set("file_path", v.FilePath)
		out.Set("pattern", v.Pattern)
		out.Set("offset", strconv.FormatInt(v.Offset, 10))
		if v.LineNumber != nil {
			out.Set("line_number", strconv.FormatInt(*v.LineNumber, 10))
		}
	case rxtypes.TraceCompletePayload:
		out.Set("event", v.Event)
		out.Set("request_id", v.RequestID)
		out.Set("paths", v.Paths)
		out.Set("patterns", v.Patterns)
		out.Set("total_files_scanned", strconv.Itoa(v.TotalFilesScanned))
		out.Set("total_files_skipped", strconv.Itoa(v.TotalFilesSkipped))
		out.Set("total_matches", strconv.Itoa(v.TotalMatches))
		out.Set("total_time_ms", strconv.Itoa(v.TotalTimeMS))
	}
	return out
}

// joinStrings flattens a []string with a separator. Needed for
// payload construction where Python uses ",".join(paths).
func joinStrings(xs []string, sep string) string {
	if len(xs) == 0 {
		return ""
	}
	if len(xs) == 1 {
		return xs[0]
	}
	// strings.Join wrapper so the intent at the call site is explicit.
	var out string
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}

// joinMap flattens a map[string]string to its VALUES (in pattern-id
// order: p1, p2, p3...). Python emits the comma-joined pattern list,
// NOT the keys.
func joinMap(m map[string]string, sep string) string {
	if len(m) == 0 {
		return ""
	}
	// Build a stable pattern-id ordering (p1, p2, ..., pN). We assume
	// keys are "pN" — if not, fall through to map iteration (unstable,
	// but only affects a hook notification, not correctness).
	var out string
	for i := 1; i <= len(m); i++ {
		k := fmt.Sprintf("p%d", i)
		v, ok := m[k]
		if !ok {
			break
		}
		if i > 1 {
			out += sep
		}
		out += v
	}
	if out == "" {
		// Fallback — shouldn't happen with engine-produced patterns.
		for _, v := range m {
			if out != "" {
				out += sep
			}
			out += v
		}
	}
	return out
}

// ============================================================================
// Type assertions
// ============================================================================

// Assert that *Dispatcher satisfies trace.HookFirer — compile-time
// guarantee that the engine can accept us without a type switch.
var _ trace.HookFirer = (*Dispatcher)(nil)

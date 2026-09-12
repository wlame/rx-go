package hooks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
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

// Dispatcher delivers webhook events. It owns one bounded queue (a
// buffered channel), one HTTP client and a pool of worker goroutines
// that read from the queue, and it is shared by every trace request of
// a process: one per `rx serve`, one per `rx trace` invocation.
//
// The Dispatcher holds no hook URLs and no request ID. Those belong to a
// single trace request, so a caller asks for a per-request view with
// ForRequest and hands that view to the trace engine. Every view sends
// its events through the same queue and client, which keeps the number
// of goroutines and connections bounded however many requests run at
// once, while each event carries the URL and request_id of the request
// that produced it.
//
// Fire-and-forget semantics: enqueueing returns as soon as the event is
// on the channel. If the channel is full, the event is dropped, a
// warning is logged and a metric is bumped — better to drop a
// notification than block the search pipeline.
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

// DispatcherConfig holds the knobs for a Dispatcher. Every field is
// optional; a zero value takes the default named in its comment.
type DispatcherConfig struct {
	// QueueDepth is the size of the buffered channel. 0 uses
	// DefaultQueueDepth.
	QueueDepth int
	// Workers is the number of goroutines that deliver events. 0 uses
	// DefaultWorkers.
	Workers int
	// Timeout bounds each webhook call. 0 uses DefaultTimeout.
	Timeout time.Duration
	// Logger receives structured output. nil uses slog.Default().
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
// Callers must call Close, then Wait, when done: Close stops new events
// and lets the workers drain the queue, Wait blocks until they have.
// `rx serve` does this at shutdown, `rx trace` before the process exits.
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
		cfg: cfg,
		// SECURITY: refuse every redirect. ValidateURL only ever sees
		// the URL the operator configured; a target that answers 302
		// would otherwise steer the request to a host nobody vetted,
		// including the loopback and metadata addresses the SSRF guard
		// exists to block. ErrUseLastResponse makes Do return the 3xx
		// itself, which the caller then reports as a non-2xx failure.
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				cfg.Logger.Debug("hook_redirect_refused", "target", req.URL.Redacted())
				return http.ErrUseLastResponse
			},
			// SECURITY: re-apply the address policy at dial time.
			// ValidateURL resolves the hostname when the hook is
			// configured; this client resolves it again when the request
			// goes out, and a name can answer differently the second
			// time. guardedDialer checks the literal IP being connected
			// to, which is the only point with no window left.
			Transport: &http.Transport{
				DialContext:         guardedDialer(nil, parseBoolEnv(os.Getenv("RX_ALLOW_INTERNAL_HOOKS"))),
				TLSHandshakeTimeout: cfg.Timeout,
			},
		},
		queue:   make(chan hookEvent, cfg.QueueDepth),
		stopped: make(chan struct{}),
		logger:  cfg.Logger,
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
// ORDERING NOTE: we flip the `closed` flag
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
// has been called. Tests use this to observe all expected calls
// deterministically.
func (d *Dispatcher) Wait() { <-d.stopped }

// ============================================================================
// Per-request view
// ============================================================================

// RequestHooks is the view of a Dispatcher for one trace request: the
// hook URLs in effect for that request and the request_id its payloads
// carry. It implements trace.HookFirer, so the trace engine calls OnFile
// and OnMatch on it directly, and it adds OnComplete for the caller to
// fire once the engine has returned.
//
// A RequestHooks is a small value with no goroutines of its own; it is
// cheap to create one per request and needs no Close. Its events go
// through the parent Dispatcher's queue, so they stop being delivered
// once the parent is closed.
type RequestHooks struct {
	dispatcher *Dispatcher
	urls       HookConfig
	requestID  string
}

// ForRequest returns the view that sends one request's events to urls,
// with requestID in every payload. urls is normally the result of
// EffectiveHooks for that request and should already have passed
// ValidateConfig; an empty URL switches that event off.
//
// INVARIANT: the view copies urls and requestID, so concurrent requests
// never see each other's values — the shared Dispatcher holds neither.
func (d *Dispatcher) ForRequest(urls HookConfig, requestID string) *RequestHooks {
	return &RequestHooks{dispatcher: d, urls: urls, requestID: requestID}
}

// OnFile satisfies trace.HookFirer. It enqueues a file_scanned event
// for the request's on_file URL; without that URL it does nothing.
func (r *RequestHooks) OnFile(_ context.Context, path string, info trace.FileInfo) {
	if r.urls.OnFileURL == "" {
		return
	}
	payload := rxtypes.FileScannedPayload{
		Event:         rxtypes.HookEventFileScanned,
		RequestID:     r.requestID,
		FilePath:      path,
		FileSizeBytes: info.FileSizeBytes,
		ScanTimeMS:    info.ScanTimeMS,
		MatchesCount:  info.MatchesCount,
	}
	r.dispatcher.enqueue(hookEvent{
		kind:      "on_file",
		url:       r.urls.OnFileURL,
		params:    payloadToParams(payload),
		requestID: r.requestID,
	})
}

// OnMatch satisfies trace.HookFirer. It enqueues a match_found event per
// matched line for the request's on_match URL. The engine calls it once
// per match, so it returns at once when that URL is not set.
func (r *RequestHooks) OnMatch(_ context.Context, path string, m trace.MatchInfo) {
	if r.urls.OnMatchURL == "" {
		return
	}
	ln := m.LineNumber
	payload := rxtypes.MatchFoundPayload{
		Event:      rxtypes.HookEventMatchFound,
		RequestID:  r.requestID,
		FilePath:   path,
		Pattern:    m.Pattern,
		Offset:     m.Offset,
		LineNumber: &ln,
	}
	r.dispatcher.enqueue(hookEvent{
		kind:      "on_match",
		url:       r.urls.OnMatchURL,
		params:    payloadToParams(payload),
		requestID: r.requestID,
	})
}

// OnComplete enqueues the trace_complete event for a finished trace. It
// is not part of trace.HookFirer, because the engine does not know when
// the caller considers the request complete; the HTTP handler and the
// CLI call it after the engine returns. A nil response, or no
// on_complete URL, does nothing.
func (r *RequestHooks) OnComplete(resp *rxtypes.TraceResponse) {
	if r.urls.OnCompleteURL == "" || resp == nil {
		return
	}
	payload := rxtypes.TraceCompletePayload{
		Event:             rxtypes.HookEventTraceComplete,
		RequestID:         r.requestID,
		Paths:             joinStrings(resp.Path, ","),
		Patterns:          joinMap(resp.Patterns, ","),
		TotalFilesScanned: len(resp.ScannedFiles),
		TotalFilesSkipped: len(resp.SkippedFiles),
		TotalMatches:      len(resp.Matches),
		TotalTimeMS:       int(resp.Time * 1000),
	}
	r.dispatcher.enqueue(hookEvent{
		kind:      "on_complete",
		url:       r.urls.OnCompleteURL,
		params:    payloadToParams(payload),
		requestID: r.requestID,
	})
}

// ============================================================================
// Worker loop
// ============================================================================

// worker consumes events from the queue until it's closed. Each event
// is one HTTP GET whose payload travels as query parameters, with no
// request body — the protocol rx-python uses
// (rx-python/src/rx/hooks.py::_call_hook_internal calls
// client.get(url, params=params)), so a webhook target serves both
// backends.
func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for ev := range d.queue {
		d.fire(ev)
	}
}

// fire executes a single webhook GET. Any failure is logged and
// metered; it never returns an error because the caller has already
// moved on.
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
	prometheus.RecordHookDuration(ev.kind, time.Since(start))
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
	// A failed call still cost wall-clock time — a hook target that
	// times out is exactly what the latency histogram should show.
	prometheus.RecordHookDuration(kind, time.Since(start))
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
// POST-CLOSE GUARD: if Close() has already been
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

// Assert that *RequestHooks satisfies trace.HookFirer — a compile-time
// guarantee that the engine accepts the per-request view directly.
var _ trace.HookFirer = (*RequestHooks)(nil)

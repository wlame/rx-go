package webapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
)

// Webhook delivery over HTTP. A caller that runs a long trace through
// GET /v1/trace and passes hook_on_* URLs must receive the events of its
// own request, at the URLs it gave, carrying the request_id of the
// response — and no other caller's events.

// webhookReceiver is a webhook target that remembers the query of every
// call it received.
type webhookReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []url.Values
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	rec := &webhookReceiver{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, r.URL.Query())
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

// received returns a copy of the calls seen so far.
func (rec *webhookReceiver) received() []url.Values {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]url.Values(nil), rec.calls...)
}

// eventsOf returns the calls whose event parameter equals event.
func (rec *webhookReceiver) eventsOf(event string) []url.Values {
	var out []url.Values
	for _, call := range rec.received() {
		if call.Get("event") == event {
			out = append(out, call)
		}
	}
	return out
}

// hookedTrace is a trace server wired the way `rx serve` wires it: one
// shared dispatcher for every request. drain closes the dispatcher and
// waits until every queued event has been delivered, so a test can
// count the calls without sleeping.
type hookedTrace struct {
	server *httptest.Server
	dir    string
	files  []string
	drain  func()
}

// newHookedTrace writes two searchable files under a sandbox root and
// starts a server with a hook dispatcher. Loopback hook targets are
// allowed through RX_ALLOW_INTERNAL_HOOKS, the opt-in an operator with
// a local collector uses; the RX_HOOK_* variables start empty.
func newHookedTrace(t *testing.T) *hookedTrace {
	t.Helper()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skipf("ripgrep not installed: %v", err)
	}
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	t.Setenv("RX_HOOK_ON_FILE_URL", "")
	t.Setenv("RX_HOOK_ON_MATCH_URL", "")
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", "")
	t.Setenv("RX_DISABLE_CUSTOM_HOOKS", "")
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	dir := t.TempDir()
	var files []string
	for _, name := range []string{"a.log", "b.log"} {
		path := filepath.Join(dir, name)
		body := "one error here\ntwo error here\nthree error here\nfour error here\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		files = append(files, path)
	}
	if err := paths.SetSearchRoots([]string{dir}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	dispatcher := hooks.NewDispatcher(hooks.DispatcherConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var drainOnce sync.Once
	drain := func() {
		drainOnce.Do(func() {
			dispatcher.Close()
			dispatcher.Wait()
		})
	}
	t.Cleanup(drain)

	srv := NewServer(Config{
		AppVersion:  "hooks-test",
		RipgrepPath: rgPath,
		TaskManager: tasks.New(tasks.Config{}),
		Hooks:       dispatcher,
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &hookedTrace{server: ts, dir: dir, files: files, drain: drain}
}

// trace runs GET /v1/trace over the fixture directory with extra query
// parameters and returns the status and, on 200, the request_id.
func (h *hookedTrace) trace(t *testing.T, extra url.Values) (status int, requestID string) {
	t.Helper()
	q := url.Values{}
	q.Set("path", h.dir)
	q.Set("regexp", "error")
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	resp, err := http.Get(h.server.URL + "/v1/trace?" + q.Encode())
	if err != nil {
		t.Errorf("get: %v", err)
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Errorf("decode: %v", err)
	}
	return resp.StatusCode, body.RequestID
}

// mustTrace is trace for a request that has to succeed.
func (h *hookedTrace) mustTrace(t *testing.T, extra url.Values) string {
	t.Helper()
	status, requestID := h.trace(t, extra)
	if status != http.StatusOK {
		t.Fatalf("trace status: got %d, want 200", status)
	}
	if requestID == "" {
		t.Fatal("trace response has no request_id")
	}
	return requestID
}

// assertRequestID fails for every call whose request_id differs from want.
func assertRequestID(t *testing.T, calls []url.Values, want string) {
	t.Helper()
	for _, call := range calls {
		if got := call.Get("request_id"); got != want {
			t.Errorf("request_id: got %q, want %q (call %v)", got, want, call)
		}
	}
}

func TestTraceHooks_HTTP_OnCompleteReachesTheRequestURL(t *testing.T) {
	h := newHookedTrace(t)
	rec := newWebhookReceiver(t)

	requestID := h.mustTrace(t, url.Values{"hook_on_complete": {rec.server.URL}})
	h.drain()

	calls := rec.received()
	if len(calls) != 1 {
		t.Fatalf("hook calls: got %d, want 1", len(calls))
	}
	if calls[0].Get("event") != "trace_complete" {
		t.Errorf("event: got %q, want trace_complete", calls[0].Get("event"))
	}
	if calls[0].Get("total_matches") != "8" {
		t.Errorf("total_matches: got %q, want 8", calls[0].Get("total_matches"))
	}
	assertRequestID(t, calls, requestID)
}

func TestTraceHooks_HTTP_OnFileFiresPerFile(t *testing.T) {
	h := newHookedTrace(t)
	rec := newWebhookReceiver(t)

	requestID := h.mustTrace(t, url.Values{"hook_on_file": {rec.server.URL}})
	h.drain()

	calls := rec.eventsOf("file_scanned")
	if len(calls) != 2 || len(rec.received()) != 2 {
		t.Fatalf("file_scanned calls: got %d of %d, want 2 of 2", len(calls), len(rec.received()))
	}
	var names []string
	for _, call := range calls {
		names = append(names, filepath.Base(call.Get("file_path")))
	}
	sort.Strings(names)
	if names[0] != "a.log" || names[1] != "b.log" {
		t.Errorf("file_path values: got %v, want [a.log b.log]", names)
	}
	assertRequestID(t, calls, requestID)
}

func TestTraceHooks_HTTP_OnMatchFiresPerMatchUpToTheCap(t *testing.T) {
	h := newHookedTrace(t)
	rec := newWebhookReceiver(t)

	const maxResults = 3
	requestID := h.mustTrace(t, url.Values{
		"hook_on_match": {rec.server.URL},
		"max_results":   {fmt.Sprint(maxResults)},
	})
	h.drain()

	calls := rec.eventsOf("match_found")
	if len(calls) == 0 || len(calls) > maxResults || len(calls) != len(rec.received()) {
		t.Fatalf("match_found calls: got %d of %d, want 1..%d", len(calls), len(rec.received()), maxResults)
	}
	for _, call := range calls {
		if call.Get("pattern") != "error" {
			t.Errorf("pattern: got %q, want error", call.Get("pattern"))
		}
	}
	assertRequestID(t, calls, requestID)
}

// The request URL wins over RX_HOOK_ON_COMPLETE_URL, the precedence the
// CLI applies to --hook-on-complete.
func TestTraceHooks_HTTP_RequestURLWinsOverTheEnvironment(t *testing.T) {
	h := newHookedTrace(t)
	fromEnv := newWebhookReceiver(t)
	fromRequest := newWebhookReceiver(t)
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", fromEnv.server.URL)

	requestID := h.mustTrace(t, url.Values{"hook_on_complete": {fromRequest.server.URL}})
	h.drain()

	if got := len(fromEnv.received()); got != 0 {
		t.Errorf("environment URL calls: got %d, want 0", got)
	}
	calls := fromRequest.received()
	if len(calls) != 1 {
		t.Fatalf("request URL calls: got %d, want 1", len(calls))
	}
	assertRequestID(t, calls, requestID)
}

// Without hook parameters the environment URL fires, with the request_id
// of the request that caused it.
func TestTraceHooks_HTTP_EnvironmentURLCarriesTheRequestID(t *testing.T) {
	h := newHookedTrace(t)
	fromEnv := newWebhookReceiver(t)
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", fromEnv.server.URL)

	requestID := h.mustTrace(t, nil)
	h.drain()

	calls := fromEnv.received()
	if len(calls) != 1 {
		t.Fatalf("environment URL calls: got %d, want 1", len(calls))
	}
	assertRequestID(t, calls, requestID)
}

// Two traces running at the same time through one dispatcher: each
// receiver gets exactly its own request's events.
func TestTraceHooks_HTTP_ConcurrentRequestsKeepTheirOwnEvents(t *testing.T) {
	h := newHookedTrace(t)
	receivers := map[string]*webhookReceiver{
		"request-a": newWebhookReceiver(t),
		"request-b": newWebhookReceiver(t),
	}

	// One goroutine per request; the WaitGroup lets the test continue
	// only once both HTTP responses are back.
	var wg sync.WaitGroup
	for requestID, rec := range receivers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.trace(t, url.Values{
				"request_id":       {requestID},
				"hook_on_file":     {rec.server.URL},
				"hook_on_complete": {rec.server.URL},
			})
		}()
	}
	wg.Wait()
	h.drain()

	for requestID, rec := range receivers {
		if got := len(rec.eventsOf("file_scanned")); got != 2 {
			t.Errorf("%s: file_scanned calls: got %d, want 2", requestID, got)
		}
		if got := len(rec.eventsOf("trace_complete")); got != 1 {
			t.Errorf("%s: trace_complete calls: got %d, want 1", requestID, got)
		}
		if got := len(rec.received()); got != 3 {
			t.Errorf("%s: calls: got %d, want 3", requestID, got)
		}
		assertRequestID(t, rec.received(), requestID)
	}
}

// The SSRF guard applies to a URL given on the request: a loopback
// target without the operator opt-in is a 400, and nothing is called.
func TestTraceHooks_HTTP_RequestURLStillPassesTheSSRFGuard(t *testing.T) {
	h := newHookedTrace(t)
	rec := newWebhookReceiver(t)
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "false")

	for _, param := range []string{"hook_on_file", "hook_on_complete"} {
		status, _ := h.trace(t, url.Values{param: {rec.server.URL}})
		if status != http.StatusBadRequest {
			t.Errorf("%s=loopback: status got %d, want 400", param, status)
		}
	}
	h.drain()

	if got := len(rec.received()); got != 0 {
		t.Errorf("hook calls: got %d, want 0", got)
	}
}

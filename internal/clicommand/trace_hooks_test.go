package clicommand

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// The --hook-on-* flags fire from the CLI, not only over HTTP. A script
// that runs `rx trace ERROR app.log --hook-on-complete=https://ci/notify`
// has to notify CI whichever backend it runs against, and the payload has
// to be the one rx-python sends.

// discardWriter swallows the command's stdout: these tests are about
// what reached the webhook, not what reached the terminal.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// asExit is errors.As specialised to *ExitError, which is what the CLI
// returns for every documented exit code.
func asExit(err error, target **ExitError) bool { return errors.As(err, target) }

// hookRecorder is a webhook target that remembers every request it saw.
type hookRecorder struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []url.Values
}

func newHookRecorder(t *testing.T) *hookRecorder {
	t.Helper()
	rec := &hookRecorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, r.URL.Query())
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (h *hookRecorder) received() []url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]url.Values(nil), h.calls...)
}

// hookFixture writes two searchable files and opts the process into
// loopback hook targets, the way an operator with a local collector does.
func hookFixture(t *testing.T) (dir string, files []string) {
	t.Helper()
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	t.Setenv("RX_HOOK_ON_FILE_URL", "")
	t.Setenv("RX_HOOK_ON_MATCH_URL", "")
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", "")
	t.Setenv("RX_DISABLE_CUSTOM_HOOKS", "")

	dir = t.TempDir()
	for _, name := range []string{"a.log", "b.log"} {
		path := filepath.Join(dir, name)
		body := "one error here\ntwo error here\nthree error here\nfour error here\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		files = append(files, path)
	}
	return dir, files
}

func TestTraceHooks_OnCompleteFiresOnce(t *testing.T) {
	dir, _ := hookFixture(t)
	rec := newHookRecorder(t)

	err := runTrace(discardWriter{}, traceParams{
		args:           []string{"error", dir},
		jsonOutput:     true,
		hookOnComplete: rec.server.URL,
	})
	if err != nil {
		t.Fatalf("runTrace: %v", err)
	}

	calls := rec.received()
	if len(calls) != 1 {
		t.Fatalf("hook calls: got %d, want 1", len(calls))
	}
	got := calls[0]
	if got.Get("event") != "trace_complete" {
		t.Errorf("event: got %q, want trace_complete", got.Get("event"))
	}
	for _, key := range []string{
		"request_id", "paths", "patterns", "total_files_scanned",
		"total_files_skipped", "total_matches", "total_time_ms",
	} {
		if got.Get(key) == "" && key != "total_files_skipped" {
			t.Errorf("payload has no %s: %v", key, got)
		}
	}
	if got.Get("total_matches") != "8" {
		t.Errorf("total_matches: got %q, want 8", got.Get("total_matches"))
	}
}

func TestTraceHooks_OnFileFiresPerFile(t *testing.T) {
	dir, _ := hookFixture(t)
	rec := newHookRecorder(t)

	err := runTrace(discardWriter{}, traceParams{
		args:       []string{"error", dir},
		jsonOutput: true,
		hookOnFile: rec.server.URL,
	})
	if err != nil {
		t.Fatalf("runTrace: %v", err)
	}

	calls := rec.received()
	if len(calls) != 2 {
		t.Fatalf("hook calls: got %d, want 2 (one per file)", len(calls))
	}
	var paths []string
	for _, call := range calls {
		if call.Get("event") != "file_scanned" {
			t.Errorf("event: got %q, want file_scanned", call.Get("event"))
		}
		for _, key := range []string{"file_path", "file_size_bytes", "scan_time_ms", "matches_count"} {
			if _, ok := call[key]; !ok {
				t.Errorf("payload has no %s: %v", key, call)
			}
		}
		paths = append(paths, filepath.Base(call.Get("file_path")))
	}
	sort.Strings(paths)
	if paths[0] != "a.log" || paths[1] != "b.log" {
		t.Errorf("file_path values: got %v, want [a.log b.log]", paths)
	}
}

func TestTraceHooks_OnMatchIsCappedByMaxResults(t *testing.T) {
	dir, _ := hookFixture(t)
	rec := newHookRecorder(t)

	err := runTrace(discardWriter{}, traceParams{
		args:        []string{"error", dir},
		jsonOutput:  true,
		maxResults:  3,
		hookOnMatch: rec.server.URL,
	})
	if err != nil {
		t.Fatalf("runTrace: %v", err)
	}

	calls := rec.received()
	if len(calls) == 0 || len(calls) > 3 {
		t.Fatalf("hook calls: got %d, want 1..3", len(calls))
	}
	for _, call := range calls {
		if call.Get("event") != "match_found" {
			t.Errorf("event: got %q, want match_found", call.Get("event"))
		}
		for _, key := range []string{"file_path", "pattern", "offset"} {
			if call.Get(key) == "" {
				t.Errorf("payload has no %s: %v", key, call)
			}
		}
	}
}

func TestTraceHooks_OnMatchWithoutMaxResultsIsAUsageError(t *testing.T) {
	dir, _ := hookFixture(t)
	rec := newHookRecorder(t)

	err := runTrace(discardWriter{}, traceParams{
		args:        []string{"error", dir},
		jsonOutput:  true,
		hookOnMatch: rec.server.URL,
	})
	if err == nil {
		t.Fatalf("runTrace: got nil error, want a usage error")
	}
	var exitErr *ExitError
	if !asExit(err, &exitErr) {
		t.Fatalf("error type: got %T, want *ExitError", err)
	}
	if exitErr.Code != ExitUsageError {
		t.Errorf("exit code: got %d, want %d", exitErr.Code, ExitUsageError)
	}
	if len(rec.received()) != 0 {
		t.Errorf("hooks fired despite the usage error: %v", rec.received())
	}
}

// RX_HOOK_ON_*_URL configures a hook without a flag, exactly as it does
// over HTTP, and RX_DISABLE_CUSTOM_HOOKS makes the flags inert.
func TestTraceHooks_EnvironmentConfiguresAndDisables(t *testing.T) {
	dir, _ := hookFixture(t)
	fromEnv := newHookRecorder(t)
	fromFlag := newHookRecorder(t)

	t.Setenv("RX_HOOK_ON_COMPLETE_URL", fromEnv.server.URL)
	t.Setenv("RX_DISABLE_CUSTOM_HOOKS", "true")

	err := runTrace(discardWriter{}, traceParams{
		args:           []string{"error", dir},
		jsonOutput:     true,
		hookOnComplete: fromFlag.server.URL,
	})
	if err != nil {
		t.Fatalf("runTrace: %v", err)
	}

	if len(fromEnv.received()) != 1 {
		t.Errorf("env hook calls: got %d, want 1", len(fromEnv.received()))
	}
	if len(fromFlag.received()) != 0 {
		t.Errorf("flag hook fired while RX_DISABLE_CUSTOM_HOOKS is set: %d calls",
			len(fromFlag.received()))
	}
}

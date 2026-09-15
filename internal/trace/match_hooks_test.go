package trace

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The match_found webhook carries the line number the response gives
// the same match: the line's number in the file when it is known, -1
// when it is not. It never carries a number counted from the start of a
// chunk, and it always carries the byte offset, from which the receiver
// can resolve an unknown line with `rx samples --offsets=…`.

// firedMatch is one OnMatch call as a recordingHookFirer saw it.
type firedMatch struct {
	path string
	info MatchInfo
}

// recordingHookFirer is a HookFirer that remembers every OnMatch call.
// The mutex makes it safe whichever goroutine the engine calls it from.
type recordingHookFirer struct {
	mu      sync.Mutex
	matches []firedMatch
}

func (r *recordingHookFirer) OnFile(context.Context, string, FileInfo) {}

func (r *recordingHookFirer) OnMatch(_ context.Context, path string, m MatchInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.matches = append(r.matches, firedMatch{path: path, info: m})
}

// fired returns a copy of the calls recorded so far.
func (r *recordingHookFirer) fired() []firedMatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]firedMatch(nil), r.matches...)
}

// writeLateNeedleFixture writes a file whose every line carries its own
// 1-based number, with the needle on every tenth line of the second
// half only, so every match lies past the first chunk. It returns the
// path and the offset of the first needle line.
func writeLateNeedleFixture(t *testing.T, name string, targetBytes int) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriterSize(f, 1<<20)
	pad := strings.Repeat("x", 200)
	written, firstNeedle := 0, int64(-1)
	for line := 1; written < targetBytes; line++ {
		kind := "filler"
		if written >= targetBytes/2 && line%10 == 0 {
			kind = "NEEDLE"
			if firstNeedle < 0 {
				firstNeedle = int64(written)
			}
		}
		n, err := fmt.Fprintf(w, "line %d %s %s\n", line, kind, pad)
		if err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		written += n
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush fixture: %v", err)
	}
	return path, firstNeedle
}

// assertHooksMatchResponse fails unless the OnMatch calls are exactly
// the response's matches, in order, each with the file path, pattern,
// offset and absolute line number the response gives it.
func assertHooksMatchResponse(t *testing.T, resp *rxtypes.TraceResponse, fired []firedMatch) {
	t.Helper()
	if len(fired) != len(resp.Matches) {
		t.Errorf("OnMatch calls: got %d, want one per response match (%d)", len(fired), len(resp.Matches))
	}
	byOffset := make(map[int64]firedMatch, len(fired))
	for _, call := range fired {
		byOffset[call.info.Offset] = call
	}
	for i, m := range resp.Matches {
		got, ok := byOffset[m.Offset]
		if !ok {
			t.Errorf("match %d (offset %d): no OnMatch call", i, m.Offset)
			continue
		}
		if i < len(fired) && fired[i].info.Offset != m.Offset {
			t.Errorf("call %d: offset %d, want the response's order (offset %d)", i, fired[i].info.Offset, m.Offset)
		}
		if got.path != resp.Files[m.File] {
			t.Errorf("match %d: path %q, want %q", i, got.path, resp.Files[m.File])
		}
		if got.info.Pattern != resp.Patterns[m.Pattern] {
			t.Errorf("match %d: pattern %q, want %q", i, got.info.Pattern, resp.Patterns[m.Pattern])
		}
		if got.info.Offset != m.Offset {
			t.Errorf("match %d: offset %d, want %d", i, got.info.Offset, m.Offset)
		}
		if got.info.LineNumber != int64(m.AbsoluteLineNumber) {
			t.Errorf("match %d (offset %d): hook line_number %d, response absolute_line_number %d",
				i, m.Offset, got.info.LineNumber, m.AbsoluteLineNumber)
		}
	}
}

// A capped trace of a chunked plain file above the large-file threshold,
// whose matches all lie past the first chunk: whatever the cap left
// unnumbered is -1 in the hook as in the response, and every number the
// hook does carry is the line's number in the file.
func TestTrace_CappedChunkedTraceMatchHookCarriesTheResponseLineNumber(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	path, firstNeedle := writeLateNeedleFixture(t, "late.log", 8<<20)

	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 || firstNeedle < tasks[0].EndOffset() {
		t.Fatalf("fixture: %d chunk(s), first needle at %d; the test needs it past the first chunk",
			len(tasks), firstNeedle)
	}

	rec := &recordingHookFirer{}
	limit := 20
	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"},
		Options{MaxResults: &limit, HookFirer: rec},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) == 0 {
		t.Fatal("no matches")
	}
	for _, m := range resp.Matches {
		if want := lineNumberFromText(t, *m.LineText); m.AbsoluteLineNumber != -1 && m.AbsoluteLineNumber != want {
			t.Errorf("offset %d: absolute_line_number %d, want %d or -1", m.Offset, m.AbsoluteLineNumber, want)
		}
	}
	assertHooksMatchResponse(t, resp, rec.fired())
}

// An uncapped trace numbers every match, and the hook carries that
// number — on the scan that writes the trace cache and on the cache hit
// that answers the same trace again.
func TestTrace_UncappedTraceMatchHookCarriesTheFileLineNumber(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	path, _ := writeChunkedFixture(t, "uncapped.log", 4<<20)

	for _, run := range []string{"scan", "cache hit"} {
		rec := &recordingHookFirer{}
		resp, err := New().RunWithOptions(
			context.Background(), []string{path}, []string{"NEEDLE"},
			Options{HookFirer: rec},
		)
		if err != nil {
			t.Fatalf("%s: RunWithOptions: %v", run, err)
		}
		if len(resp.Matches) == 0 {
			t.Fatalf("%s: no matches", run)
		}
		assertHooksMatchResponse(t, resp, rec.fired())
		for i, call := range rec.fired() {
			want := lineNumberFromText(t, *resp.Matches[i].LineText)
			if call.info.LineNumber != int64(want) {
				t.Fatalf("%s: offset %d: hook line_number %d, want %d",
					run, call.info.Offset, call.info.LineNumber, want)
			}
		}
	}
}

// A match whose line the scan could not count is sent as -1 with its
// offset, even when relative_line_number holds a number counted from
// the start of its chunk.
func TestFireMatchHooks_UnknownLineIsSentAsMinusOne(t *testing.T) {
	t.Parallel()
	chunkRelative := 17
	matches := []rxtypes.Match{
		{Pattern: "p1", File: "f1", Offset: 4_000_123, RelativeLineNumber: &chunkRelative, AbsoluteLineNumber: -1},
		{Pattern: "p1", File: "f1", Offset: 4_000_456, RelativeLineNumber: &chunkRelative, AbsoluteLineNumber: 9812},
	}
	rec := &recordingHookFirer{}
	fireMatchHooks(context.Background(), rec, matches,
		map[string]string{"f1": "/logs/app.log"}, map[string]string{"p1": "timeout"})

	want := []firedMatch{
		{path: "/logs/app.log", info: MatchInfo{Pattern: "timeout", Offset: 4_000_123, LineNumber: -1}},
		{path: "/logs/app.log", info: MatchInfo{Pattern: "timeout", Offset: 4_000_456, LineNumber: 9812}},
	}
	got := rec.fired()
	if len(got) != len(want) {
		t.Fatalf("calls: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

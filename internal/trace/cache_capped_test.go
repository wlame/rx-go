package trace

import (
	"io"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A capped trace answered from the trace cache rebuilds only the
// matches it can return. It used to rebuild every cached match, reading
// the file up to the last one, and then keep the first max_results.

// cappedTraceFromCache runs a capped trace that must be answered from
// the cache.
func cappedTraceFromCache(t *testing.T, path string, patterns []string, opts Options) *rxtypes.TraceResponse {
	t.Helper()
	scanned := false
	opts.afterScan = func(string) { scanned = true }
	resp := traceOnce(t, path, patterns, opts)
	if scanned {
		t.Fatal("the trace scanned the file instead of reading the cache")
	}
	return resp
}

// The cache hit of a capped trace reads the file only up to the line
// after the last match it returns, plus one read buffer.
func TestCacheHit_CappedTraceReadsOnlyToItsLastMatch(t *testing.T) {
	patterns := []string{"NEEDLE"}
	path, _, fresh := cacheFixture(t, patterns)
	offsets := fixtureLineOffsets(t, path)

	var read *atomic.Int64
	original := openForReconstruct
	openForReconstruct = func(p string) (io.ReadSeekCloser, error) {
		f, counter := counting.OpenCounting(t, p)
		read = counter
		return f, nil
	}
	t.Cleanup(func() { openForReconstruct = original })

	limit := 20
	resp := cappedTraceFromCache(t, path, patterns, Options{MaxResults: &limit})

	if len(resp.Matches) != limit {
		t.Fatalf("matches: got %d, want %d", len(resp.Matches), limit)
	}
	last := resp.Matches[limit-1].AbsoluteLineNumber
	budget := offsets[last+1] + reconstructBufferBytes
	if read == nil {
		t.Fatal("the cache hit opened no source")
	}
	if got := read.Load(); got > budget {
		t.Errorf("read %d bytes to return %d of %d cached matches, budget %d", got, limit, len(fresh.Matches), budget)
	}
}

// The capped answer from the cache is the uncapped answer from the
// cache cut to the cap: the same matches, numbers and context lines.
// Two patterns match every needle line, and the odd cap falls between
// the two matches of one line. Needles are 10 lines apart and the
// leading context is 12 lines, so the window of the last match kept
// holds lines that only the leading context of a match past the cap
// supplies.
func TestCacheHit_CappedTraceAnswersTheUncappedHitCutToTheCap(t *testing.T) {
	patterns := []string{"NEEDLE", "NEEDLE "}
	path, _, _ := cacheFixture(t, patterns)
	withContext := Options{ContextBefore: 12, ContextAfter: 1}
	full := cappedTraceFromCache(t, path, patterns, withContext)

	limit := 7
	capped := withContext
	capped.MaxResults = &limit
	resp := cappedTraceFromCache(t, path, patterns, capped)

	if len(full.Matches) < limit {
		t.Fatalf("fixture has %d matches; the test needs more than %d", len(full.Matches), limit)
	}
	if !reflect.DeepEqual(resp.Matches, full.Matches[:limit]) {
		t.Errorf("capped matches differ from the first %d of the uncapped hit:\n got %+v\nwant %+v",
			limit, resp.Matches, full.Matches[:limit])
	}
	for key, lines := range resp.ContextLines {
		if !reflect.DeepEqual(lines, full.ContextLines[key]) {
			t.Errorf("context of %s: got %+v, want %+v", key, lines, full.ContextLines[key])
		}
	}
	if len(resp.ContextLines) != limit {
		t.Errorf("context entries: got %d, want one per match (%d)", len(resp.ContextLines), limit)
	}
}

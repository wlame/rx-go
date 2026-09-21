package samples

import (
	"bytes"
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/timestamps"
)

// An answer of more lines than Request.MaxLines is refused with
// ErrTooManyLines naming the count reached, in every mode; one of
// exactly MaxLines is answered. A line that two keys share counts
// once per key, as the answer holds it.
func TestResolve_MaxLinesRefusesALargerAnswer(t *testing.T) {
	const maxLines = 1000
	path, lines, size := largeTimedLog(t, 3000)
	end := func(n int64) *int64 { return &n }
	shared := make([]string, 0, 501)
	for i := range 501 {
		// 501 spellings of lines 1-2: a start at or before line 1's
		// time, an end before line 3's.
		shared = append(shared, iso(lines[0].ms-int64(i/100))+".."+iso(lines[1].ms+int64(i%100)))
	}
	cases := []struct {
		name    string
		req     Request
		refused bool
	}{
		{"a range of exactly the limit", Request{Lines: []OffsetOrRange{{Start: 1, End: end(maxLines)}}}, false},
		{"a range one line longer", Request{Lines: []OffsetOrRange{{Start: 1, End: end(maxLines + 1)}}}, true},
		{"overlapping ranges", Request{Lines: []OffsetOrRange{{Start: 1, End: end(600)}, {Start: 2, End: end(600)}}}, true},
		{"singles with context", Request{Lines: singlesFrom(1, 200), BeforeContext: 3, AfterContext: 3}, true},
		{"a byte range over the file", Request{Offsets: []OffsetOrRange{{Start: 0, End: end(size - 1)}}}, true},
		{"an open time range", Request{Timestamps: []string{iso(lines[0].ms) + ".."}}, true},
		{"time queries that share a range", Request{Timestamps: shared}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Path, req.IndexLoader, req.MaxLines = path, NoIndex, maxLines
			resp, err := Resolve(t.Context(), req)
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := len(resp.Samples["1-1000"]); got != maxLines {
					t.Fatalf("%d lines", got)
				}
				return
			}
			if !errors.Is(err, ErrTooManyLines) || !strings.Contains(err.Error(), "1001") {
				t.Fatalf("err %v; want ErrTooManyLines at 1001 lines", err)
			}
		})
	}
}

// singlesFrom returns n single positions from line first on.
func singlesFrom(first, n int64) []OffsetOrRange {
	out := make([]OffsetOrRange, 0, n)
	for line := first; line < first+n; line++ {
		out = append(out, OffsetOrRange{Start: line})
	}
	return out
}

// The lines are counted before they are held, so a request that asks
// for far more than the limit stops reading once it passes it: a
// hundred time ranges open to the end of a 5 MB file read about the
// limit's worth of lines, not the file a hundred times over.
func TestBudget_MaxLinesStopsTheReadAtTheLimit(t *testing.T) {
	const maxLines = 1000
	path, lines, size := largeTimedLog(t, 35_000)
	var values []string
	for i := range 100 {
		values = append(values, iso(lines[i].ms)+"..")
	}
	counter := withCountingOpen(t)
	_, err := Resolve(t.Context(), Request{Path: path, Timestamps: values, IndexLoader: NoIndex, MaxLines: maxLines})
	if !errors.Is(err, ErrTooManyLines) {
		t.Fatalf("err %v; want ErrTooManyLines", err)
	}
	// The detection head, the search for the hundred starts, and the
	// lines up to the limit, each with a read buffer.
	if read, budget := counter.Load(), int64(timestamps.SampleBytes)+2*maxLines*150+128*1024; read > budget {
		t.Errorf("read %d bytes of %d; budget %d", read, size, budget)
	}
}

// longLinesLog writes a log of n timestamped lines of about width bytes,
// then one line of long bytes and one short last line, and returns its
// path and text.
func longLinesLog(t *testing.T, n, width, long int) (string, []byte) {
	t.Helper()
	var b strings.Builder
	for i := range n {
		b.WriteString(iso(timeBase+int64(i)*1000) + " " + strings.Repeat("x", width) + "\n")
	}
	b.WriteString(iso(timeBase+int64(n)*1000) + " " + strings.Repeat("y", long) + "\n")
	b.WriteString(iso(timeBase+int64(n+1)*1000) + " last\n")
	path := filepath.Join(t.TempDir(), "long.log")
	writeFile(t, path, []byte(b.String()))
	return path, []byte(b.String())
}

// An answer of more bytes of line text than Request.MaxBytes is refused
// with ErrTooManyBytes, in every mode, and one within it is answered.
func TestResolve_MaxBytesRefusesALargerAnswer(t *testing.T) {
	const maxBytes = 1 << 20
	path, text := longLinesLog(t, 30, 100_000, 10)
	end := func(n int64) *int64 { return &n }
	cases := []struct {
		name    string
		req     Request
		refused bool
	}{
		{"ten lines of 100 KB", Request{Lines: []OffsetOrRange{{Start: 1, End: end(10)}}}, false},
		{"eleven lines of 100 KB", Request{Lines: []OffsetOrRange{{Start: 1, End: end(11)}}}, true},
		{"a byte range over the file", Request{Offsets: []OffsetOrRange{{Start: 0, End: end(int64(len(text)) - 1)}}}, true},
		{"an open time range", Request{Timestamps: []string{iso(timeBase) + ".."}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Path, req.IndexLoader, req.MaxBytes = path, NoIndex, maxBytes
			_, err := Resolve(t.Context(), req)
			if tc.refused != errors.Is(err, ErrTooManyBytes) || (!tc.refused && err != nil) {
				t.Fatalf("err %v; want refused %v", err, tc.refused)
			}
		})
	}
}

// A line longer than MaxBytes is never held whole: asked for, it is
// refused after at most the limit's worth of it is in memory; passed on
// the way to another line, it is not kept at all.
func TestResolve_MaxBytesDoesNotHoldALongLine(t *testing.T) {
	const maxBytes, long = 1 << 20, 32 << 20
	path, text := longLinesLog(t, 3, 100, long)
	lastLine := int64(bytes.LastIndex(text[:len(text)-1], []byte("\n")) + 1)
	cases := []struct {
		name    string
		req     Request
		refused bool
	}{
		{"the long line", Request{Lines: []OffsetOrRange{{Start: 4}}}, true},
		{"the line after it, by line", Request{Lines: []OffsetOrRange{{Start: 5}}}, false},
		{"the line after it, by offset", Request{Offsets: []OffsetOrRange{{Start: lastLine}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Path, req.IndexLoader, req.MaxBytes = path, NoIndex, maxBytes
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := Resolve(t.Context(), req)
			runtime.ReadMemStats(&after)
			if tc.refused != errors.Is(err, ErrTooManyBytes) || (!tc.refused && err != nil) {
				t.Fatalf("err %v; want refused %v", err, tc.refused)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > long/4 {
				t.Errorf("allocated %d bytes around a %d-byte line; the limit is %d", allocated, long, maxBytes)
			}
		})
	}
}

// The lines the offsets pass keeps for the context of a window that may
// start later hold at most the byte limit of text: older lines' text is
// let go first, and a window that would need it passes the limit.
func TestLineRing_KeepsAtMostTheByteLimitOfText(t *testing.T) {
	const limit = 1 << 20
	ring := newLineRing(50, limit)
	line := strings.Repeat("x", 900_000)
	for i := range 50 {
		ring.push(ringLine{text: line, start: int64(i) * 900_001, textBytes: 900_000})
	}
	kept := 0
	for _, l := range ring.lines() {
		kept += len(l.text)
	}
	if kept > limit {
		t.Errorf("the ring keeps %d bytes of text; the limit is %d", kept, limit)
	}
	if got := len(ring.lines()); got != 50 {
		t.Errorf("%d lines remembered, want 50 (their sizes still count)", got)
	}
}

// A window whose leading context would pass the byte limit is refused;
// one whose context fits is answered whole.
func TestResolve_MaxBytesCountsTheLeadingContext(t *testing.T) {
	const maxBytes = 1 << 20
	path, text := longLinesLog(t, 50, 300_000, 10)
	lastLine := int64(bytes.LastIndex(text[:len(text)-1], []byte("\n")) + 1)
	for _, tc := range []struct {
		before  int
		refused bool
	}{{3, false}, {50, true}} {
		resp, err := Resolve(t.Context(), Request{Path: path, Offsets: []OffsetOrRange{{Start: lastLine}}, BeforeContext: tc.before, IndexLoader: NoIndex, MaxBytes: maxBytes})
		if tc.refused != errors.Is(err, ErrTooManyBytes) || (!tc.refused && err != nil) {
			t.Fatalf("before %d: err %v; want refused %v", tc.before, err, tc.refused)
		}
		if !tc.refused {
			if got := resp.Samples[strconv.FormatInt(lastLine, 10)]; len(got) != 4 || len(got[0]) < 300_000 {
				t.Errorf("before %d: sample of %d lines", tc.before, len(got))
			}
		}
	}
}

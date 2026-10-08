package logchain

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// traceback is n lines without a timestamp of their own, each width
// bytes long with its line break, as a stack trace continues a logged
// line.
func traceback(n, width int, label string) []byte {
	var buf bytes.Buffer
	for i := 1; i <= n; i++ {
		head := fmt.Sprintf("    at %s.frame%d(", label, i)
		buf.WriteString(head)
		buf.WriteString(strings.Repeat("x", width-len(head)-2))
		buf.WriteString(")\n")
	}
	return buf.Bytes()
}

// stamped is one line with a timestamp of its own at `at`, width bytes
// long with its line break (none when withBreak is false).
func stamped(at time.Time, width int, label string, withBreak bool) []byte {
	head := at.Format("2006-01-02 15:04:05.000") + " " + label + " "
	line := head + strings.Repeat("y", width-len(head)-1)
	if withBreak {
		return []byte(line + "\n")
	}
	return []byte(line + "y")
}

// partReads counts the reads a chain answer makes of each part: through
// the reader, and through the read back's own reads of an earlier part
// (its last byte, one of its lines).
type partReads struct {
	mu     sync.Mutex
	reader map[string]int
	ends   map[string]int
	lines  map[string]int
}

// countPartReads makes the read back's reads count into a partReads for the
// rest of the test, and returns it with a reader that counts too.
func countPartReads(t *testing.T) (*partReads, PartReader) {
	t.Helper()
	counts := &partReads{reader: map[string]int{}, ends: map[string]int{}, lines: map[string]int{}}
	ends, line := partEndsWithLineBreak, resolveEarlierLine
	t.Cleanup(func() { partEndsWithLineBreak, resolveEarlierLine = ends, line })
	partEndsWithLineBreak = func(ctx context.Context, req samples.Request, textLen int64) (bool, error) {
		counts.add(counts.ends, req.Path)
		return ends(ctx, req, textLen)
	}
	resolveEarlierLine = func(ctx context.Context, req samples.Request) (*rxtypes.SamplesResponse, error) {
		counts.add(counts.lines, req.Path)
		return line(ctx, req)
	}
	reader := func(ctx context.Context, part Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
		counts.add(counts.reader, part.Path)
		return samples.Resolve(ctx, req)
	}
	return counts, reader
}

func (p *partReads) add(m map[string]int, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m[path[strings.LastIndexByte(path, '/')+1:]]++
}

// readsOf is how many times name was read, by every route.
func (p *partReads) readsOf(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reader[name] + p.ends[name] + p.lines[name]
}

// tracebackChain is a chain whose second part and active file start with
// lines that have no timestamp of their own: a stack trace the first
// part's last line began. The first part is gzip.
//
//	app.log.2.gz  10 timestamped lines              lines 1-10
//	app.log.1     3 traceback lines, 4 timestamped  lines 11-17
//	app.log       2 traceback lines, 3 timestamped  lines 18-22
func tracebackChain(t *testing.T) builtChain {
	t.Helper()
	first := timedLines(chainBase, time.Second, 1, 10, "app.log.2.gz")
	second := append(traceback(3, 60, "second"), timedLines(chainBase.Add(time.Minute), time.Second, 14, 4, "app.log.1")...)
	active := append(traceback(2, 60, "active"), timedLines(chainBase.Add(2*time.Minute), time.Second, 20, 3, "app.log")...)
	return buildChain(t, "app.log", []chainFile{
		{name: "app.log.2.gz", text: first, codec: compressedcopy.Gzip},
		{name: "app.log.1", text: second},
		{name: "app.log", text: active},
	})
}

// Lines without a timestamp of their own at the start of a part get the
// line_timestamps the concatenation gives them: the read back continues
// into the part before, cold and indexed.
func TestSamples_ReadBackCrossesThePartEdge(t *testing.T) {
	c := tracebackChain(t)
	requireLikeConcatenation(t, c,
		SamplesRequest{Lines: lines(t, "11,12,13,18,19")},
		SamplesRequest{Lines: lines(t, "11-15,18-22,9-12")},
		SamplesRequest{Lines: lines(t, "12"), BeforeContext: 0, AfterContext: 5},
		SamplesRequest{Part: "", Lines: lines(t, "-5")},
	)
}

// The read back answers from the earlier part's index: a gzip part is
// not read at all when the window lies in the next part, cold (its
// index built in memory by the scan) and indexed.
func TestSamples_ReadBackReadsNoEarlierPartWhenItsIndexAnswers(t *testing.T) {
	c := tracebackChain(t)
	req := SamplesRequest{Lines: lines(t, "11-13"), IndexLoader: samples.StoredIndex}
	for _, scan := range []bool{true, false} {
		if !scan {
			storeIndexes(t, c.dir, c.order...)
		}
		counts, reader := countPartReads(t)
		d := describe(t, c.dir, c.name, Options{Scan: scan})
		resp, err := Samples(context.Background(), d, req, reader)
		if err != nil {
			t.Fatalf("scan %v: %v", scan, err)
		}
		_, stamps := flatten(resp.Samples["11-13"])
		if stamps[0] == nil {
			t.Fatalf("scan %v: the first traceback line has no timestamp", scan)
		}
		if n := counts.readsOf("app.log.2.gz"); n != 0 {
			t.Fatalf("scan %v: app.log.2.gz read %d times (reader %v, ends %v, lines %v)", scan, n,
				counts.reader, counts.ends, counts.lines)
		}
		if n := counts.readsOf("app.log.1"); n != 1 {
			t.Fatalf("scan %v: app.log.1 read %d times, want once", scan, n)
		}
	}
}

// The read back passes over an empty part to the part before it, and
// finds a line in a part shorter than its distance.
func TestSamples_ReadBackPassesAnEmptyPartAndAShortOne(t *testing.T) {
	short := timedLines(chainBase, time.Second, 1, 2, "app.log.3")
	after := append(traceback(3, 50, "after"), timedLines(chainBase.Add(time.Minute), time.Second, 6, 10, "app.log.1.gz")...)
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.3", text: short},
		{name: "app.log.2", text: nil},
		{name: "app.log.1.gz", text: after, codec: compressedcopy.Gzip},
		{name: "app.log", text: timedLines(chainBase.Add(2*time.Minute), time.Second, 16, 2, "app.log")},
	})
	requireLikeConcatenation(t, c,
		SamplesRequest{Lines: lines(t, "3-6")},
		SamplesRequest{Lines: lines(t, "4"), BeforeContext: 2, AfterContext: 2},
	)
}

// A distance past RX_TIMESTAMP_LOOKBACK_KB carries nothing across the
// edge, as in the concatenation: the earlier part's last timestamped
// line is too far back.
func TestSamples_ReadBackStopsAtItsDistance(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "1")
	first := append(timedLines(chainBase, time.Second, 1, 3, "app.log.1"), traceback(8, 100, "tail")...)
	active := append(traceback(4, 100, "active"), timedLines(chainBase.Add(time.Minute), time.Second, 16, 10, "app.log")...)
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1", text: first},
		{name: "app.log", text: active},
	})
	requireLikeConcatenation(t, c, SamplesRequest{Lines: lines(t, "11-15")})
	// Line 3 is the last timestamped line; lines 12 and 13 start 852
	// and 952 bytes after it, lines 14 and 15 1052 and 1152.
	resp := chainSamples(t, c, SamplesRequest{Lines: lines(t, "12-15")}, true)
	if _, stamps := flatten(resp.Samples["12-15"]); stamps[0] == nil || stamps[1] == nil || stamps[2] != nil || stamps[3] != nil {
		t.Fatalf("lines 852, 952, 1052 and 1152 bytes after the last timestamp got %s", stampsText(stamps, 4))
	}
}

// When a line's distance from the earlier part's last timestamp equals
// RX_TIMESTAMP_LOOKBACK_KB exactly when that part ends with a line
// break, the part's last byte decides: the chain adds a line break after
// a part without one, which puts the line one byte too far. Both cases,
// for a plain and a gzip part, equal the concatenation, and the last
// byte is read only for such a line.
func TestSamples_ReadBackAtExactlyItsDistance(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "1")
	for _, codec := range []string{"", compressedcopy.Gzip} {
		for _, withBreak := range []bool{true, false} {
			t.Run(fmt.Sprintf("codec %q, line break %v", codec, withBreak), func(t *testing.T) {
				name := "app.log.1"
				if codec != "" {
					name = "app.log.1.gz"
				}
				// The earlier part's last line, 100 bytes from its start
				// to the part's end; the active file's second line starts
				// 924 bytes into it, so 1024 bytes after that line start
				// when the part ends with a line break, 1025 when not.
				first := append(timedLines(chainBase, time.Second, 1, 3, name), stamped(chainBase.Add(5*time.Second), 100, "last", withBreak)...)
				active := append(traceback(1, 924, "near"), traceback(1, 50, "edge")...)
				active = append(active, timedLines(chainBase.Add(time.Minute), time.Second, 7, 2, "app.log")...)
				c := buildChain(t, "app.log", []chainFile{
					{name: name, text: first, codec: codec},
					{name: "app.log", text: active},
				})
				counts, reader := countPartReads(t)
				d := describe(t, c.dir, c.name, Options{Scan: true})
				resp, err := Samples(context.Background(), d, SamplesRequest{Lines: lines(t, "5-6"), IndexLoader: samples.StoredIndex}, reader)
				if err != nil {
					t.Fatalf("samples: %v", err)
				}
				requireAnswerLike(t, "exact distance", resp, concatSamples(t, c, SamplesRequest{Lines: lines(t, "5-6")}), false)
				if counts.ends[name] != 1 {
					t.Fatalf("the last byte of %s read %d times, want once", name, counts.ends[name])
				}
				_, stamps := flatten(resp.Samples["5-6"])
				if (stamps[1] != nil) != withBreak {
					t.Fatalf("the line at exactly the distance: %s", stampsText(stamps, 2))
				}
				counts2, reader2 := countPartReads(t)
				if _, err := Samples(context.Background(), d, SamplesRequest{Lines: lines(t, "5"), IndexLoader: samples.StoredIndex}, reader2); err != nil {
					t.Fatalf("samples: %v", err)
				}
				if counts2.ends[name] != 0 {
					t.Fatalf("a line nearer than the distance read the last byte of %s", name)
				}
			})
		}
	}
}

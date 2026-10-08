package logchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
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
// the reader, and through the read back's own read of an earlier part's
// last byte, with the read limit each of those was given and how many
// of them the limit refused (samples.ErrReadLimit: nothing read).
type partReads struct {
	mu        sync.Mutex
	reader    map[string]int
	ends      map[string]int
	endLimits map[string][]int64
	refused   map[string]int
}

// countPartReads makes the read back's reads count into a partReads for the
// rest of the test, and returns it with a reader that counts too.
func countPartReads(t *testing.T) (*partReads, PartReader) {
	t.Helper()
	counts := &partReads{reader: map[string]int{}, ends: map[string]int{}, endLimits: map[string][]int64{}, refused: map[string]int{}}
	ends := partEndsWithLineBreak
	t.Cleanup(func() { partEndsWithLineBreak = ends })
	partEndsWithLineBreak = func(ctx context.Context, req samples.Request, textLen, readLimit int64) (bool, error) {
		name := counts.add(counts.ends, req.Path)
		got, err := ends(ctx, req, textLen, readLimit)
		counts.mu.Lock()
		defer counts.mu.Unlock()
		counts.endLimits[name] = append(counts.endLimits[name], readLimit)
		if errors.Is(err, samples.ErrReadLimit) {
			counts.refused[name]++
		}
		return got, err
	}
	reader := func(ctx context.Context, part Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
		counts.add(counts.reader, part.Path)
		return samples.Resolve(ctx, req)
	}
	return counts, reader
}

// add counts one read of the file at path under its name, and returns
// the name.
func (p *partReads) add(m map[string]int, path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := path[strings.LastIndexByte(path, '/')+1:]
	m[name]++
	return name
}

// readsOf is how many times name was read, by every route.
func (p *partReads) readsOf(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reader[name] + p.ends[name]
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
			t.Fatalf("scan %v: app.log.2.gz read %d times (reader %v, ends %v)", scan, n, counts.reader, counts.ends)
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

// SECURITY: the last byte of an earlier part is read within what is left
// of the request's byte limit. At exactly the read back's distance a
// line's timestamp depends on that byte. A plain part gives it in a
// read of one byte. A gzip part whose text is longer than what is left
// is not read: the line carries no timestamp (null, not known) rather
// than one the part may not give it, and the line before it still
// carries its value. With room for the gzip part's text the answer
// equals the concatenation's, as without a limit. Cold and indexed.
func TestSamples_ReadBackReadsAPartsEndWithinTheByteLimit(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "1")
	const tight = 4096
	for _, codec := range []string{"", compressedcopy.Gzip} {
		name := "app.log.1"
		if codec != "" {
			name = "app.log.1.gz"
		}
		// 100 lines before the last make the part's text about 5.6 KiB,
		// more than the tight limit, while the answer's two lines hold
		// under 1 KiB. The last line is 100 bytes with its line break, and
		// the active file's second line (103) starts 924 bytes into it:
		// 1024 bytes after the last line's start.
		first := append(timedLines(chainBase, time.Second, 1, 100, name), stamped(chainBase.Add(200*time.Second), 100, "last", true)...)
		active := append(traceback(1, 924, "near"), traceback(1, 50, "edge")...)
		active = append(active, timedLines(chainBase.Add(time.Hour), time.Second, 104, 2, "app.log")...)
		c := buildChain(t, "app.log", []chainFile{
			{name: name, text: first, codec: codec},
			{name: "app.log", text: active},
		})
		base := SamplesRequest{Lines: lines(t, "102-103"), IndexLoader: samples.StoredIndex}
		want := concatSamples(t, c, base)
		wantStamps := want.LineTimestamps["102-103"]
		if wantStamps[0] == nil || wantStamps[1] == nil {
			t.Fatalf("the concatenation gives %s, want two values", stampsText(wantStamps, 2))
		}
		for _, scan := range []bool{true, false} {
			if !scan {
				storeIndexes(t, c.dir, c.order...)
			}
			d := describe(t, c.dir, c.name, Options{Scan: scan})
			for _, limit := range []int64{tight, 1 << 20} {
				label := fmt.Sprintf("%s, scan %v, limit %d", name, scan, limit)
				counts, reader := countPartReads(t)
				req := base
				req.MaxBytes = limit
				resp, err := Samples(context.Background(), d, req, reader)
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				got, stamps := flatten(resp.Samples["102-103"])
				if !slices.Equal(got, want.Samples["102-103"]) {
					t.Fatalf("%s: lines %q", label, got)
				}
				if counts.ends[name] != 1 || counts.endLimits[name][0] > limit {
					t.Fatalf("%s: the last byte read %d times with limits %v", label, counts.ends[name], counts.endLimits[name])
				}
				refused := codec != "" && limit == tight
				if refused != (counts.refused[name] == 1) {
					t.Fatalf("%s: %d reads refused by the limit", label, counts.refused[name])
				}
				wantText := stampsText(wantStamps, 2)
				if refused {
					wantText = stampsText([]*int64{wantStamps[0], nil}, 2)
				}
				if got := stampsText(stamps, 2); got != wantText {
					t.Fatalf("%s: line_timestamps %s, want %s", label, got, wantText)
				}
			}
		}
	}
}

// Under file_tz, the index of an earlier part whose written zone offset
// changes more often than the index records (zone_offsets null) does not
// give the instant of the part's last timestamped line. The read back
// across the edge does not read the part for it: the lines at the start
// of the next part that would carry it get none (null, not known), cold
// and indexed. Their text, and the timestamp of every line with one of
// its own, equal the concatenation's.
func TestSamples_ReadBackDoesNotReadAPartForAnInstantItsIndexLacks(t *testing.T) {
	t.Setenv(config.ChainOverlapSecondsSetting.Name, "86400")
	const layout = "2006-01-02T15:04:05.000"
	n := index.MaxZoneOffsets + 76
	var first bytes.Buffer
	for i := 0; i < n; i++ {
		offset := "+01:00"
		if i%2 == 1 {
			offset = "+02:00"
		}
		fmt.Fprintf(&first, "%s%s LINE %d part=app.log.1.gz local=%d\n", chainBase.Add(time.Duration(i)*time.Second).Format(layout), offset, i+1, i+1)
	}
	var stampedTail bytes.Buffer
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&stampedTail, "%s+01:00 LINE %d part=app.log local=%d\n", chainBase.Add(2*time.Hour+time.Duration(i)*time.Second).Format(layout), n+4+i, i+4)
	}
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1.gz", text: first.Bytes(), codec: compressedcopy.Gzip},
		{name: "app.log", text: append(traceback(3, 60, "active"), stampedTail.Bytes()...)},
	})
	utc, err := config.ParseZone("UTC")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	c.zone = utc
	// The active part's three traceback lines and its first line with a
	// timestamp of its own: no line of the earlier part.
	key := fmt.Sprintf("%d-%d", n+1, n+4)
	req := SamplesRequest{Lines: lines(t, key), IndexLoader: samples.StoredIndex}
	want := concatSamples(t, c, req)
	wantStamps := want.LineTimestamps[key]
	for _, scan := range []bool{true, false} {
		if !scan {
			storeIndexes(t, c.dir, c.order...)
		}
		d := describe(t, c.dir, c.name, Options{Scan: scan, FileZone: utc})
		if d.Response.State != rxtypes.ChainStateReady || d.facts[d.Order[0]].lastMs != nil {
			t.Fatalf("scan %v: want a ready chain whose first part's index gives no last instant: %s", scan, jsonOf(t, d.Response))
		}
		counts, reader := countPartReads(t)
		resp, err := Samples(context.Background(), d, req, reader)
		if err != nil {
			t.Fatalf("scan %v: %v", scan, err)
		}
		if reads := counts.readsOf("app.log.1.gz"); reads != 0 {
			t.Fatalf("scan %v: app.log.1.gz read %d times (reader %v, ends %v)", scan, reads, counts.reader, counts.ends)
		}
		got, stamps := flatten(resp.Samples[key])
		if !slices.Equal(got, want.Samples[key]) {
			t.Fatalf("scan %v: lines %q", scan, got)
		}
		// The concatenation gives the traceback the instant of line n, the
		// earlier part's last; the chain gives it none. Line n+4 has a
		// timestamp of its own.
		expect := []*int64{nil, nil, nil, wantStamps[3]}
		if wantStamps[0] == nil || stampsText(stamps, 4) != stampsText(expect, 4) {
			t.Fatalf("scan %v: line_timestamps %s, the concatenation %s", scan, stampsText(stamps, 4), stampsText(wantStamps, 4))
		}
	}
}

// SECURITY: the reads of earlier parts' last bytes share what is left of
// the request's byte limit. Two gzip parts each about 5.6 KiB of text
// meet a line at exactly the read back's distance in the part after
// them. Under a limit of 8 KiB the first fits and is read; the second
// does not fit in what the first left, and its line gets no timestamp.
// With room for both, both lines equal the concatenation's.
func TestSamples_ReadBackEdgeReadsShareTheByteLimit(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "1")
	// Each part after the first starts with a line 924 bytes long and a
	// line that starts 1024 bytes after the previous part's last line.
	opening := func(label string) []byte {
		return append(traceback(1, 924, label+"-near"), traceback(1, 50, label+"-edge")...)
	}
	first := append(timedLines(chainBase, time.Second, 1, 100, "app.log.2.gz"), stamped(chainBase.Add(200*time.Second), 100, "last", true)...)
	second := append(opening("second"), timedLines(chainBase.Add(time.Hour), time.Second, 104, 100, "app.log.1.gz")...)
	second = append(second, stamped(chainBase.Add(time.Hour+200*time.Second), 100, "last", true)...)
	active := append(opening("active"), timedLines(chainBase.Add(2*time.Hour), time.Second, 207, 2, "app.log")...)
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.2.gz", text: first, codec: compressedcopy.Gzip},
		{name: "app.log.1.gz", text: second, codec: compressedcopy.Gzip},
		{name: "app.log", text: active},
	})
	base := SamplesRequest{Lines: lines(t, "102-103,205-206"), IndexLoader: samples.StoredIndex}
	want := concatSamples(t, c, base)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	for _, tc := range []struct {
		limit         int64
		secondRefused bool
	}{{8 << 10, true}, {1 << 20, false}} {
		counts, reader := countPartReads(t)
		req := base
		req.MaxBytes = tc.limit
		resp, err := Samples(context.Background(), d, req, reader)
		if err != nil {
			t.Fatalf("limit %d: %v", tc.limit, err)
		}
		if counts.refused["app.log.2.gz"] != 0 || (counts.refused["app.log.1.gz"] == 1) != tc.secondRefused {
			t.Fatalf("limit %d: refused %v, limits given %v", tc.limit, counts.refused, counts.endLimits)
		}
		for _, key := range []string{"102-103", "205-206"} {
			wantStamps := want.LineTimestamps[key]
			if key == "205-206" && tc.secondRefused {
				wantStamps = []*int64{wantStamps[0], nil}
			}
			if _, stamps := flatten(resp.Samples[key]); stampsText(stamps, 2) != stampsText(wantStamps, 2) {
				t.Fatalf("limit %d, key %s: line_timestamps %s, want %s", tc.limit, key, stampsText(stamps, 2), stampsText(wantStamps, 2))
			}
		}
	}
}

package logchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// linesAt is a part's text of one line per time in at, each
// `<time> LINE <global> part=<name> local=<n>`.
func linesAt(firstGlobal int, part string, at ...time.Time) []byte {
	var buf bytes.Buffer
	for i, t := range at {
		fmt.Fprintf(&buf, "%s LINE %d part=%s local=%d\n", t.Format("2006-01-02 15:04:05.000"), firstGlobal+i, part, i+1)
	}
	return buf.Bytes()
}

// timeChain is a chain with an hour's gap between its first two parts
// and a part whose last line is earlier than its highest:
//
//	app.log.3     10:00:00 .. 10:00:59, one a second      lines 1-60
//	app.log.2.gz  11:00:00, 11:00:30, 11:00:10, 11:00:20  lines 61-64
//	app.log.1     11:05:00 .. 11:05:09                    lines 65-74
//	app.log       11:10:00 .. 11:10:04                    lines 75-79
func timeChain(t *testing.T) builtChain {
	t.Helper()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	hm := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	return buildChain(t, "app.log", []chainFile{
		{name: "app.log.3", text: timedLines(hm(10, 0, 0), time.Second, 1, 60, "app.log.3")},
		{name: "app.log.2.gz", text: linesAt(61, "app.log.2.gz", hm(11, 0, 0), hm(11, 0, 30), hm(11, 0, 10), hm(11, 0, 20)), codec: compressedcopy.Gzip},
		{name: "app.log.1", text: timedLines(hm(11, 5, 0), time.Second, 65, 10, "app.log.1")},
		{name: "app.log", text: timedLines(hm(11, 10, 0), time.Second, 75, 5, "app.log")},
	})
}

// Time queries find the line the concatenation finds: T inside a part,
// T in the gap between two parts (the first line of the next), T before
// the chain (line 1) and after it (none), T equal to a part's first
// line, T that only a line before a backward step reaches (the part's
// highest time, not its last, says where), a range across three parts,
// open ranges, a range whose end comes before its start, and a time of
// day on a chain of one day, cold and indexed.
func TestSamples_TimeQueries(t *testing.T) {
	c := timeChain(t)
	requireLikeConcatenation(t, c,
		SamplesRequest{Timestamps: []string{
			"2026-10-01 10:00:30.500", "2026-10-01 10:30", "2026-10-01 09:00", "2026-10-01 12:00",
			"2026-10-01 11:05:00", "2026-10-01 11:00:25", "10:00:30",
		}, BeforeContext: 2, AfterContext: 2},
		SamplesRequest{Timestamps: []string{
			"2026-10-01 10:00:50..2026-10-01 11:05:02", "2026-10-01 11:05:07..", "..2026-10-01 10:00:05",
			"2026-10-01 11:00:25..2026-10-01 11:00:12", "2026-10-01 11:10:03..2026-10-01 13:00",
		}},
	)
}

// A time of day without a date takes its date from the chain's first
// and last timestamps as one file takes it from its own: a chain of two
// days refuses it, as the concatenation does.
func TestSamples_TimeOfDayOnAChainOfTwoDays(t *testing.T) {
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1", text: timedLines(chainBase, time.Hour, 1, 3, "app.log.1")},
		{name: "app.log", text: timedLines(chainBase.Add(26*time.Hour), time.Hour, 4, 3, "app.log")},
	})
	d := describe(t, c.dir, c.name, Options{Scan: true})
	_, err := Samples(context.Background(), d, SamplesRequest{Timestamps: []string{"01:30"}, IndexLoader: samples.StoredIndex}, resolveReader)
	if !errors.Is(err, timestamps.ErrInvalidQuery) {
		t.Fatalf("a time of day on two days: %v, want an invalid query", err)
	}
	_, single := samples.Resolve(context.Background(), samples.Request{Path: c.concat, Timestamps: []string{"01:30"}, IndexLoader: samples.NoIndex})
	if !errors.Is(single, timestamps.ErrInvalidQuery) {
		t.Fatalf("the concatenation answers %v", single)
	}
}

// SECURITY: the time bounds of a request are searched in one forward
// sweep over the parts, so each part is read at most once for them and
// once for the answer's lines. Under file_tz, 100 parts whose lines
// write two zone offsets each have a highest time known only as an
// upper bound, 60 seconds above their lines; 100 times that such bounds
// reach and no line of those parts does move on from part to part in the
// same sweep, to the active part, rather than reread the parts they
// passed. The answers equal the concatenation's.
func TestSamples_TimeBoundsReadEachPartOnce(t *testing.T) {
	const parts = 100
	const layout = "2006-01-02T15:04:05.000"
	var files []chainFile
	for j := 0; j < parts; j++ {
		at := chainBase.Add(time.Duration(2*j) * time.Millisecond)
		name := fmt.Sprintf("app.log.%d", parts-j)
		text := fmt.Sprintf("%s+00:01 LINE %d part=%s local=1\n%s+00:00 LINE %d part=%s local=2\n",
			at.Format(layout), 2*j+1, name, at.Add(time.Millisecond).Format(layout), 2*j+2, name)
		files = append(files, chainFile{name: name, text: []byte(text)})
	}
	files = append(files, chainFile{name: "app.log", text: []byte(chainBase.Add(time.Hour).Format(layout) + "+00:00 LINE 201 part=app.log local=1\n")})
	c := buildChain(t, "app.log", files)
	utc, err := config.ParseZone("UTC")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	c.zone = utc
	storeIndexes(t, c.dir, c.order...)
	d := describe(t, c.dir, c.name, Options{FileZone: utc})
	if d.Response.State != rxtypes.ChainStateReady || !d.Response.Parts[0].MaxIsBound {
		t.Fatalf("want a ready chain whose parts' highest times are bounds: %s", jsonOf(t, d.Response.Parts[0]))
	}
	// Time i is 1 ms after part i's last line plus a minute: the bound of
	// part i and of every later part reaches it, and no frozen line does.
	var queries []string
	for i := 0; i < parts; i++ {
		queries = append(queries, chainBase.Add(time.Duration(2*i+1)*time.Millisecond+time.Minute).Format(layout))
	}
	reads := map[string]int{}
	reader := func(ctx context.Context, p Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
		reads[p.Name]++
		return samples.Resolve(ctx, req)
	}
	req := SamplesRequest{Timestamps: queries, IndexLoader: samples.StoredIndex}
	resp, err := Samples(context.Background(), d, req, reader)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	total := 0
	for name, n := range reads {
		if n > 2 {
			t.Fatalf("%s read %d times, want at most twice (all reads: %v)", name, n, reads)
		}
		total += n
	}
	if total > 2*(parts+1) {
		t.Fatalf("%d part reads for %d parts", total, parts+1)
	}
	requireAnswerLike(t, "100 times over 100 parts", resp, concatSamples(t, c, req), true)
	if resp.Timestamps[queries[0]] != 201 {
		t.Fatalf("the first time found line %d, want 201", resp.Timestamps[queries[0]])
	}
}

// Under file_tz, a part whose lines write two zone offsets (a change
// back from summer time) has a highest time known only as an upper
// bound. A time below that bound and above every line of the part is
// searched in that part, found in none of its lines, and the search
// moves on to the next part: the answer equals the concatenation's.
func TestSamples_InexactHighestTimeMovesOn(t *testing.T) {
	t.Setenv(config.ChainOverlapSecondsSetting.Name, "86400")
	zoned := func(text string) []byte { return []byte(text) }
	// Wall clocks in Berlin on the night summer time ends: 02:00-03:00
	// happens twice, at +02:00 and then at +01:00.
	first := zoned("2026-10-25T01:30:00.000+02:00 LINE 1 part=app.log.1 local=1\n" +
		"2026-10-25T02:40:00.000+02:00 LINE 2 part=app.log.1 local=2\n" +
		"2026-10-25T02:10:00.000+01:00 LINE 3 part=app.log.1 local=3\n" +
		"2026-10-25T02:25:00.000+01:00 LINE 4 part=app.log.1 local=4\n")
	active := zoned("2026-10-25T03:05:00.000+01:00 LINE 5 part=app.log local=1\n" +
		"2026-10-25T03:30:00.000+01:00 LINE 6 part=app.log local=2\n")
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1", text: first},
		{name: "app.log", text: active},
	})
	berlin, err := config.ParseZone("Europe/Berlin")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	c.zone = berlin
	d := describe(t, c.dir, c.name, Options{Scan: true, FileZone: berlin})
	if p := d.Response.Parts[0]; p.Name != "app.log.1" || !p.MaxIsBound {
		t.Fatalf("the first part's highest time is no bound: %s", jsonOf(t, p))
	}
	requireLikeConcatenation(t, c,
		SamplesRequest{Timestamps: []string{"2026-10-25T03:00", "2026-10-25T02:30", "2026-10-25T02:41..2026-10-25T03:10"}, BeforeContext: 1, AfterContext: 1},
	)
}

// boundReads is what a Samples call asked of each part to search its
// time bounds: the reads that carry samples.Request.TimeBounds, apart
// from the reads of the answer's pieces and of a part's line count.
type boundReads struct {
	// perPart counts the bound reads of each part, by name.
	perPart map[string]int
	// most is the most time queries one bound read carried.
	most int
}

// countedTimeSamples answers req on c through a reader that counts its
// bound reads: cold (described by a scan, no stored index) when scan,
// else from the stored indexes. It requires the answer to equal the
// concatenation's, and no bound read to carry more time queries than
// samples takes in one request (samples.MaxTimestampValues).
func countedTimeSamples(t *testing.T, c builtChain, req SamplesRequest, scan bool) boundReads {
	t.Helper()
	label := fmt.Sprintf("%d times, scan %v", len(req.Timestamps), scan)
	d := describe(t, c.dir, c.name, Options{Scan: scan, FileZone: c.zone})
	if d.Response.State != rxtypes.ChainStateReady {
		t.Fatalf("%s: the chain is %s: %+v", label, d.Response.State, d.Response.Reasons)
	}
	counted := boundReads{perPart: map[string]int{}}
	// The closure writes into counted, which it shares with this
	// function: Go closures capture the variable, not a copy of it.
	reader := func(ctx context.Context, p Part, r samples.Request) (*rxtypes.SamplesResponse, error) {
		if r.TimeBounds != nil {
			counted.perPart[p.Name]++
			counted.most = max(counted.most, len(r.Timestamps))
		}
		return samples.Resolve(ctx, r)
	}
	req.IndexLoader = samples.StoredIndex
	resp, err := Samples(context.Background(), d, req, reader)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	requireAnswerLike(t, label, resp, concatSamples(t, c, req), true)
	if counted.most > samples.MaxTimestampValues {
		t.Fatalf("%s: a bound read carried %d time queries, samples takes at most %d", label, counted.most, samples.MaxTimestampValues)
	}
	return counted
}

// requireBoundReads answers req on c cold and then with every part
// indexed (countedTimeSamples), and requires each part's bound reads to
// be what want gives it (none for a part want does not name).
func requireBoundReads(t *testing.T, c builtChain, req SamplesRequest, want map[string]int) {
	t.Helper()
	cold := countedTimeSamples(t, c, req, true)
	storeIndexes(t, c.dir, c.order...)
	indexed := countedTimeSamples(t, c, req, false)
	for label, got := range map[string]boundReads{"cold": cold, "indexed": indexed} {
		if !maps.Equal(got.perPart, want) {
			t.Fatalf("%s: bound reads %v, want %v", label, got.perPart, want)
		}
	}
}

// isoMillis is the layout of the time queries below.
const isoMillis = "2006-01-02T15:04:05.000"

// SECURITY: a closed range has two bounds, so the 1,000 time queries a
// request may hold can give one part 2,000 bounds to search, and samples
// takes at most 1,000 per request. 501 ranges inside one part (1,002
// bounds) are searched there in two reads, each within the limit, and
// answered as the concatenation answers them.
func TestSamples_ClosedRangesInOnePartAreSearchedInTwoReads(t *testing.T) {
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1", text: timedLines(chainBase, time.Second, 1, 600, "app.log.1")},
		{name: "app.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 601, 10, "app.log")},
	})
	var queries []string
	for i := 0; i < 501; i++ {
		from := chainBase.Add(time.Duration(i) * time.Second)
		queries = append(queries, from.Format(isoMillis)+".."+from.Add(2*time.Second).Format(isoMillis))
	}
	requireBoundReads(t, c, SamplesRequest{Timestamps: queries}, map[string]int{"app.log.1": 2})
}

// SECURITY: the forward sweep gathers in one part the bounds that pass
// over the parts before it. Under file_tz the first part, whose lines
// write two zone offsets, has a highest time known only as an upper
// bound, which the 1,000 bounds of 500 ranges reach; none of its lines
// does, so all of them move on to the third part, where one more time
// waits already. Its 1,001 bounds are searched in two reads, and the
// answer equals the concatenation's.
func TestSamples_BoundsGatheredInOnePartAreSearchedInTwoReads(t *testing.T) {
	t.Setenv(config.ChainOverlapSecondsSetting.Name, "86400")
	at := func(seconds int) string { return chainBase.Add(time.Duration(seconds) * time.Second).Format(isoMillis) }
	var first, second, third, active bytes.Buffer
	line := 0
	write := func(buf *bytes.Buffer, part string, seconds int, offset string) {
		line++
		fmt.Fprintf(buf, "%s%s LINE %d part=%s\n", at(seconds), offset, line, part)
	}
	for s := 0; s <= 10; s++ {
		offset := "+00:00"
		if s%2 == 1 {
			offset = "+00:05"
		}
		write(&first, "app.log.3", s, offset)
	}
	for s := 20; s <= 30; s++ {
		write(&second, "app.log.2", s, "+00:00")
	}
	for s := 40; s <= 600; s += 5 {
		write(&third, "app.log.1", s, "+00:00")
	}
	for s := 660; s <= 670; s++ {
		write(&active, "app.log", s, "+00:00")
	}
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.3", text: first.Bytes()}, {name: "app.log.2", text: second.Bytes()},
		{name: "app.log.1", text: third.Bytes()}, {name: "app.log", text: active.Bytes()},
	})
	utc, err := config.ParseZone("UTC")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	c.zone = utc
	var queries []string
	for i := 0; i < 500; i++ {
		from := chainBase.Add(35*time.Second + time.Duration(i)*10*time.Millisecond)
		queries = append(queries, from.Format(isoMillis)+".."+from.Add(20*time.Second).Format(isoMillis))
	}
	queries = append(queries, at(8*60))
	requireBoundReads(t, c, SamplesRequest{Timestamps: queries}, map[string]int{"app.log.3": 1, "app.log.1": 2})
}

// 1,000 single times inside one part, as many bounds as samples takes
// in one request, are searched there in one read, and answered as the
// concatenation answers them.
func TestSamples_AThousandTimesInOnePartAreSearchedInOneRead(t *testing.T) {
	c := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1", text: timedLines(chainBase, time.Second, 1, 1000, "app.log.1")},
		{name: "app.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 1001, 10, "app.log")},
	})
	var queries []string
	for i := 0; i < samples.MaxTimestampValues; i++ {
		queries = append(queries, chainBase.Add(time.Duration(i)*time.Second).Format(isoMillis))
	}
	requireBoundReads(t, c, SamplesRequest{Timestamps: queries, BeforeContext: 1, AfterContext: 1}, map[string]int{"app.log.1": 1})
}

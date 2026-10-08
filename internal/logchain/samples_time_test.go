package logchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

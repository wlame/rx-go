package logchain

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// syslogLines is n lines of a year-less syslog, the first at start and
// each next one step later: `Dec 31 23:59:58 host app: LINE <n>`.
func syslogLines(start time.Time, step time.Duration, firstGlobal, n int) []byte {
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * step)
		fmt.Fprintf(&buf, "%s host app: LINE %d\n", at.Format(time.Stamp), firstGlobal+i)
	}
	return buf.Bytes()
}

// Year-less lines take their year from their own part's modification
// time: parts rotated around a new year read December as the old year
// and January as the new one, so the order and the checks hold. Each
// part's times equal what rx gives that part as a single file.
func TestDescribe_YearLessPartsAcrossANewYear(t *testing.T) {
	dir := t.TempDir()
	dec30 := time.Date(2025, 12, 30, 22, 0, 0, 0, time.UTC)
	dec31 := time.Date(2025, 12, 31, 22, 0, 0, 0, time.UTC)
	jan1 := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
	writeChainFiles(t, dir, []chainFile{
		{name: "syslog.2", text: syslogLines(dec30, time.Minute, 1, 60), mtime: dec30.Add(time.Hour)},
		{name: "syslog.1", text: syslogLines(dec31, time.Minute, 61, 120), mtime: jan1.Add(-4 * time.Second)},
		{name: "syslog", text: syslogLines(jan1, time.Minute, 181, 30), mtime: jan1.Add(time.Hour)},
	})
	for _, opts := range []Options{{Scan: true}, {}} {
		if !opts.Scan {
			storeIndexes(t, dir, "syslog.2", "syslog.1")
		}
		d := describe(t, dir, "syslog", opts)
		if d.Response.State != rxtypes.ChainStateReady || !slices.Equal(partNamesOf(d), []string{"syslog.2", "syslog.1", "syslog"}) {
			t.Fatalf("scan %v: state %s %+v, order %v", opts.Scan, d.Response.State, d.Response.Reasons, partNamesOf(d))
		}
		wantFirst := map[string]time.Time{"syslog.2": dec30, "syslog.1": dec31, "syslog": jan1}
		for _, p := range d.Response.Parts {
			if *p.FirstMs != wantFirst[p.Name].UnixMilli() {
				t.Fatalf("scan %v: %s first %s, want %s", opts.Scan, p.Name, time.UnixMilli(*p.FirstMs).UTC(), wantFirst[p.Name])
			}
			_, single := singleFileAnswer(t, p.Path)
			if *p.FirstMs != *single.FirstMs || *p.LastMs != *single.LastMs {
				t.Fatalf("scan %v: %s first/last %d/%d, as a file %d/%d", opts.Scan, p.Name, *p.FirstMs, *p.LastMs,
					*single.FirstMs, *single.LastMs)
			}
		}
	}
}

// zonedLines is the lines of one file whose timestamps carry zones,
// written as given.
func zonedLines(stamps ...string) []byte {
	var buf bytes.Buffer
	for i, s := range stamps {
		fmt.Fprintf(&buf, "%s LINE %d\n", s, i+1)
	}
	return buf.Bytes()
}

// file_tz reads every part's lines as the wall clock they write, in the
// zone, as it reads one file: a zone-less part moves by the zone's
// offset, and a zoned part reads its written wall clocks in the zone.
// Each part's times equal the single-file answer under the same zone.
// In a part whose lines write two offsets (a change to winter time),
// the highest time is an upper bound, at or above the latest wall clock
// any line writes.
func TestDescribe_FileZoneReadsEveryPartInTheZone(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		// Written as the clocks went back: 02:59:59 at +02:00 is followed
		// an hour later by 02:00:00 at +01:00.
		{name: "z.log.2", text: zonedLines("2025-10-26T02:30:00.000+02:00", "2025-10-26T02:59:59.000+02:00",
			"2025-10-26T02:00:00.000+01:00", "2025-10-26T02:10:00.000+01:00")},
		{name: "z.log.1", text: zonedLines("2025-10-26T05:00:00.000+02:00", "2025-10-26T05:30:00.000+02:00")},
		{name: "z.log", text: timedLines(time.Date(2025, 10, 26, 9, 0, 0, 0, time.UTC), time.Minute, 7, 5, "active")},
	})
	tokyo, err := config.ParseZone("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	plain := describe(t, dir, "z.log", Options{Scan: true})
	inTokyo := describe(t, dir, "z.log", Options{Scan: true, FileZone: tokyo})
	byName := func(d *Description) map[string]rxtypes.ChainPart {
		out := map[string]rxtypes.ChainPart{}
		for _, p := range d.Response.Parts {
			out[p.Name] = p
		}
		return out
	}
	before, after := byName(plain), byName(inTokyo)
	const nine = int64(9 * time.Hour / time.Millisecond)
	// The zone-less active file: 09:00 read in Tokyo is 00:00 UTC.
	if *after["z.log"].FirstMs != *before["z.log"].FirstMs-nine {
		t.Fatalf("active first %d, want %d", *after["z.log"].FirstMs, *before["z.log"].FirstMs-nine)
	}
	// A zoned part with one offset: 05:00 written at +02:00 read in
	// Tokyo is 05:00+09:00, its instant moved by 2 − 9 hours, exactly.
	if *after["z.log.1"].FirstMs != *before["z.log.1"].FirstMs+(2*3600_000-nine) || after["z.log.1"].MaxIsBound {
		t.Fatalf("one-offset part: %+v", after["z.log.1"])
	}
	for _, d := range []*Description{plain, inTokyo} {
		zone := config.Zone{}
		if d == inTokyo {
			zone = tokyo
		}
		for _, p := range d.Response.Parts {
			_, single := singleFileAnswerIn(t, p.Path, zone)
			if *p.FirstMs != *single.FirstMs || *p.LastMs != *single.LastMs {
				t.Fatalf("%s in %q: first/last %d/%d, as a file %d/%d", p.Name, zone.Name, *p.FirstMs, *p.LastMs,
					*single.FirstMs, *single.LastMs)
			}
		}
	}
	// The part with two offsets: without a zone its highest instant is
	// exact (02:10+01:00); in Tokyo the latest wall clock is 02:59:59
	// (a line before it), and the bound is at or above it.
	dst := after["z.log.2"]
	latestWall := time.Date(2025, 10, 26, 2, 59, 59, 0, mustLocation(t, "Asia/Tokyo")).UnixMilli()
	if before["z.log.2"].MaxIsBound || *before["z.log.2"].MaxMs != time.Date(2025, 10, 26, 1, 10, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("without a zone: %+v", before["z.log.2"])
	}
	if !dst.MaxIsBound || *dst.MaxMs < latestWall {
		t.Fatalf("in Tokyo: max %d bound %v, the latest wall clock is %d", *dst.MaxMs, dst.MaxIsBound, latestWall)
	}
}

// singleFileAnswerIn is singleFileAnswer under a file zone.
func singleFileAnswerIn(t *testing.T, path string, zone config.Zone) (int64, *rxtypes.TimeRangeResponse) {
	t.Helper()
	lines, _ := singleFileAnswer(t, path)
	tr, err := samples.TimeRange(context.Background(), samples.Request{Path: path, FileZone: zone, IndexLoader: samples.NoIndex})
	if err != nil {
		t.Fatalf("time range of %s: %v", filepath.Base(path), err)
	}
	return lines, tr
}

// mustLocation loads a zone for a test.
func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zone %s on this host: %v", name, err)
	}
	return loc
}

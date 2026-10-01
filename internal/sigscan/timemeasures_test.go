package sigscan

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// stampAt writes an ISO timestamp t after 2026-01-02T10:00:00Z.
func stampAt(after time.Duration) string {
	return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).Add(after).Format("2006-01-02T15:04:05.000Z")
}

// hourWithBurst is one line a second for an hour, except the minute
// from 10:30:00, which holds 200 lines.
func hourWithBurst() string {
	var b strings.Builder
	for s := 0; s < 3600; s++ {
		if s/60 == 30 {
			if s%60 == 0 {
				for i := range 200 {
					fmt.Fprintf(&b, "%s burst %d\n", stampAt(30*time.Minute+time.Duration(i)*300*time.Millisecond), i)
				}
			}
			continue
		}
		fmt.Fprintf(&b, "%s tick %d\n", stampAt(time.Duration(s)*time.Second), s)
	}
	return b.String()
}

// One line a second for an hour and 200 lines in one minute give one
// burst over that minute; the series has 60 buckets of 60 lines and the
// burst's 200; auto picks 5 s for an hour; a quieter burst under
// min_count is none. A few chunkings and storages agree.
func TestScan_DensityMeasure(t *testing.T) {
	text := []byte(hourWithBurst())
	sigs := loadSigs(t, sigFile(
		measureSig("minutes", "density", "bucket: 1m, factor: 3"),
		measureSig("auto", "density", ""),
	), "minutes", "auto")
	ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, Options{})

	// Lines 1801 to 2000 are the burst minute's 200 lines.
	if got := spans(ref, "minutes"); !slices.Equal(got, []lineSpan{{1801, 2000}}) {
		t.Errorf("bursts %v, want lines 1801-2000", got)
	}
	series := ref.BySignature["minutes"].Series
	wantCounts := slices.Repeat([]int64{60}, 60)
	wantCounts[30] = 200
	if series == nil || series.BucketMS != 60000 || series.StartMS != msOf(t, "2026-01-02T10:00:00Z") || !slices.Equal(series.Counts, wantCounts) {
		t.Errorf("series %+v, want 1m buckets from 10:00 holding %v", series, wantCounts)
	}
	if got := ref.BySignature["auto"].Series; got == nil || got.BucketMS != 5000 || len(got.Counts) != 720 {
		t.Errorf("auto series %+v, want 720 buckets of 5s", got)
	}
	requireTimes(t, ref, "minutes", msOf(t, "2026-01-02T10:30:00Z"))

	for _, v := range []variant{chunkVariants()[2], chunkVariants()[13], storageVariants()[0], storageVariants()[len(storageVariants())-1]} {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, ref, scanVariant(t, v, text, sigs, Options{}))
		})
	}

	quietSigs := loadSigs(t, sigFile(measureSig("quiet", "density", "bucket: 1m, factor: 3, min_count: 250")), "quiet")
	if got := scanOne(t, string(text), quietSigs, Options{}).BySignature["quiet"]; got.Count != 0 || got.Series == nil {
		t.Errorf("a burst under min_count: %d findings, series %v; want none and a series", got.Count, got.Series)
	}
}

// secondsLog is a line a second for each of the given seconds, with
// extra lines (unstamped) after the stamped line of any second in more.
func secondsLog(seconds []int, more map[int]string) string {
	var b strings.Builder
	for i, s := range seconds {
		fmt.Fprintf(&b, "%s line %d\n", stampAt(time.Duration(s)*time.Second), i+1)
		if extra, ok := more[s]; ok {
			b.WriteString(extra + "\n")
		}
	}
	return b.String()
}

// rangeSeconds is the seconds from a to b, one apart.
func rangeSeconds(a, b int) []int {
	var out []int
	for s := a; s <= b; s++ {
		out = append(out, s)
	}
	return out
}

// A 10-minute silence in a line-a-second log is found as the line before
// the silence and the first stamped line after it; when an unstamped
// continuation line comes before the silence, the finding starts there,
// with the time of the stamped line before it.
func TestScan_TimeGapMeasure(t *testing.T) {
	sigs := loadSigs(t, sigFile(measureSig("gap", "time-gap", "")), "gap")
	seconds := append(rangeSeconds(0, 99), rangeSeconds(700, 749)...)
	plain := scanOne(t, secondsLog(seconds, nil), sigs, Options{LookbackBytes: 1 << 10})
	if got := spans(plain, "gap"); !slices.Equal(got, []lineSpan{{100, 101}}) {
		t.Errorf("silence: %v, want lines 100-101", got)
	}
	cont := scanOne(t, secondsLog(seconds, map[int]string{99: "  continued"}), sigs, Options{LookbackBytes: 1 << 10})
	if got := spans(cont, "gap"); !slices.Equal(got, []lineSpan{{101, 102}}) {
		t.Errorf("silence after a continuation line: %v, want lines 101-102", got)
	}
	requireTimes(t, cont, "gap", msOf(t, stampAt(99*time.Second)))
	short := scanOne(t, secondsLog(append(rangeSeconds(0, 99), rangeSeconds(150, 199)...), nil), sigs, Options{})
	if got := short.BySignature["gap"].Count; got != 0 {
		t.Errorf("a 51-second silence under min_gap 1m: %d findings", got)
	}
}

// A step back of 5 s is found; one of 0.5 s is within the tolerance.
func TestScan_TimeBackwardsMeasure(t *testing.T) {
	sigs := loadSigs(t, sigFile(measureSig("back", "time-backwards", "")), "back")
	text := lines(
		stampAt(0)+" a", stampAt(time.Second)+" b", stampAt(10*time.Second)+" c",
		stampAt(5*time.Second)+" d", stampAt(6*time.Second)+" e", stampAt(5500*time.Millisecond)+" f",
	)
	res := scanOne(t, text, sigs, Options{})
	if got := spans(res, "back"); !slices.Equal(got, []lineSpan{{4, 4}}) {
		t.Errorf("steps back %v, want line 4", got)
	}
	requireTimes(t, res, "back", msOf(t, stampAt(5*time.Second)))
}

// timeMeasuresFixture: a header line before the first stamped line (not
// counted by density), a burst second with continuation lines, a
// silence after a stamped line and one after a continuation line, and a
// step back, on every chunking and storage.
var timeMeasuresFixture = fixture{
	name: "time measures",
	def: measureSig("gap", "time-gap", "min_gap: 10s, factor: 2") +
		measureSig("back", "time-backwards", "") +
		measureSig("burst", "density", "bucket: 1s, factor: 2, z: 1, min_count: 4"),
	ids: []string{"gap", "back", "burst"},
	text: lines(
		"header line",
		stampAt(0)+" a", stampAt(500*time.Millisecond)+" b",
		stampAt(time.Second)+" c", "  cont 1",
		stampAt(1200*time.Millisecond)+" d", stampAt(1400*time.Millisecond)+" e",
		stampAt(1600*time.Millisecond)+" f", stampAt(1800*time.Millisecond)+" g", "  cont 2",
		stampAt(2*time.Second)+" h", stampAt(3*time.Second)+" i",
		stampAt(20*time.Second)+" j", stampAt(14*time.Second)+" k",
		stampAt(21*time.Second)+" l", "  cont 3",
		stampAt(40*time.Second)+" m", stampAt(41*time.Second)+" n",
	),
	opts: Options{LookbackBytes: 1 << 10},
	want: map[string][]lineSpan{
		"gap":   {{12, 13}, {16, 17}},
		"back":  {{14, 14}},
		"burst": {{4, 10}},
	},
	timed: true,
}

// gapMedianFixture: lines 2 s apart with a silence of 8 s and one of
// 12 s; with min_gap 1s and factor 5 the median interval decides: only
// the 12 s silence reaches 5 × 2 s.
var gapMedianFixture = func() fixture {
	var seconds []int
	for s := 0; s <= 40; s += 2 {
		seconds = append(seconds, s)
	}
	for s := 48; s <= 70; s += 2 {
		seconds = append(seconds, s)
	}
	for s := 82; s <= 100; s += 2 {
		seconds = append(seconds, s)
	}
	return fixture{
		name: "time-gap against the median interval",
		def:  measureSig("gap", "time-gap", "min_gap: 1s, factor: 5"),
		ids:  []string{"gap"},
		text: secondsLog(seconds, nil),
		want: map[string][]lineSpan{"gap": {{33, 34}}},
	}
}()

// A silence counts only when it reaches factor × the median interval.
func TestScan_TimeGapAgainstTheMedian(t *testing.T) {
	gapMedianFixture.run(t)
}

// The time measures' findings and times, and density's series: every
// line from the first stamped one on is counted in the second of its
// time, a continuation line in its stamped line's second.
func TestScan_TimeMeasures(t *testing.T) {
	timeMeasuresFixture.run(t)
	res := scanOne(t, timeMeasuresFixture.text, loadSigs(t, sigFile(timeMeasuresFixture.def), "burst"), Options{})
	want := make([]int64, 42)
	for second, n := range map[int]int64{0: 2, 1: 7, 2: 1, 3: 1, 14: 1, 20: 1, 21: 2, 40: 1, 41: 1} {
		want[second] = n
	}
	if got := res.BySignature["burst"].Series; got == nil || got.BucketMS != 1000 || !slices.Equal(got.Counts, want) {
		t.Errorf("series %+v, want %v", got, want)
	}
}

// A file without timestamps gives the time measures nothing, and
// density no series.
func TestScan_TimeMeasuresWithoutTimestamps(t *testing.T) {
	sigs := loadSigs(t, sigFile(timeMeasuresFixture.def), timeMeasuresFixture.ids...)
	res := scanOne(t, lines("a", "b", "c"), sigs, Options{})
	for _, id := range timeMeasuresFixture.ids {
		if got := res.BySignature[id]; got.Count != 0 || got.Series != nil {
			t.Errorf("%s: %d findings, series %v", id, got.Count, got.Series)
		}
	}
}

// The bucket of a density series: an explicit one as it is, auto the
// smallest size of the divisor chain (1s, 5s, 15s, 30s, 1m, 5m, 15m,
// 30m, 1h, 3h, 6h, 12h, 1d) that cuts the span into at most 1,000
// buckets, and either doubled while the series would pass
// maxSeriesBuckets; and the slot level divides the bucket of every
// longer span.
func TestDensityGrid(t *testing.T) {
	day := (24 * time.Hour).Milliseconds()
	cases := []struct {
		grid   densityGrid
		span   int64
		bucket int64
	}{
		{densityGrid{}, 0, 1000},
		{densityGrid{}, 3_599_000, 5000},
		{densityGrid{}, 1000 * 60_000, 60_000},
		{densityGrid{}, 1000*60_000 + 1, 300_000},
		{densityGrid{}, 1_500_000, 5000},
		{densityGrid{}, 9_000_000, 15_000},
		{densityGrid{}, 7_000_000_000, 10_800_000},
		{densityGrid{}, 2000 * day, day},
		{densityGrid{}, 70_000 * day, 2 * day},
		{densityGrid{explicit: 7000}, 3_600_000, 7000},
		{densityGrid{explicit: 1000}, 100_000_000, 2000},
	}
	for _, tc := range cases {
		if got := tc.grid.bucket(tc.span); got != tc.bucket {
			t.Errorf("%+v over %d ms: bucket %d, want %d", tc.grid, tc.span, got, tc.bucket)
		}
	}
	for _, grid := range []densityGrid{{}, {explicit: 7000}} {
		spans := []int64{0, 999, 3_599_000, 7_200_000, 86_400_000, 1000 * day, 100_000 * day}
		for i, a := range spans {
			for _, b := range spans[i:] {
				if grid.bucket(b)%grid.level(a) != 0 || grid.level(b)%grid.level(a) != 0 {
					t.Errorf("%+v: level %d of span %d does not divide bucket %d and level %d of span %d",
						grid, grid.level(a), a, grid.bucket(b), grid.level(b), b)
				}
			}
		}
	}
}

// An interval keeps three significant digits: exact below a second.
func TestIntervalBuckets(t *testing.T) {
	for _, tc := range []struct{ v, want int64 }{
		{0, 0}, {999, 999}, {1000, 1000}, {1234, 1230}, {61_234, 61_200}, {3_600_000, 3_600_000}, {9_999, 9_990},
	} {
		if got := intervalValue(intervalBucket(tc.v)); got != tc.want {
			t.Errorf("%d ms: bucket value %d, want %d", tc.v, got, tc.want)
		}
	}
}

// Past its limit, the keeper keeps the longest candidates, whole groups
// of one length at a time, the same set whatever the order they come in.
func TestGapKeeperKeepsTheLongest(t *testing.T) {
	cases := []struct{ deltas, want []int64 }{
		{[]int64{5, 9, 5, 7, 9, 3, 8, 5}, []int64{9, 9, 8}}, // the cutoff rises to 7
		{[]int64{1, 2, 3, 4, 5, 6, 7, 8}, []int64{8, 7, 6}},
	}
	for _, tc := range cases {
		requireKept(t, tc.deltas, tc.want)
	}
}

// requireKept feeds deltas to a keeper of limit 3, in order and
// reversed, and fails unless it keeps want.
func requireKept(t *testing.T, deltas, want []int64) {
	t.Helper()
	for _, order := range [][]int64{deltas, reversed(deltas)} {
		k := gapKeeper{limit: 3, cutoff: -1}
		for _, d := range order {
			k.add(gapCandidate{delta: d})
		}
		k.compact()
		var got []int64
		for _, c := range k.items {
			got = append(got, c.delta)
		}
		slices.Sort(got)
		slices.Reverse(got)
		if !slices.Equal(got, want) {
			t.Errorf("order %v: kept %v, want %v", order, got, want)
		}
	}
}

// reversed returns a reversed copy.
func reversed(s []int64) []int64 {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

// Empty buckets do not enter the median: three lines a second for ten
// seconds, a silence of fifty, and ten seconds more are no burst, though
// a median over every bucket would be 0 and make every busy second one.
func TestScan_DensityMedianOfBusyBuckets(t *testing.T) {
	var ls []string
	for _, s := range append(rangeSeconds(0, 9), rangeSeconds(60, 69)...) {
		for i := range 3 {
			ls = append(ls, fmt.Sprintf("%s line %d", stampAt(time.Duration(s)*time.Second+time.Duration(i)*100*time.Millisecond), i))
		}
	}
	sigs := loadSigs(t, sigFile(measureSig("burst", "density", "bucket: 1s, factor: 2, min_count: 2")), "burst")
	res := scanOne(t, lines(ls...), sigs, Options{})
	if got := res.BySignature["burst"]; got.Count != 0 || len(got.Series.Counts) != 70 {
		t.Errorf("%d bursts over %d buckets, want none over 70", got.Count, len(got.Series.Counts))
	}
}

// A step back counts as an interval of 0 in the median.
func TestIntervalCountsStepBackIsZero(t *testing.T) {
	var c intervalCounts
	for _, d := range []int64{-6000, -5000, 100} {
		c.add(d)
	}
	if got := c.lowerMedian(); got != 0 {
		t.Errorf("median %d, want 0", got)
	}
}

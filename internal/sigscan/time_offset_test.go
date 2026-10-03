package sigscan

import (
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/timestamps"
)

// zonedText is an ISO log whose lines write their offsets: +01:00, then
// +02:00 after a daylight-saving change, then Z, a line that writes no
// zone, a line that goes back in time, and a silence. Findings of every
// kind take their time from their own line or from an earlier one.
var zonedText = lines(
	"2026-03-29T01:59:58+01:00 INFO start",
	"2026-03-29T01:59:59+01:00 ERROR before the change",
	"Traceback (most recent call last):",
	"  File \"app.py\", line 3",
	"ValueError: bad input",
	"2026-03-29T03:00:01+02:00 ERROR after the change",
	"  continued",
	"  more detail",
	"2026-03-29T01:00:02Z ERROR in UTC",
	"2026-03-29T03:00:03 ERROR without a zone",
	"2026-03-29T03:00:04+02:00 INFO back in time",
	"2026-03-29T04:30:00+02:00 ERROR after a silence",
	"  tail",
)

// zonedSigs find ERROR lines, tracebacks, runs of indented lines,
// silences and steps back in time.
var zonedSigs = sigFile(
	regexSig("error", `ERROR`),
	kindSig("tb", "block", `start: '^Traceback \(most recent call last\):'`, `continue: '^\s'`, `end: '^[A-Za-z_][\w.]*(?::|$)'`),
	kindSig("indent", "run", `match: '^\s'`, `min: 2`),
	measureSig("gap", "time-gap", "min_gap: 10m, factor: 2"),
	measureSig("back", "time-backwards", "tolerance: 1s"),
)

// writtenOffsets parses every line of text with the parser an index
// build uses and gives, for each line that has a timestamp of its own,
// the zone offset it writes (minutes east of UTC) and its stamp, by line
// start offset.
func writtenOffsets(t *testing.T, text string) map[int64]timestamps.Stamp {
	t.Helper()
	format, ok, err := index.DetectTimeFormat(strings.NewReader(text), int64(len(text)), filekind.Kind{})
	if err != nil || !ok {
		t.Fatalf("detect: %v %v", ok, err)
	}
	parser, err := timestamps.NewParser(format, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]timestamps.Stamp{}
	start := int64(0)
	for _, l := range strings.SplitAfter(text, "\n") {
		if l == "" {
			continue
		}
		if s, ok := index.LineStamp(parser, []byte(l)); ok {
			out[start] = s
		}
		start += int64(len(l))
	}
	return out
}

// Every finding with a time carries the offset written on the stamped
// line its time comes from (its own line, or the nearest earlier stamped
// one): 60, 120, 0 for Z and 0 for a line that writes no zone; and
// every chunking and storage gives the same offsets.
func TestScan_TimeOffsetIsTheOffsetTheStampedLineWrites(t *testing.T) {
	sigs := loadSigs(t, zonedSigs, "error", "tb", "indent", "gap", "back")
	opts := Options{LookbackBytes: 64 << 10}
	want := scanOne(t, zonedText, sigs, opts)
	stamps := writtenOffsets(t, zonedText)
	timed := 0
	for id, res := range want.BySignature {
		for _, f := range res.Findings {
			if f.TimeMS == nil {
				if f.TimeOffsetMinutes != 0 {
					t.Errorf("%s at line %d: offset %d without a time", id, f.StartLine, f.TimeOffsetMinutes)
				}
				continue
			}
			// The stamped line the time comes from: the latest one that
			// starts at or before the finding and has its instant.
			var from timestamps.Stamp
			found, best := false, int64(-1)
			for start, s := range stamps {
				if start <= f.StartOffset && s.Ms == *f.TimeMS && start > best {
					from, found, best = s, true, start
				}
			}
			if !found {
				t.Fatalf("%s at line %d: no stamped line has its time %d", id, f.StartLine, *f.TimeMS)
			}
			if f.TimeOffsetMinutes != from.OffsetMinutes {
				t.Errorf("%s at line %d: offset %d, the stamped line writes %d", id, f.StartLine, f.TimeOffsetMinutes, from.OffsetMinutes)
			}
			timed++
		}
	}
	if timed < 8 {
		t.Fatalf("only %d findings with a time: the fixture does not cover the kinds", timed)
	}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, want, scanVariant(t, v, []byte(zonedText), sigs, opts))
		})
	}
}

// A density burst carries the offset of the stamped line its time comes
// from, as every other finding does: the time-measure fixture with its
// timestamps written at +02:00, over every chunking and storage.
func TestScan_DensityBurstCarriesTheOffset(t *testing.T) {
	text := strings.ReplaceAll(timeMeasuresFixture.text, "Z ", "+02:00 ")
	if text == timeMeasuresFixture.text {
		t.Fatal("the fixture writes no Z to replace")
	}
	sigs := loadSigs(t, sigFile(measureSig("burst", "density", "bucket: 1s, factor: 2, z: 1, min_count: 4")), "burst")
	opts := Options{LookbackBytes: 1 << 10}
	want := scanOne(t, text, sigs, opts)
	bursts := want.BySignature["burst"].Findings
	if len(bursts) != 1 || bursts[0].TimeMS == nil || bursts[0].TimeOffsetMinutes != 120 {
		t.Fatalf("bursts %+v, want one with offset 120", bursts)
	}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, want, scanVariant(t, v, []byte(text), sigs, opts))
		})
	}
}

// coarseBucketText is a zoned log whose first line writes +02:00 and the
// later instant (10:00:05Z), and whose second line writes Z and an
// instant 5 s earlier; 48 more lines at 10:00:10Z make 50 lines in that
// 15 s bucket, and 69 lines 105 s apart reach 12:00:00Z, so auto picks
// 15 s buckets over the 2 h span. The first two lines start in
// different slots and meet only when the slots coarsen, where the slot
// of the earlier instant takes the first line from the other one.
var coarseBucketText = func() string {
	stamp := func(after time.Duration) string {
		return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).Add(after).Format(time.RFC3339) + " INFO busy"
	}
	ls := []string{"2026-01-02T12:00:05+02:00 INFO busy", stamp(0)}
	for range 48 {
		ls = append(ls, stamp(10*time.Second))
	}
	for i := range 69 {
		ls = append(ls, stamp(time.Minute+time.Duration(i)*105*time.Second))
	}
	return lines(ls...)
}()

// A burst whose first line is in a slot merged into the slot of an
// earlier instant takes that line's offset with its time: the burst
// starts at line 1, at 10:00:05Z written as +02:00, over every chunking
// and storage.
func TestScan_DensityBurstOffsetSurvivesCoarseningSlots(t *testing.T) {
	sigs := loadSigs(t, sigFile(measureSig("burst", "density", "bucket: auto, min_count: 10")), "burst")
	opts := Options{LookbackBytes: 1 << 10}
	want := scanOne(t, coarseBucketText, sigs, opts)
	bursts := want.BySignature["burst"].Findings
	if len(bursts) != 1 || bursts[0].StartLine != 1 || bursts[0].EndLine != 50 {
		t.Fatalf("bursts %+v, want one over lines 1-50", bursts)
	}
	requireTimes(t, want, "burst", msOf(t, "2026-01-02T10:00:05Z"))
	if off := bursts[0].TimeOffsetMinutes; off != 120 {
		t.Fatalf("burst offset %d, want 120 (its first line writes +02:00)", off)
	}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, want, scanVariant(t, v, []byte(coarseBucketText), sigs, opts))
		})
	}
}

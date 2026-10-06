package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timedLine is one line of a generated log: its text, and the own
// timestamp a reader must find on it (has is false for a line with
// none, such as a continuation line).
type timedLine struct {
	text string
	ms   int64
	has  bool
}

// joinLines lays lines end to end, each ended by a newline, except the
// last when finalNewline is false.
func joinLines(lines []timedLine, finalNewline bool) []byte {
	var buf bytes.Buffer
	for i, l := range lines {
		buf.WriteString(l.text)
		if i < len(lines)-1 || finalNewline {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// bruteMaxBefore is the latest own timestamp among the lines numbered
// below lineNumber, computed the slow way, or nil when none has one.
func bruteMaxBefore(lines []timedLine, lineNumber int64) *int64 {
	var best *int64
	for i := int64(0); i < lineNumber-1 && i < int64(len(lines)); i++ {
		if !lines[i].has {
			continue
		}
		if best == nil || lines[i].ms > *best {
			v := lines[i].ms
			best = &v
		}
	}
	return best
}

// pointAt is the TimePoint of the 1-based line n of lines.
func pointAt(lines []timedLine, n int) *rxtypes.TimePoint {
	offset := int64(0)
	for i := 0; i < n-1; i++ {
		offset += int64(len(lines[i].text)) + 1
	}
	return &rxtypes.TimePoint{Ms: lines[n-1].ms, Line: int64(n), Offset: offset}
}

// expectedTimeIndex is the time section of lines with every field but
// max_before, computed from the generator's own values.
func expectedTimeIndex(lines []timedLine, base rxtypes.TimeIndex) rxtypes.TimeIndex {
	want := base
	var maxMs int64
	seen := false
	for i, l := range lines {
		if !l.has {
			continue
		}
		want.TimestampedLines++
		if want.First == nil {
			want.First = pointAt(lines, i+1)
		}
		want.Last = pointAt(lines, i+1)
		if seen && maxMs-l.ms > 1000 {
			want.BackwardSteps++
			want.MaxBackwardMs = max(want.MaxBackwardMs, maxMs-l.ms)
		}
		if !seen || l.ms > maxMs {
			maxMs, seen = l.ms, true
		}
	}
	return want
}

// requireMaxBeforeMatches checks max_before against the brute force at
// every checkpoint of idx, and that it never decreases.
func requireMaxBeforeMatches(t *testing.T, label string, idx *rxtypes.UnifiedFileIndex, lines []timedLine) {
	t.Helper()
	ti := idx.TimeIndex
	if len(ti.MaxBefore) != len(idx.LineIndex) {
		t.Fatalf("%s: max_before has %d entries, line_index %d", label, len(ti.MaxBefore), len(idx.LineIndex))
	}
	var previous *int64
	for i, cp := range idx.LineIndex {
		got, want := ti.MaxBefore[i], bruteMaxBefore(lines, cp.LineNumber)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: checkpoint %d (line %d): max_before %s, want %s",
				label, i, cp.LineNumber, msText(got), msText(want))
		}
		if previous != nil && (got == nil || *got < *previous) {
			t.Fatalf("%s: max_before decreases at checkpoint %d: %s after %s",
				label, i, msText(got), msText(previous))
		}
		previous = got
	}
}

func msText(v *int64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// withoutMaxBefore returns a copy of ti without its max_before, the one
// field that follows the checkpoints of each storage format.
func withoutMaxBefore(ti *rxtypes.TimeIndex) rxtypes.TimeIndex {
	out := *ti
	out.MaxBefore = nil
	return out
}

// buildAt writes text to a file of the given name under dir, sets its
// mtime and builds its index with the given step.
func buildAt(t *testing.T, dir, name string, write func(*testing.T, string, []byte), text []byte, mtime time.Time, step int64) *rxtypes.UnifiedFileIndex {
	t.Helper()
	path := filepath.Join(dir, name)
	write(t, path, text)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	idx, err := Build(path, BuildOptions{StepBytes: step})
	if err != nil {
		t.Fatalf("Build(%s): %v", name, err)
	}
	return idx
}

var (
	// fileMtime is the mtime every fixture gets, so a year-less format
	// reads its year from a fixed date.
	fileMtime = time.Date(2025, 12, 27, 12, 0, 0, 0, time.UTC)
	plus2     = time.FixedZone("+02:00", 2*60*60)
)

func intPtr(v int) *int { return &v }

func strPtr(s string) *string { return &s }

// logShape is one way the real logs of the playground write their
// lines, reproduced in a few dozen generated lines.
type logShape struct {
	name string
	// first is the moment of the first line; each later timestamped
	// line is 1.5 s after the one before.
	first time.Time
	// line writes the timestamped line for moment t.
	line func(t time.Time) string
	// ms is the value the index must hold for moment t (the file frame).
	ms func(t time.Time) int64
	// continuation, when set, follows every fifth timestamped line.
	continuation string
	// other, when set, writes every fifth timestamped line instead of
	// line: a second component writing to the same file.
	other func(t time.Time) string
	want  rxtypes.TimeIndex
}

func wallMs(t time.Time) int64   { return t.UnixMilli() }
func secondMs(t time.Time) int64 { return t.Truncate(time.Second).UnixMilli() }
func unpadded(t time.Time) string {
	return t.Format("2006-1-2 15:4:5") + fmt.Sprintf(":%d", t.Nanosecond()/1e6)
}

var logShapes = []logShape{
	{
		name:  "middleware iso with milliseconds",
		first: time.Date(2025, 12, 10, 7, 0, 4, 574e6, time.UTC),
		line:  func(t time.Time) string { return t.Format("2006-01-02 15:04:05.000") + " INFO [main] request served" },
		ms:    wallMs,
		want:  rxtypes.TimeIndex{Format: "iso", Anchored: true, FirstText: strPtr("2025-12-10 07:00:04.574")},
	},
	{
		// middleware.log is written on a -07:00 host: its own lines carry
		// no zone, and the JVM's GC lines between them carry -0700. One
		// clock, so the GC lines read as the wall clock they show.
		name:  "middleware with zoned GC lines in a zone-less file",
		first: time.Date(2025, 12, 10, 7, 0, 4, 574e6, time.UTC),
		line:  func(t time.Time) string { return t.Format("2006-01-02 15:04:05.000") + " INFO [main] request served" },
		other: func(t time.Time) string {
			return "[" + t.Format("2006-01-02T15:04:05.000") + "-0700][1246.139s][info][gc] GC(313) Pause Young"
		},
		ms:   wallMs,
		want: rxtypes.TimeIndex{Format: "iso", Anchored: true, FirstText: strPtr("2025-12-10 07:00:04.574")},
	},
	{
		name:  "SOMELOG one-digit fields and milliseconds after a colon",
		first: time.Date(2025, 2, 5, 8, 6, 2, 7e6, time.UTC),
		line:  func(t time.Time) string { return unpadded(t) + " [worker] tick" },
		ms:    wallMs,
		want:  rxtypes.TimeIndex{Format: "iso", Anchored: true, FirstText: strPtr("2025-2-5 08:6:2:7")},
	},
	{
		// MST is one of the zone words that name no single offset, so the
		// lines stay zone-less, as the real file's do.
		name:  "postgresql zone word and tab-led continuation lines",
		first: time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC),
		line: func(t time.Time) string {
			return t.Format("2006-01-02 15:04:05") + " MST [4242]: LOG:  duration: 1.2 ms"
		},
		ms:           secondMs,
		continuation: "\t->  Seq Scan on accounts  (cost=0.00..1.01 rows=1 width=4)",
		want:         rxtypes.TimeIndex{Format: "iso", Anchored: true, FirstText: strPtr("2025-12-10 07:00:00")},
	},
	{
		name:  "core syslog without a year",
		first: time.Date(2025, 12, 10, 7, 49, 50, 123e6, time.UTC),
		line:  func(t time.Time) string { return t.Format("Jan _2 15:04:05.000") + " host app[17]: handled" },
		ms:    wallMs,
		want:  rxtypes.TimeIndex{Format: "syslog", Anchored: true, YearFromMtime: true, FirstText: strPtr("Dec 10 07:49:50.123")},
	},
	{
		name:         "python logging with a comma before the milliseconds",
		first:        time.Date(2026, 1, 6, 9, 15, 0, 250e6, time.UTC),
		line:         func(t time.Time) string { return t.Format("2006-01-02 15:04:05,000") + " - app - INFO - step done" },
		ms:           wallMs,
		continuation: "Traceback (most recent call last):",
		want:         rxtypes.TimeIndex{Format: "iso", Anchored: true, FirstText: strPtr("2026-01-06 09:15:00,250")},
	},
	{
		name:  "iso with a numeric zone",
		first: time.Date(2026, 10, 6, 10, 34, 56, 123e6, time.UTC),
		line:  func(t time.Time) string { return t.In(plus2).Format("2006-01-02T15:04:05.000-07:00") + " job ran" },
		ms:    wallMs,
		want: rxtypes.TimeIndex{Format: "iso", Anchored: true, HasZone: true, FirstZoneOffsetMinutes: intPtr(120),
			FirstText: strPtr("2026-10-06T12:34:56.123+02:00")},
	},
	{
		name:  "access log with the timestamp inside the line",
		first: time.Date(2025, 12, 10, 7, 0, 4, 0, time.UTC),
		line: func(t time.Time) string {
			return `10.0.0.7 - - [` + t.Format("02/Jan/2006:15:04:05 -0700") + `] "GET /health HTTP/1.1" 200 12`
		},
		ms: secondMs,
		want: rxtypes.TimeIndex{Format: "clf", HasZone: true, FirstZoneOffsetMinutes: intPtr(0),
			FirstText: strPtr("[10/Dec/2025:07:00:04 +0000]")},
	},
}

// linesOf generates 60 timestamped lines of a shape, with its
// continuation line after every fifth.
func linesOf(shape logShape) []timedLine {
	var lines []timedLine
	at := shape.first
	for i := 1; i <= 60; i++ {
		write := shape.line
		if shape.other != nil && i%5 == 0 {
			write = shape.other
		}
		lines = append(lines, timedLine{text: write(at), ms: shape.ms(at), has: true})
		if shape.continuation != "" && i%5 == 0 {
			lines = append(lines, timedLine{text: shape.continuation})
		}
		at = at.Add(1500 * time.Millisecond)
	}
	return lines
}

// Every build records the time section of a timestamped log: its
// format, the first and last timestamped line, how many lines carry a
// timestamp, and the latest timestamp before each checkpoint.
func TestBuild_RecordsTheTimeSectionOfEachLogShape(t *testing.T) {
	for _, shape := range logShapes {
		t.Run(shape.name, func(t *testing.T) {
			lines := linesOf(shape)
			idx := buildAt(t, t.TempDir(), "app.log", writePlain, joinLines(lines, true), fileMtime, 256)
			if idx.TimeIndex == nil {
				t.Fatal("time_index is null for a timestamped log")
			}
			want := expectedTimeIndex(lines, shape.want)
			if got := withoutMaxBefore(idx.TimeIndex); !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Fatalf("time_index\n got %s\nwant %s", gotJSON, wantJSON)
			}
			if len(idx.LineIndex) < 3 {
				t.Fatalf("fixture gives %d checkpoints; want several", len(idx.LineIndex))
			}
			requireMaxBeforeMatches(t, "plain", idx, lines)
		})
	}
}

// A line counts as a backward step when its timestamp is more than one
// second below the latest timestamp before it, the running maximum,
// not the line just above it.
func TestBuild_CountsBackwardStepsAgainstTheRunningMaximum(t *testing.T) {
	cases := []struct {
		name      string
		offsets   []int64 // milliseconds after the first line
		wantSteps int64
		wantMax   int64
	}{
		{"in order", []int64{0, 1000, 2000, 3000}, 0, 0},
		{"back 2 s then 0.5 s", []int64{0, 10_000, 8_000, 9_500, 11_000}, 1, 2000},
		{"back exactly 1 s is not a step", []int64{0, 10_000, 9_000, 10_500}, 0, 0},
		{"each line a little below the maximum", []int64{0, 10_000, 9_500, 9_200, 8_900}, 1, 1100},
		{"two steps, the larger kept", []int64{0, 10_000, 7_000, 12_000, 10_500}, 2, 3000},
	}
	base := time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []timedLine
			for _, off := range tc.offsets {
				at := base.Add(time.Duration(off) * time.Millisecond)
				lines = append(lines, timedLine{text: at.Format("2006-01-02 15:04:05.000") + " event", ms: at.UnixMilli(), has: true})
			}
			idx := buildAt(t, t.TempDir(), "app.log", writePlain, joinLines(lines, true), fileMtime, 0)
			if idx.TimeIndex == nil {
				t.Fatal("time_index is null")
			}
			if idx.TimeIndex.BackwardSteps != tc.wantSteps || idx.TimeIndex.MaxBackwardMs != tc.wantMax {
				t.Errorf("backward_steps %d, max_backward_ms %d; want %d, %d",
					idx.TimeIndex.BackwardSteps, idx.TimeIndex.MaxBackwardMs, tc.wantSteps, tc.wantMax)
			}
		})
	}
}

// A year-less timestamp takes its year from the file's mtime: a log
// last written in January holds the December lines of the year before.
func TestBuild_YearlessLinesTakeTheirYearFromTheMtime(t *testing.T) {
	text := []byte("Dec 31 23:59:58 host app: a\nDec 31 23:59:59 host app: b\nJan  1 00:00:01 host app: c\n")
	mtime := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	idx := buildAt(t, t.TempDir(), "syslog", writePlain, text, mtime, 0)
	if idx.TimeIndex == nil {
		t.Fatal("time_index is null")
	}
	wantFirst := time.Date(2025, 12, 31, 23, 59, 58, 0, time.UTC).UnixMilli()
	wantLast := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC).UnixMilli()
	ti := idx.TimeIndex
	if !ti.YearFromMtime || ti.First.Ms != wantFirst || ti.Last.Ms != wantLast {
		t.Errorf("year_from_mtime %v, first %d, last %d; want true, %d, %d",
			ti.YearFromMtime, ti.First.Ms, ti.Last.Ms, wantFirst, wantLast)
	}
	if ti.BackwardSteps != 0 {
		t.Errorf("backward_steps %d across New Year; want 0", ti.BackwardSteps)
	}
}

// A file with no timestamp format has no time section, and its index
// says so with an explicit null.
func TestBuild_TimeIndexIsNullWithoutTimestamps(t *testing.T) {
	idx := buildAt(t, t.TempDir(), "app.log", writePlain, numberedText(500, "no time here"), fileMtime, 256)
	if idx.TimeIndex != nil {
		t.Fatalf("time_index = %+v; want null", idx.TimeIndex)
	}
	body, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte(`"time_index":null`)) {
		t.Errorf("JSON has no \"time_index\":null: %s", body)
	}
}

// randomTimedLog is a log with jitter, gaps and lines without a
// timestamp, including continuation lines longer than a small frame.
func randomTimedLog(rng *rand.Rand) []timedLine {
	base := time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC).UnixMilli()
	n := 1 + rng.IntN(600)
	var lines []timedLine
	clock := base
	for i := 0; i < n; i++ {
		switch r := rng.IntN(100); {
		case r < 20:
			lines = append(lines, timedLine{text: "\tat com.example.Service.run(Service.java:10)"})
		case r < 23:
			lines = append(lines, timedLine{text: "continued " + strings.Repeat("x", 300+rng.IntN(400))})
		default:
			clock += int64(rng.IntN(800))
			if rng.IntN(50) == 0 {
				clock += int64(rng.IntN(3)-1) * 3_600_000 // a jump of an hour either way
			}
			ms := clock + int64(rng.IntN(5000)) - 2500 // apps a few seconds apart
			at := time.UnixMilli(ms).UTC()
			lines = append(lines, timedLine{text: at.Format("2006-01-02 15:04:05.000") + " INFO served", ms: ms, has: true})
		}
	}
	return lines
}

// firstStampText is the first width bytes of the first timestamped
// line of lines, where a generator that starts each such line with its
// timestamp writes it, or nil when no line has one.
func firstStampText(lines []timedLine, width int) *string {
	for _, l := range lines {
		if l.has {
			text := l.text[:width]
			return &text
		}
	}
	return nil
}

// randomCuts returns frame boundaries for text: mid-line cuts at random
// spacing, some repeated (an empty frame), and sometimes a cut at the
// very end.
func randomCuts(rng *rand.Rand, size int) []int {
	var cuts []int
	for at := 1 + rng.IntN(200); at < size; at += 1 + rng.IntN(400) {
		cuts = append(cuts, at)
		if rng.IntN(8) == 0 {
			cuts = append(cuts, at)
		}
	}
	if rng.IntN(4) == 0 {
		cuts = append(cuts, size)
	}
	return cuts
}

// max_before at every checkpoint is the brute-force running maximum,
// for plain, gzip and seekable copies of one log. A seekable file has
// checkpoints of its own (each frame, and every 10,000 lines inside a
// frame), so its max_before follows those; everything else in the
// time section is the same for the three copies.
func TestBuild_MaxBeforeIsTheRunningMaximumForEveryStorage(t *testing.T) {
	for seed := uint64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewPCG(seed, 123))
		lines := randomTimedLog(rng)
		text := joinLines(lines, rng.IntN(2) == 0)
		step := int64(32 + rng.IntN(2048))
		dir := t.TempDir()

		plain := buildAt(t, dir, "app.log", writePlain, text, fileMtime, step)
		gz := buildAt(t, dir, "app.log.gz", writeGzip, text, fileMtime, step)
		frames := seekablefile.SplitAt(text, randomCuts(rng, len(text))...)
		sk := buildAt(t, dir, "app.zst", func(t *testing.T, path string, _ []byte) {
			seekablefile.Write(t, path, frames)
		}, text, fileMtime, step)

		label := fmt.Sprintf("seed %d (%d lines, %d frames)", seed, len(lines), len(frames))
		if plain.TimeIndex == nil {
			if gz.TimeIndex != nil || sk.TimeIndex != nil {
				t.Fatalf("%s: plain has no time_index, gzip %v, seekable %v", label, gz.TimeIndex, sk.TimeIndex)
			}
			continue
		}
		requireMaxBeforeMatches(t, label+" plain", plain, lines)
		requireMaxBeforeMatches(t, label+" seekable", sk, lines)
		if !reflect.DeepEqual(gz.TimeIndex, plain.TimeIndex) {
			t.Fatalf("%s: gzip time_index differs from plain", label)
		}
		if gotSk, want := withoutMaxBefore(sk.TimeIndex), withoutMaxBefore(plain.TimeIndex); !reflect.DeepEqual(gotSk, want) {
			t.Fatalf("%s: seekable time_index %+v, plain %+v", label, gotSk, want)
		}
		base := rxtypes.TimeIndex{Format: "iso", Anchored: true, FirstText: firstStampText(lines, len("2006-01-02 15:04:05.000"))}
		if want := expectedTimeIndex(lines, base); !reflect.DeepEqual(withoutMaxBefore(plain.TimeIndex), want) {
			t.Fatalf("%s: time_index %+v, want %+v", label, withoutMaxBefore(plain.TimeIndex), want)
		}
	}
}

// A seekable frame holding more than 10,000 lines gets checkpoints
// inside it, and each carries the running maximum at its own line,
// whether the frame ends at a line break or in the middle of a line.
func TestBuild_SeekableInteriorCheckpointsCarryTheirMaximum(t *testing.T) {
	var lines []timedLine
	at := time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC)
	for i := 0; i < 25_000; i++ {
		if i%3 == 2 {
			lines = append(lines, timedLine{text: "\tcontinued"})
			continue
		}
		lines = append(lines, timedLine{text: at.Format("2006-01-02 15:04:05.000") + " x", ms: at.UnixMilli(), has: true})
		at = at.Add(7 * time.Millisecond)
	}
	text := joinLines(lines, true)
	startOf := func(line int) int { return int(pointAt(lines, line).Offset) }
	layouts := map[string][][]byte{
		"one frame":                  seekablefile.SplitAt(text),
		"cut after line 10001 began": seekablefile.SplitAt(text, startOf(10001)+5, startOf(22000)),
		"cut at a line start":        seekablefile.SplitAt(text, startOf(12345)),
	}
	for name, frames := range layouts {
		t.Run(name, func(t *testing.T) {
			idx := buildAt(t, t.TempDir(), "app.zst", func(t *testing.T, path string, _ []byte) {
				seekablefile.Write(t, path, frames)
			}, text, fileMtime, 0)
			interior := 0
			for _, cp := range idx.LineIndex {
				if cp.FrameIndex != nil && cp.LineNumber > 1 && cp.ByteOffset != frameStart(frames, *cp.FrameIndex) {
					interior++
				}
			}
			if interior == 0 {
				t.Fatalf("no interior checkpoint in %v", idx.LineIndex)
			}
			requireMaxBeforeMatches(t, name, idx, lines)
		})
	}
}

// frameStart is the offset in the text where frame i begins.
func frameStart(frames [][]byte, i int) int64 {
	var at int64
	for _, f := range frames[:i] {
		at += int64(len(f))
	}
	return at
}

// A seekable file built without analysis records its time section and
// still leaves out the line-length statistics, as it always has.
func TestBuild_SeekableWithoutAnalysisRecordsOnlyTheTimeSection(t *testing.T) {
	lines := linesOf(logShapes[0])
	idx := buildAt(t, t.TempDir(), "app.zst", writeSeekable, joinLines(lines, true), fileMtime, 0)
	if idx.TimeIndex == nil {
		t.Fatal("time_index is null for a timestamped seekable file")
	}
	if idx.LineLengthMax != nil || idx.LineEnding != nil {
		t.Errorf("line_length_max %v, line_ending %v; want null without analysis", idx.LineLengthMax, idx.LineEnding)
	}
}

// DetectTimeFormat decides from the first mebibyte of the text, and
// reads no more of a plain file than that.
func TestDetectTimeFormat_ReadsOnlyTheHead(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC)
	for buf.Len() < 4*timestamps.SampleBytes {
		fmt.Fprintf(&buf, "%s INFO request served\n", at.Format("2006-01-02 15:04:05.000"))
		at = at.Add(time.Millisecond)
	}
	text := buf.Bytes()
	counter := counting.NewReaderAt(bytes.NewReader(text))
	kind := filekind.Of(bytes.NewReader(text), int64(len(text)))

	format, ok, err := DetectTimeFormat(counter, int64(len(text)), kind)
	if err != nil || !ok {
		t.Fatalf("DetectTimeFormat = %+v, %v, %v; want a format", format, ok, err)
	}
	want, _ := timestamps.Detect(text[:timestamps.SampleBytes])
	if !reflect.DeepEqual(format, want) {
		t.Errorf("format %+v, want %+v", format, want)
	}
	if got := counter.Load(); got > timestamps.SampleBytes {
		t.Errorf("read %d bytes; budget %d", got, timestamps.SampleBytes)
	}
}

// A time section survives Save and Load unchanged, for a plain file
// and for a seekable one whose frames give several checkpoints on one
// line, and it is the format version this build writes.
func TestSaveLoad_KeepsTheTimeSection(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	lines := linesOf(logShapes[2])
	text := joinLines(lines, true)
	for name, write := range map[string]func(*testing.T, string, []byte){
		"app.log": writePlain,
		"app.zst": func(t *testing.T, path string, text []byte) {
			seekablefile.Write(t, path, seekablefile.SplitEvery(text, 23))
		},
	} {
		t.Run(name, func(t *testing.T) {
			built := buildAt(t, t.TempDir(), name, write, text, fileMtime, 128)
			if built.TimeIndex == nil || built.Version != 8 {
				t.Fatalf("version %d, time section present %v; want 8 and true", built.Version, built.TimeIndex != nil)
			}
			cachePath, err := Save(built)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			loaded, err := LoadFromPath(cachePath)
			if err != nil {
				t.Fatalf("LoadFromPath: %v", err)
			}
			if !reflect.DeepEqual(loaded.TimeIndex, built.TimeIndex) {
				t.Errorf("loaded time_index %+v, built %+v", loaded.TimeIndex, built.TimeIndex)
			}
		})
	}
}

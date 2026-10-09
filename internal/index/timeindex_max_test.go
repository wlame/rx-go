package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// maxBase is the moment the generated logs of these tests start at.
var maxBase = time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC)

// isoAt is a zone-less ISO line with milliseconds written d after maxBase,
// followed by rest.
func isoAt(d time.Duration, rest string) timedLine {
	at := maxBase.Add(d)
	return timedLine{text: at.Format("2006-01-02 15:04:05.000") + " " + rest, ms: at.UnixMilli(), has: true}
}

// stackLine is a line without an own timestamp.
var stackLine = timedLine{text: "\tat com.example.Service.run(Service.java:10)"}

// max is the highest own timestamp of the file and the first line that
// holds it, whatever order the lines are in; last stays the last line
// with a timestamp.
func TestBuild_MaxIsTheHighestTimestampAndTheFirstLineThatHasIt(t *testing.T) {
	cases := []struct {
		name     string
		lines    []timedLine
		wantMax  int // the 1-based line max names
		wantLast int // the 1-based line last names
	}{
		{
			name: "lines in order: max is the last line",
			lines: []timedLine{
				isoAt(0, "a"), isoAt(time.Second, "b"), stackLine,
				isoAt(2*time.Second, "c"), isoAt(3*time.Second, "d"),
			},
			wantMax: 5, wantLast: 5,
		},
		{
			name: "highest in the middle, later lines hours back",
			lines: []timedLine{
				isoAt(0, "a"), isoAt(time.Second, "b"), isoAt(5*time.Hour, "peak"), stackLine,
				isoAt(2*time.Hour, "c"), isoAt(3*time.Hour, "d"),
			},
			wantMax: 3, wantLast: 6,
		},
		{
			name: "two lines share the highest value: the first of them",
			lines: []timedLine{
				isoAt(0, "a"), isoAt(9*time.Second, "first peak"), isoAt(time.Second, "b"),
				isoAt(9*time.Second, "second peak"), isoAt(2*time.Second, "c"),
			},
			wantMax: 2, wantLast: 5,
		},
		{
			name: "highest on the first line",
			lines: []timedLine{
				isoAt(time.Hour, "peak"), isoAt(0, "a"), stackLine, isoAt(time.Second, "b"),
			},
			wantMax: 1, wantLast: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := buildAt(t, t.TempDir(), "app.log", writePlain, joinLines(tc.lines, true), fileMtime, 64)
			ti := idx.TimeIndex
			if ti == nil {
				t.Fatal("time_index is null for a timestamped log")
			}
			if want := pointAt(tc.lines, tc.wantMax); !reflect.DeepEqual(ti.Max, want) {
				t.Errorf("max = %+v; want line %d: %+v", ti.Max, tc.wantMax, want)
			}
			if want := pointAt(tc.lines, tc.wantLast); !reflect.DeepEqual(ti.Last, want) {
				t.Errorf("last = %+v; want line %d: %+v", ti.Last, tc.wantLast, want)
			}
		})
	}
}

// The lines after the last checkpoint count too: no max_before entry
// covers them, and max is the one value that does.
func TestBuild_MaxFindsAHighestLineAfterTheLastCheckpoint(t *testing.T) {
	// 40 lines of 32 bytes each with a step of 256 bytes put checkpoints
	// on lines 1, 9, 17, 25 and 33; line 36 is the highest, and the
	// lines after it go back.
	var lines []timedLine
	for i := 1; i <= 40; i++ {
		d := time.Duration(i) * time.Second
		if i == 36 {
			d = 10 * time.Hour
		}
		lines = append(lines, isoAt(d, fmt.Sprintf("LINE%3d", i)))
	}
	text := joinLines(lines, true)
	if len(text) != 40*32 {
		t.Fatalf("fixture is %d bytes; the checkpoint layout assumes %d", len(text), 40*32)
	}
	idx := buildAt(t, t.TempDir(), "app.log", writePlain, text, fileMtime, 256)
	ti := idx.TimeIndex
	if ti == nil {
		t.Fatal("time_index is null")
	}
	lastCheckpoint := idx.LineIndex[len(idx.LineIndex)-1].LineNumber
	if lastCheckpoint >= 36 {
		t.Fatalf("fixture: last checkpoint names line %d; want one before line 36 (%v)", lastCheckpoint, idx.LineIndex)
	}
	if want := pointAt(lines, 36); !reflect.DeepEqual(ti.Max, want) {
		t.Fatalf("max = %+v; want line 36: %+v", ti.Max, want)
	}
	lastMaxBefore := ti.MaxBefore[len(ti.MaxBefore)-1]
	if lastMaxBefore == nil || ti.Max.Ms <= *lastMaxBefore {
		t.Errorf("max %d ms is not above the last max_before entry %s", ti.Max.Ms, msText(lastMaxBefore))
	}
}

// max is null when no line has an own timestamp: a time section with
// none says "max": null, and a file with no timestamp format, or no
// text at all, has no time section.
func TestBuild_MaxIsNullWithoutTimestampedLines(t *testing.T) {
	indexer, err := newTimeIndexer(timestamps.Format{Family: timestamps.FamilyISO, Anchored: true}, 0)
	if err != nil {
		t.Fatalf("newTimeIndexer: %v", err)
	}
	// The walk marks the checkpoint [1, 0] before it reads line 1.
	indexer.markBefore(1)
	indexer.observe([]byte(stackLine.text), 1, 0, int64(len(stackLine.text))+1)
	section, err := indexer.result([]rxtypes.LineIndexEntry{{LineNumber: 1, ByteOffset: 0}})
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if section.Max != nil {
		t.Errorf("max = %+v for a section without timestamped lines; want null", section.Max)
	}
	body, err := json.Marshal(section)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte(`"max":null`)) {
		t.Errorf("JSON has no \"max\":null: %s", body)
	}

	for name, text := range map[string][]byte{
		"no timestamp format": numberedText(50, "no time here"),
		"empty file":          {},
	} {
		t.Run(name, func(t *testing.T) {
			idx := buildAt(t, t.TempDir(), "app.log", writePlain, text, fileMtime, 64)
			if idx.TimeIndex != nil {
				t.Errorf("time_index = %+v; want null", idx.TimeIndex)
			}
		})
	}
}

// Every way rx stores a log gives the same max: plain, gzip, bzip2, xz,
// zstd and seekable zstd, for a text whose highest line is in the
// middle and whose last line has no line break after it.
func TestBuild_MaxIsTheSameForEveryStorage(t *testing.T) {
	var lines []timedLine
	for i := 1; i <= 300; i++ {
		switch {
		case i == 137:
			lines = append(lines, isoAt(7*time.Hour, "peak"))
		case i%7 == 0:
			lines = append(lines, stackLine)
		default:
			lines = append(lines, isoAt(time.Duration(i)*time.Second, "request served"))
		}
	}
	text := joinLines(lines, false)
	dir := t.TempDir()
	plain := buildAt(t, dir, "app.log", writePlain, text, fileMtime, 512)
	if want := pointAt(lines, 137); plain.TimeIndex == nil || !reflect.DeepEqual(plain.TimeIndex.Max, want) {
		t.Fatalf("plain max = %+v; want line 137: %+v", plain.TimeIndex, want)
	}
	formats := map[string]string{
		compressedcopy.Gzip:         "app.log.gz",
		compressedcopy.Bzip2:        "app.log.bz2",
		compressedcopy.Xz:           "app.log.xz",
		compressedcopy.Zstd:         "app.log.zst",
		compressedcopy.SeekableZstd: "app.seekable.zst",
	}
	for format, name := range formats {
		t.Run(format, func(t *testing.T) {
			body := compressedcopy.Encode(t, format, text)
			if body == nil {
				t.Skipf("no encoder for %s on this host", format)
			}
			idx := buildAt(t, dir, name, func(t *testing.T, path string, _ []byte) {
				writePlain(t, path, body)
			}, text, fileMtime, 512)
			if idx.TimeIndex == nil {
				t.Fatal("time_index is null")
			}
			// max_before follows each storage's checkpoints; every other
			// member, max included, is the plain file's.
			if got, want := withoutMaxBefore(idx.TimeIndex), withoutMaxBefore(plain.TimeIndex); !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Errorf("time_index\n got %s\nwant %s", gotJSON, wantJSON)
			}
			if err := validTimeIndex(idx); err != nil {
				t.Errorf("the built index fails the load check: %v", err)
			}
		})
	}
}

// max is in the frame first and last are in: the UTC instant in a file
// whose timestamps carry zones, and the wall clock a line shows in one
// whose timestamps carry none (a line there that writes a zone keeps the
// wall clock it shows). The index applies no zone of its own; a file
// zone given to a request is applied when the request reads it.
func TestBuild_MaxIsInTheFrameOfFirstAndLast(t *testing.T) {
	plus2 := time.FixedZone("", 2*60*60)
	zoned := func(at time.Time, zone *time.Location) timedLine {
		return timedLine{text: at.In(zone).Format("2006-01-02T15:04:05.000-07:00") + " job ran", ms: at.UnixMilli(), has: true}
	}
	// Lines written at +02:00 show 12:00 and later; one written at
	// +00:00 shows 11:00 but is the latest instant (11:00Z, while the
	// others are 10:00Z and a little after).
	var zonedLines []timedLine
	for i := 0; i < 20; i++ {
		zonedLines = append(zonedLines, zoned(maxBase.Add(3*time.Hour+time.Duration(i)*time.Second), plus2))
	}
	zonedLines = append(zonedLines, zoned(maxBase.Add(4*time.Hour), time.UTC))
	for i := 0; i < 10; i++ {
		zonedLines = append(zonedLines, zoned(maxBase.Add(3*time.Hour+time.Duration(30+i)*time.Second), plus2))
	}

	// A zone-less file with one line that writes -07:00: as an instant
	// it would be the latest (14:59Z), but it shows 07:59 and counts as
	// that, below the 08:00 lines around it.
	var walls []timedLine
	for i := 0; i < 20; i++ {
		walls = append(walls, isoAt(time.Hour+time.Duration(i)*time.Second, "INFO served"))
	}
	gcAt := maxBase.Add(59 * time.Minute)
	walls = append(walls, timedLine{
		text: "[" + gcAt.Format("2006-01-02T15:04:05.000") + "-0700][812.406s][info][gc] Pause Young",
		ms:   gcAt.UnixMilli(), has: true,
	})
	walls = append(walls, isoAt(time.Hour+30*time.Second, "INFO served"))

	cases := []struct {
		name    string
		lines   []timedLine
		hasZone bool
		wantMax int
	}{
		{"zoned file: the latest instant", zonedLines, true, 21},
		{"zone-less file: the latest wall clock", walls, false, 22},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := buildAt(t, t.TempDir(), "app.log", writePlain, joinLines(tc.lines, true), fileMtime, 256)
			ti := idx.TimeIndex
			if ti == nil || ti.HasZone != tc.hasZone {
				t.Fatalf("time_index = %+v; want a section with has_zone %v", ti, tc.hasZone)
			}
			if want := pointAt(tc.lines, tc.wantMax); !reflect.DeepEqual(ti.Max, want) {
				t.Errorf("max = %+v; want line %d: %+v", ti.Max, tc.wantMax, want)
			}
		})
	}
}

// An index stored by format 9 has a time section without max. It is an
// index of another version: absent, not worth a warning, and the next
// build writes one with max in its place.
func TestLoadFromPath_AFormat9IndexIsAbsent(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	built := buildAt(t, dir, "app.log", writePlain, joinLines(linesOf(logShapes[0]), true), fileMtime, 256)
	source := filepath.Join(dir, "app.log")
	cachePath, err := Save(built)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	doc["version"] = 9
	delete(doc["time_index"].(map[string]any), "max")
	if body, err = json.Marshal(doc); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatalf("write format 9 index: %v", err)
	}
	log := captureIndexLog(t)

	if idx, err := LoadFromPath(cachePath); idx != nil || !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("LoadFromPath = %v, %v; want no index and ErrIndexNotFound", idx, err)
	}
	if idx, err := LoadForSource(source); idx != nil || !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("LoadForSource = %v, %v; want no index and ErrIndexNotFound", idx, err)
	}
	if strings.Contains(log.String(), "index_unreadable") {
		t.Errorf("a format 9 index was logged as damaged:\n%s", log.String())
	}

	rebuilt, err := Build(source, BuildOptions{StepBytes: 256})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rebuilt.Version != Version || rebuilt.TimeIndex == nil || rebuilt.TimeIndex.Max == nil {
		t.Errorf("rebuilt index: version %d, time section %+v; want version %d with max", rebuilt.Version, rebuilt.TimeIndex, Version)
	}
}

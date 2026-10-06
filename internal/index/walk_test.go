package index

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/timestamps"
)

// A line longer than the walk's read buffer arrives in pieces and is
// joined; the lines around it, and a second long line of another length
// right after it, must come out whole and in their own places. The text
// mixes short timestamped lines with lines of 600, 300 and 270 KiB, the
// last one without a final newline.
func TestWalkLines_LongLinesAreJoinedAndNumbered(t *testing.T) {
	at := time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC)
	stamp := func(i int) string {
		return at.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05.000") + " INFO short"
	}
	lines := []string{
		stamp(0), stamp(1),
		"x" + strings.Repeat("a", 600<<10),
		stamp(2),
		"y" + strings.Repeat("b", 300<<10),
		"z" + strings.Repeat("c", 280<<10),
		stamp(3), stamp(4),
		"w" + strings.Repeat("d", 270<<10),
	}
	text := []byte(strings.Join(lines, "\n"))

	ti, err := newTimeIndexer(timestamps.Format{Family: timestamps.FamilyISO, Anchored: true}, 0)
	if err != nil {
		t.Fatalf("newTimeIndexer: %v", err)
	}
	const step = 64 << 10
	stats, err := walkLines(bytes.NewReader(text), step, nil, ti)
	if err != nil {
		t.Fatalf("walkLines: %v", err)
	}

	if stats.LineCount != int64(len(lines)) || stats.TotalBytes != int64(len(text)) {
		t.Fatalf("walk read %d lines, %d bytes; want %d, %d", stats.LineCount, stats.TotalBytes, len(lines), len(text))
	}
	if want := 600<<10 + 1; stats.LineStats.Max != want {
		t.Errorf("longest line %d bytes, want %d", stats.LineStats.Max, want)
	}
	starts := make([]int64, len(lines))
	for i := 1; i < len(lines); i++ {
		starts[i] = starts[i-1] + int64(len(lines[i-1])) + 1
	}
	for _, cp := range stats.LineIndex {
		if cp.ByteOffset != starts[cp.LineNumber-1] {
			t.Errorf("checkpoint %+v: line %d starts at %d", cp, cp.LineNumber, starts[cp.LineNumber-1])
		}
	}
	// Line 1, then the line after each long line that has one after it.
	var numbered []int64
	for _, cp := range stats.LineIndex {
		numbered = append(numbered, cp.LineNumber)
	}
	if want := []int64{1, 4, 6, 7}; !slices.Equal(numbered, want) {
		t.Errorf("checkpoints name lines %v; want %v", numbered, want)
	}
	section, err := ti.result(stats.LineIndex)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if section.TimestampedLines != 5 || section.First.Line != 1 || section.Last.Line != 8 || section.Last.Offset != starts[7] {
		t.Errorf("time section counts %d lines, first line %d, last line %d at %d; want 5, 1, 8 at %d",
			section.TimestampedLines, section.First.Line, section.Last.Line, section.Last.Offset, starts[7])
	}
}

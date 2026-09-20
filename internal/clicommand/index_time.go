package clicommand

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Labels for the time section in `rx index --info`, keyed by the field
// they describe.
var (
	timePlacementLabel = map[bool]string{
		true:  "at the start of each line",
		false: "inside the line",
	}
	dayOrderLabel = map[bool]string{
		true:  "day first",
		false: "month first",
	}
	// timestampLayout prints a value of the time section: an instant in
	// UTC for a file whose timestamps carry zones, and the wall-clock
	// reading, with no zone, for a file whose timestamps carry none.
	timestampLayout = map[bool]string{
		true:  "2006-01-02T15:04:05.000Z",
		false: "2006-01-02 15:04:05.000",
	}
)

// writeTimeIndexHuman prints the time section of an index for
// `rx index --info`: the format, the first and last timestamps with
// their lines, the count of timestamped lines and the backward steps.
func writeTimeIndexHuman(out io.Writer, ti *rxtypes.TimeIndex) {
	if ti == nil {
		_, _ = fmt.Fprintln(out, "  time_format: none recognized")
		return
	}
	_, _ = fmt.Fprintf(out, "  time_format: %s\n", describeTimeFormat(ti))
	if ti.First != nil {
		_, _ = fmt.Fprintf(out, "  first_timestamp: %s (line %d)\n", timestampText(ti, ti.First.Ms), ti.First.Line)
	}
	if ti.Last != nil {
		_, _ = fmt.Fprintf(out, "  last_timestamp: %s (line %d)\n", timestampText(ti, ti.Last.Ms), ti.Last.Line)
	}
	_, _ = fmt.Fprintf(out, "  timestamped_lines: %d\n", ti.TimestampedLines)
	if ti.BackwardSteps == 0 {
		_, _ = fmt.Fprintln(out, "  backward_steps: 0")
		return
	}
	_, _ = fmt.Fprintf(out, "  backward_steps: %d (largest %d ms)\n", ti.BackwardSteps, ti.MaxBackwardMs)
}

// describeTimeFormat words a detected format: its family, the day
// order of a slash date, where the timestamp sits, how its zone is
// read and, for a format without a year, where the year comes from.
func describeTimeFormat(ti *rxtypes.TimeIndex) string {
	parts := []string{ti.Format}
	if ti.DayFirst != nil {
		parts = append(parts, dayOrderLabel[*ti.DayFirst])
	}
	parts = append(parts, timePlacementLabel[ti.Anchored], zoneNote(ti))
	if ti.YearFromMtime {
		parts = append(parts, "year from the file's mtime")
	}
	return strings.Join(parts, ", ")
}

// zoneNote says how the file's timestamps are read: as UTC when they
// carry no zone, or with their zones, naming the first one.
func zoneNote(ti *rxtypes.TimeIndex) string {
	if !ti.HasZone {
		return "no zone, read as UTC"
	}
	if ti.FirstZoneOffsetMinutes == nil {
		return "with zones"
	}
	return fmt.Sprintf("with zones (first %s)", zoneOffsetText(*ti.FirstZoneOffsetMinutes))
}

// zoneOffsetText writes a zone offset east of UTC as ±HH:MM.
func zoneOffsetText(minutes int) string {
	sign := "+"
	if minutes < 0 {
		sign, minutes = "-", -minutes
	}
	return fmt.Sprintf("%s%02d:%02d", sign, minutes/60, minutes%60)
}

// timestampText prints one value of the time section in its frame.
func timestampText(ti *rxtypes.TimeIndex, ms int64) string {
	return time.UnixMilli(ms).UTC().Format(timestampLayout[ti.HasZone])
}

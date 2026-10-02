package samples

import (
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// StoredTimes turns the time values stored for a file's lines into the
// UTC instants an answer about the file gives: the values of
// line_timestamps and of TimeRange under the same file zone.
//
// A line index and a signature scan store each line's time in the
// file's own frame (milliseconds since the epoch: the instant for a file
// whose timestamps carry zones, the wall clock read as UTC for one whose
// timestamps carry none). An answer reads them in the frame its request
// asks for (frameIn): without a file zone, a zone-less file's wall
// clocks are read in RX_LOG_TZ and a zoned file's instants stay as they
// are; under a file zone every line's written wall clock is read in that
// zone. The second needs, for a file whose timestamps carry zones, the
// offset each line writes, which only a line index that records where
// it changes (zone_offsets) gives without reading the file again.
type StoredTimes struct {
	frame timeFrame
	// segments turn a stored value into the frame's (segmentsToFrame):
	// storedFrame when the frame is the file's own, the line index's
	// zone offsets under a file zone on a zoned file. Nil when they are
	// not known: then no value can be turned.
	segments []zoneSegment
}

// StoredTimesRequest names the file whose stored times to turn and how
// the answer reads them.
type StoredTimesRequest struct {
	// Path is the file as the caller named it, and Source its pin, which
	// every read goes through; Kind is what the file is.
	Path   string
	Source paths.Pinned
	Kind   filekind.Kind
	// IndexLoader gives the file's line index, or none (nil, NoIndex).
	IndexLoader IndexLoader
	// FileZone, when set, reads every line's written wall clock in this
	// zone (Request.FileZone, the file_tz of a request); a zone a line
	// writes is ignored.
	FileZone *time.Location
	// LogZone is the zone a zone-less file's wall clocks are read in
	// when FileZone is nil (RX_LOG_TZ); nil means UTC.
	LogZone *time.Location
}

// StoredTimesOf returns the StoredTimes of req's file, or nil for a file
// without a timestamp format.
//
// The format comes from the file's line index when req.IndexLoader
// gives one (which detected it the same way), and is otherwise detected
// from the head of the text, at most timestamps.SampleBytes read through
// the pin, as timesOf does. The zone offsets come only from the index.
func StoredTimesOf(req StoredTimesRequest) (*StoredTimes, error) {
	lookup := Request{Path: req.Path, Source: req.Source, IndexLoader: req.IndexLoader}
	var (
		detected timestamps.Format
		section  *rxtypes.TimeIndex
	)
	if idx := loadIndexOrNone(lookup); idx != nil {
		if idx.TimeIndex == nil {
			return nil, nil
		}
		section = idx.TimeIndex
		detected = formatOfSection(section)
	} else {
		format, ok, err := detectTimeFormat(req.Source, req.Kind)
		if err != nil || !ok {
			return nil, err
		}
		detected = format
	}
	fileZone := config.Zone{}
	if req.FileZone != nil {
		fileZone = config.Zone{Name: req.FileZone.String(), Location: req.FileZone}
	}
	logZone := config.Zone{Name: "UTC", Location: time.UTC}
	if req.LogZone != nil {
		logZone = config.Zone{Name: req.LogZone.String(), Location: req.LogZone}
	}
	times := &StoredTimes{frame: frameIn(fileZone, detected, logZone)}
	switch {
	case times.frame.readsStoredFrame(detected):
		times.segments = storedFrame
	case section != nil:
		if segments, ok := segmentsToFrame(times.frame, detected, section); ok {
			times.segments = segments
		}
	}
	return times, nil
}

// Instant returns the UTC instant of stored, the value kept for line
// (the line whose own timestamp gave it, or any later line up to the
// next timestamped one: they share its offset), and false when it
// cannot be given: under a file zone, for a file whose timestamps carry
// zones and whose index does not record where their offset changes, or
// when the instant falls outside the years 1 to 9999.
func (t *StoredTimes) Instant(line, stored int64) (int64, bool) {
	if t.segments == nil {
		return 0, false
	}
	return t.frame.instant(frameValueAt(t.segments, line, stored))
}

// Exact reports whether Instant turns every line's value by one offset,
// whatever line it is given: the frame is the file's own, or the file
// writes one offset. A value that belongs to no line (the start of a
// density series) can be turned only then.
func (t *StoredTimes) Exact() bool {
	return len(t.segments) == 1
}

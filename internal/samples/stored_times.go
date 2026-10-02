package samples

import (
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/timestamps"
)

// StoredTimes turns the time values stored for a file's lines into the
// UTC instants an answer about the file gives: the values of
// line_timestamps and of TimeRange under the same file zone.
//
// A signature scan stores each time in the file's own frame
// (milliseconds since the epoch: the instant for a file whose timestamps
// carry zones, the wall clock read as UTC for one whose timestamps carry
// none), beside the zone offset the line writes. An answer reads them in
// the frame its request asks for (frameIn): without a file zone, a
// zone-less file's wall clocks are read in RX_LOG_TZ and a zoned file's
// instants stay as they are; under a file zone every line's written
// wall clock is read in that zone, and for a file whose timestamps
// carry zones that wall clock is the instant plus the offset the line
// writes, the value a samples parser of that frame reads from the line
// (timestamps.Parser's inFileFrame). Nothing of the file is read for it,
// and no line index is needed.
type StoredTimes struct {
	frame timeFrame
	// stored is true when the frame is the file's own: a stored value
	// needs no offset to be read in it.
	stored bool
}

// StoredTimesRequest names the file whose stored times to turn and how
// the answer reads them.
type StoredTimesRequest struct {
	// Path is the file as the caller named it, and Source its pin, which
	// every read goes through; Kind is what the file is.
	Path   string
	Source paths.Pinned
	Kind   filekind.Kind
	// IndexLoader gives the file's line index, or none (nil, NoIndex);
	// it only spares the read of the head for the format.
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
// the pin, as timesOf does: the answer is the same either way.
func StoredTimesOf(req StoredTimesRequest) (*StoredTimes, error) {
	var detected timestamps.Format
	if idx := loadIndexOrNone(Request{Path: req.Path, Source: req.Source, IndexLoader: req.IndexLoader}); idx != nil {
		if idx.TimeIndex == nil {
			return nil, nil
		}
		detected = formatOfSection(idx.TimeIndex)
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
	frame := frameIn(fileZone, detected, logZone)
	return &StoredTimes{frame: frame, stored: frame.readsStoredFrame(detected)}, nil
}

// Instant returns the UTC instant of stored, a time value kept in the
// file's own frame, whose line writes the zone offset writtenOffset
// (minutes east of UTC; 0 for a line that writes none), and false when
// the instant falls outside the years 1 to 9999.
func (t *StoredTimes) Instant(stored int64, writtenOffset int) (int64, bool) {
	if t.stored {
		return t.frame.instant(stored)
	}
	return t.frame.instant(stored + int64(writtenOffset)*msPerMinute)
}

// Exact reports whether Instant needs no line's offset: the frame is
// the file's own. A value that belongs to no line (the start of a
// density series) can be turned only then.
func (t *StoredTimes) Exact() bool {
	return t.stored
}

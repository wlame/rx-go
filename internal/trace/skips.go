package trace

import (
	"errors"
	"log/slog"

	"github.com/wlame/rx-go/internal/compression"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// skipList collects the paths a search passes over, or does not search
// in full, each with the reason the answer gives for it (skip_reasons).
//
// A path is listed once, at the place it was first skipped, with the
// first reason given for it: a file that a later step skips again (a
// damaged frame, then a line no pattern can be credited with) keeps the
// reason that met it first.
//
// Bound: one entry per path the search was given or its walk met, which
// the walk already bounds by the files and directories it lists.
type skipList struct {
	items []rxtypes.SkippedFile
	// listed holds the paths already in items, for the once-per-path rule.
	listed map[string]bool
}

// add records that path was skipped for reason, unless path is listed
// already.
func (s *skipList) add(path, reason string) {
	if s.listed == nil {
		s.listed = map[string]bool{}
	}
	if s.listed[path] {
		return
	}
	s.listed[path] = true
	s.items = append(s.items, rxtypes.SkippedFile{Path: path, Reason: reason})
}

// addErr records that path was skipped because of err, with the reason
// skipReason words for it.
func (s *skipList) addErr(path string, err error) {
	s.add(path, skipReason(err))
}

// addAll records every item of other, in its order.
func (s *skipList) addAll(other []rxtypes.SkippedFile) {
	for _, item := range other {
		s.add(item.Path, item.Reason)
	}
}

// paths is the skipped_files list: the paths, in the order skipped.
// Never nil, so it serializes as [] rather than null.
func (s *skipList) paths() []string {
	out := make([]string, len(s.items))
	for i, item := range s.items {
		out[i] = item.Path
	}
	return out
}

// reasons is the skip_reasons list, in the order of paths. Never nil.
func (s *skipList) reasons() []rxtypes.SkippedFile {
	return append([]rxtypes.SkippedFile{}, s.items...)
}

// The reasons a search gives for a file it reads only in part. Both
// start with notSearchedInFull, so a caller can tell such a file, whose
// matches are kept, from one passed over whole.
const (
	notSearchedInFull      = "not searched in full"
	reasonIncompleteStream = notSearchedInFull + ": the compressed stream ends early; the matches before its end are kept"
	// reasonDamagedFrames is followed by the numbers of the frames.
	reasonDamagedFrames = notSearchedInFull + ": the lines of damaged frames are left out; the matches of every other line are kept"
)

// skipReasons words the errors a search meets for a file it skips, by
// the sentinel each wraps (errors.Is), first match wins. Every wording
// is fixed text: an error's own text names paths (for a link, its
// target, which can be outside every search root) and other internals,
// so it never reaches an answer. A damaged seekable file's reason adds
// the frame numbers, which are positions in the file the caller named.
var skipReasons = []struct {
	err    error
	reason string
}{
	{compression.ErrTooLargeToDecode, compression.TooLargeToDecodeReason},
	{ErrIncompleteStream, reasonIncompleteStream},
	{seekable.ErrDamagedFrame, reasonDamagedFrames},
	{errLineMatchesNoPattern, "a matched line matches none of the patterns alone, so which pattern it belongs to cannot be told"},
}

// skipReason is the reason the answer gives for a file skipped because
// of err: the wording skipReasons holds for it, or the fixed wording
// sandbox.FailureReason gives a failure to reach a file ("permission
// denied", "cannot be read", …). An error that gets the catch-all
// "cannot be read" is logged whole, so the operator still has the
// detail the answer leaves out.
func skipReason(err error) string {
	for _, known := range skipReasons {
		if !errors.Is(err, known.err) {
			continue
		}
		var damaged *damagedFramesError
		if errors.As(err, &damaged) {
			return known.reason + " (frames " + damaged.frames + ")"
		}
		return known.reason
	}
	reason := sandbox.FailureReason(err)
	if reason == sandbox.ReasonUnreadable {
		slog.Default().Warn("trace_file_unreadable", "error", err.Error())
	}
	return reason
}

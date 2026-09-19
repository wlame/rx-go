package index

import (
	"errors"
	"sort"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrLineOutOfRange is returned by lookup helpers when caller asks for
// a line number outside [1, LineCount].
var ErrLineOutOfRange = errors.New("line number out of range")

// FindNearestCheckpoint returns the largest checkpoint whose LineNumber
// is <= target. If no checkpoint is <= target (e.g. target == 0),
// returns the zero-value LineIndexEntry{0, 0}.
//
// LineIndex must be sorted by LineNumber ascending — the builder
// guarantees this.
func FindNearestCheckpoint(idx *rxtypes.UnifiedFileIndex, target int64) rxtypes.LineIndexEntry {
	if len(idx.LineIndex) == 0 {
		return rxtypes.LineIndexEntry{}
	}
	// sort.Search finds the FIRST index where predicate is true;
	// we want the LAST where LineNumber <= target, i.e. predicate
	// "LineNumber > target" flips from false → true at position i;
	// the answer is entry[i-1].
	i := sort.Search(len(idx.LineIndex), func(i int) bool {
		return idx.LineIndex[i].LineNumber > target
	})
	if i == 0 {
		return rxtypes.LineIndexEntry{}
	}
	return idx.LineIndex[i-1]
}

// FindNearestCheckpointForOffset returns the largest checkpoint whose
// ByteOffset is <= target. Symmetric with FindNearestCheckpoint but
// searches on offset instead of line number.
func FindNearestCheckpointForOffset(idx *rxtypes.UnifiedFileIndex, target int64) rxtypes.LineIndexEntry {
	if len(idx.LineIndex) == 0 {
		return rxtypes.LineIndexEntry{}
	}
	i := sort.Search(len(idx.LineIndex), func(i int) bool {
		return idx.LineIndex[i].ByteOffset > target
	})
	if i == 0 {
		return rxtypes.LineIndexEntry{}
	}
	return idx.LineIndex[i-1]
}

// CheckpointForContext returns the checkpoint a pass starts from when it
// must show `context` lines before the line holding offset, or the zero
// entry when that pass has to start at the first byte of the file.
//
// The line holding offset is not known before the pass reads it, but
// the checkpoint at or before offset gives a lower bound: if that
// checkpoint starts line L, offset is on line L or later. A start at a
// line no later than L-context therefore leaves at least `context`
// lines ahead of offset's line, whatever line that turns out to be.
// That start is found by line number, as a --lines request finds the
// checkpoint before its own leading context.
//
// A checkpoint gap holds as many lines as fit in the index step, which
// on a log of long lines is only a few, so the answer can be several
// checkpoints back. Both lookups are binary searches over the index.
func CheckpointForContext(idx *rxtypes.UnifiedFileIndex, offset int64, context int) rxtypes.LineIndexEntry {
	if idx == nil || len(idx.LineIndex) == 0 {
		return rxtypes.LineIndexEntry{}
	}
	holding := FindNearestCheckpointForOffset(idx, offset)
	if holding.LineNumber == 0 {
		return rxtypes.LineIndexEntry{}
	}
	// FindNearestCheckpoint returns the zero entry when no checkpoint
	// starts at or before the wanted line, which is the start of the
	// file: line 1 at byte 0.
	return FindNearestCheckpoint(idx, holding.LineNumber-int64(context))
}

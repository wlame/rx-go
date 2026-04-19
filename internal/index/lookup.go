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

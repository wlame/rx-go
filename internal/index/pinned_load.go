package index

import (
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// DescribesPinned reports whether idx was built from the file src pins,
// by the inode the index recorded and the one the pin recorded.
//
// An index is looked up and validated by path, while a pinned file is
// read through its own handle. When the path led to another file at the
// moment of the look-up (a link retargeted, and put back before the
// read), the index that passed validation describes that other file,
// and its checkpoints would number the pinned file's lines wrongly. The
// inode is what tells the two apart.
//
// An index or a platform that records no inode cannot be compared, and
// is accepted, as IsValidForSource accepts it. The index records no
// device, so two files with one inode number on different filesystems
// are not told apart; such a mix-up needs both inside the search roots
// and costs wrong line numbers, never text from elsewhere.
func DescribesPinned(idx *rxtypes.UnifiedFileIndex, src paths.Pinned) bool {
	if idx == nil || idx.SourceInode == nil || src.IsZero() {
		return true
	}
	inode, _, ok := sourceIdentity(src.Info())
	if !ok {
		return true
	}
	return *idx.SourceInode == inode
}

// LoadForPinned is LoadForSource for a pinned file: the index stored
// for the pin's path, when it is valid for the path and was built from
// the pinned file (DescribesPinned). An index of another file is
// reported as stale, (nil, nil), so the caller answers without it.
func LoadForPinned(src paths.Pinned) (*rxtypes.UnifiedFileIndex, error) {
	idx, err := LoadForSource(src.Path())
	if err != nil || idx == nil {
		return idx, err
	}
	if !DescribesPinned(idx, src) {
		return nil, nil
	}
	return idx, nil
}

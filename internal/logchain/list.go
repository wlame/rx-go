package logchain

import (
	"slices"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// List answers one directory's chains, from names only: the body of
// GET /v1/logs/chains and of `rx logs list --json`.
//
// dir is checked like Resolve checks a handle's directory (search
// roots, hidden components; every directory without roots), made
// absolute, pinned and listed once through paths.ListDir. Then Group
// finds the chains, classifying only the names that can form one.
//
// What it reads beyond the listing: the text check of those names (at
// most a file's signature, its seek table and 8 KiB of its text,
// filekind.OfPinned, as /v1/tree reads every file; in a chain of more
// than MaxParts parts only until that is known), and the stored line
// index of each frozen part for is_indexed (index.PeekForSource, as
// /v1/tree does, stopping at the first part without one; none of an
// empty part, and none for a chain of more than MaxParts parts).
// Nothing is written.
//
// The errors are validateDir's (*paths.ErrPathOutsideRoots,
// *paths.ErrHiddenPath), the pin's or the listing's (wrapping
// fs.ErrNotExist or fs.ErrPermission), and ErrNotADirectory.
func List(dir string) (*rxtypes.ChainsResponse, error) {
	validated, err := validateDir(dir)
	if err != nil {
		return nil, err
	}
	pinned, err := paths.Pin(validated)
	if err != nil {
		return nil, err
	}
	if !pinned.Info().IsDir() {
		return nil, ErrNotADirectory
	}
	listed, err := paths.ListDir(pinned)
	if err != nil {
		return nil, err
	}
	candidates := Group(validated, EntriesOf(listed), ClassifyPinned)
	resp := &rxtypes.ChainsResponse{Path: validated, Chains: make([]rxtypes.ChainEntry, 0, len(candidates))}
	for _, c := range candidates {
		resp.Chains = append(resp.Chains, listingEntry(c))
	}
	return resp, nil
}

// listingEntry is the listing entry of one candidate.
//
// SECURITY: the entry of a chain of more than MaxParts parts lists none
// of them, and looks at none of their indexes: such a chain is not read
// as one text, and its entry stays a few hundred bytes however many
// files share its name.
func listingEntry(c Candidate) rxtypes.ChainEntry {
	entry := rxtypes.ChainEntry{
		Path:               c.Handle(),
		Name:               c.Name,
		Parts:              []string{},
		HasActive:          c.HasActive(),
		Missing:            c.Missing,
		MissingCount:       c.MissingCount,
		CompressionFormats: []string{},
		Unreadable:         []string{},
		TooManyParts:       c.TooManyParts,
	}
	if c.TooManyParts {
		return entry
	}
	entry.IsIndexed = everyFrozenPartIndexed(c)
	for _, p := range c.Parts {
		entry.Parts = append(entry.Parts, p.Name)
		entry.Size += p.Info.Size()
		if p.ReadError != nil {
			entry.Unreadable = append(entry.Unreadable, p.Name)
		}
		if p.Format == compression.FormatNone {
			continue
		}
		// The format's name as /v1/tree reports compression_format: a
		// seekable zstd file is zstd.
		name := filekind.Kind{Format: p.Format}.CompressionName()
		if !slices.Contains(entry.CompressionFormats, name) {
			entry.CompressionFormats = append(entry.CompressionFormats, name)
		}
	}
	slices.Sort(entry.CompressionFormats)
	return entry
}

// peekPartIndex looks at a part's stored line index without counting
// it as a use (index.PeekForSource). A test replaces it to count the
// indexes a listing looks at.
var peekPartIndex = index.PeekForSource

// everyFrozenPartIndexed reports whether every part but the active one
// has a current line index. A peek, not a lookup: listing a directory
// does not use its indexes, so it does not move the index cache
// metrics.
//
// An empty frozen part (0 bytes) counts as indexed without a peek: it
// holds no line, so a description needs no index of it to be ready,
// and the index task a description starts never builds one. The rule
// is the one the description cache applies (cacheable), so `idx` in a
// listing follows the state `ready` of the chain's description.
//
// SECURITY: an index is found and validated by the part's path, while
// the part is the file the listing pinned (Part.File). DescribesPinned
// holds the index to the inode and device that pin recorded, as
// index.LoadForPinned does for a read: when another file took the
// part's name after the listing (a rotation renamed it there), that
// file's index does not make the listed part indexed.
func everyFrozenPartIndexed(c Candidate) bool {
	for _, p := range c.Parts {
		if p.IsActive {
			continue
		}
		// A part the listing could not read has no index this check can
		// trust: validating one reads the part's head and tail.
		if p.ReadError != nil {
			return false
		}
		if p.Info.Size() == 0 {
			continue
		}
		idx, err := peekPartIndex(p.Path)
		if err != nil || idx == nil || !index.DescribesPinned(idx, p.File) {
			return false
		}
	}
	return true
}

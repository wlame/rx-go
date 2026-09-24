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
// filekind.OfPinned, as /v1/tree reads every file), and the stored line
// index of each frozen part for is_indexed (index.PeekForSource, as
// /v1/tree does, stopping at the first part without one). Nothing is
// written.
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
func listingEntry(c Candidate) rxtypes.ChainEntry {
	entry := rxtypes.ChainEntry{
		Path:               c.Handle(),
		Name:               c.Name,
		Parts:              make([]string, 0, len(c.Parts)),
		HasActive:          c.HasActive(),
		Missing:            c.Missing,
		CompressionFormats: []string{},
		IsIndexed:          everyFrozenPartIndexed(c),
	}
	for _, p := range c.Parts {
		entry.Parts = append(entry.Parts, p.Name)
		entry.Size += p.Info.Size()
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

// everyFrozenPartIndexed reports whether every part but the active one
// has a current line index. A peek, not a lookup: listing a directory
// does not use its indexes, so it does not move the index cache
// metrics.
func everyFrozenPartIndexed(c Candidate) bool {
	for _, p := range c.Parts {
		if p.IsActive {
			continue
		}
		if idx, err := index.PeekForSource(p.Path); err != nil || idx == nil {
			return false
		}
	}
	return true
}

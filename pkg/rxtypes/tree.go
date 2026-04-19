package rxtypes

// TreeEntry is a single file or directory entry in a tree listing.
//
// Pointer-typed fields are NULL for entries where they don't apply
// (e.g. Size is nil for directories; ChildrenCount is nil for files).
// The frontend uses this polymorphism to decide which columns to render.
type TreeEntry struct {
	Name              string  `json:"name"`
	Path              string  `json:"path"`
	Type              string  `json:"type"` // "file" | "directory"
	Size              *int64  `json:"size"`
	SizeHuman         *string `json:"size_human"`
	ModifiedAt        *string `json:"modified_at"`
	IsText            *bool   `json:"is_text"`
	IsCompressed      *bool   `json:"is_compressed"`
	CompressionFormat *string `json:"compression_format"`
	IsIndexed         *bool   `json:"is_indexed"`
	LineCount         *int64  `json:"line_count"`
	ChildrenCount     *int    `json:"children_count"`
}

// TreeResponse is the body for GET /v1/tree.
//
// Parent is nil when Path is a configured search root. IsSearchRoot is
// true in the same case. Tree listings are subject to --search-root
// sandboxing, so Path is always either a search root or one of its
// descendants.
//
// Stage 9 Round 2 S2 rule: schema-documented fields must emit null when
// unset. Python emits total_size/total_size_human as null on a
// search-root TreeResponse; Go matches by dropping omitempty.
type TreeResponse struct {
	Path           string      `json:"path"`
	Parent         *string     `json:"parent"`
	IsSearchRoot   bool        `json:"is_search_root"`
	Entries        []TreeEntry `json:"entries"`
	TotalEntries   int         `json:"total_entries"`
	TotalSize      *int64      `json:"total_size"`
	TotalSizeHuman *string     `json:"total_size_human"`
}

// SearchRootsResponse is the body for GET /v1/search-roots (informational).
type SearchRootsResponse struct {
	Roots      []TreeEntry `json:"roots"`
	TotalRoots int         `json:"total_roots"`
}

package rxtypes

// TreeEntry is a single file or directory entry in a tree listing.
//
// Pointer-typed fields are NULL for entries where they don't apply
// (e.g. Size is nil for directories; ChildrenCount is nil for files).
// The frontend uses this polymorphism to decide which columns to render.
type TreeEntry struct {
	Name      string  `json:"name"`
	Path      string  `json:"path"`
	Type      string  `json:"type"` // "file" | "directory"
	Size      *int64  `json:"size"`
	SizeHuman *string `json:"size_human"`
	// ModifiedAt is RFC 3339 in UTC with six fractional digits, e.g.
	// "2026-09-06T00:53:24.438322Z". rx-python renders the same string.
	ModifiedAt *string `json:"modified_at" format:"date-time"`
	IsText     *bool   `json:"is_text"`
	// NotTextReason says why a file's text is not text, in the words
	// every command refuses the file with; nil for a text file, a
	// directory and a file the listing could not open.
	NotTextReason     *string `json:"not_text_reason" doc:"Why the file is not text, in the words every command refuses it with, starting with 'not a text file': a NUL byte in the first 8 KiB of its text, UTF-16 text, or a systemd journal whose header rx cannot read. Null for a text file, a directory, and a file the listing could not open."`
	IsCompressed      *bool   `json:"is_compressed"`
	CompressionFormat *string `json:"compression_format"`
	IsIndexed         *bool   `json:"is_indexed"`
	LineCount         *int64  `json:"line_count"`
	ChildrenCount     *int    `json:"children_count"`
	// Findings is how much of the default set of signatures the
	// findings cache holds for the file; nil for a directory, a file
	// whose text is not text, and a file with no current cache entry.
	Findings *TreeFindings `json:"findings" doc:"What the findings cache holds for the file: its state and its findings count. Null for a directory, a file that is not text, and a file the cache holds nothing current for."`
}

// TreeFindings is what the findings cache holds for one file of a tree
// listing, read from its summary file alone.
type TreeFindings struct {
	_     struct{} `nullable:"true"`
	State string   `json:"state" enum:"full,partial" doc:"full: every signature of the default set has a current cache entry; partial: some signature has one, an opt-in one included."`
	Count int64    `json:"count" doc:"The findings of every current entry together, opt-in signatures included."`
}

// TreeResponse is the body for GET /v1/tree.
//
// Parent is nil when Path is a configured search root. IsSearchRoot is
// true in the same case. Tree listings are subject to --search-root
// sandboxing, so Path is always either a search root or one of its
// descendants.
//
// schema-documented fields must emit null when
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

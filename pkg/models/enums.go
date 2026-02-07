package models

// IndexType represents the type of index
type IndexType string

const (
	IndexTypeRegular      IndexType = "regular"
	IndexTypeCompressed   IndexType = "compressed"
	IndexTypeSeekableZstd IndexType = "seekable_zstd"
)

// FileType represents the type of file
type FileType string

const (
	FileTypeRegular    FileType = "regular"
	FileTypeGzip       FileType = "gzip"
	FileTypeZstd       FileType = "zstd"
	FileTypeXz         FileType = "xz"
	FileTypeBzip2      FileType = "bzip2"
	FileTypeUnknown    FileType = "unknown"
)

// LineEnding represents the line ending style
type LineEnding string

const (
	LineEndingLF    LineEnding = "LF"
	LineEndingCRLF  LineEnding = "CRLF"
	LineEndingCR    LineEnding = "CR"
	LineEndingMixed LineEnding = "mixed"
)

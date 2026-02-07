package compression

// Format represents a compression format
type Format string

const (
	FormatNone   Format = "none"
	FormatGzip   Format = "gzip"
	FormatZstd   Format = "zstd"
	FormatBzip2  Format = "bzip2"
	FormatXz     Format = "xz"
	FormatLz4    Format = "lz4"
	FormatUnknown Format = "unknown"
)

// String returns the string representation
func (f Format) String() string {
	return string(f)
}

// IsCompressed returns true if the format is compressed
func (f Format) IsCompressed() bool {
	return f != FormatNone && f != FormatUnknown
}

// DecompressorCommand returns the command to decompress this format
func (f Format) DecompressorCommand() string {
	switch f {
	case FormatGzip:
		return "gzip"
	case FormatZstd:
		return "zstd"
	case FormatBzip2:
		return "bzip2"
	case FormatXz:
		return "xz"
	case FormatLz4:
		return "lz4"
	default:
		return ""
	}
}

// Extension returns the typical file extension for this format
func (f Format) Extension() string {
	switch f {
	case FormatGzip:
		return ".gz"
	case FormatZstd:
		return ".zst"
	case FormatBzip2:
		return ".bz2"
	case FormatXz:
		return ".xz"
	case FormatLz4:
		return ".lz4"
	default:
		return ""
	}
}

package rxtypes

// ChainEntry is one log chain of a directory listing: the files of one
// rotated log (`syslog`, `syslog.1`, `syslog.2.gz`, …), found from their
// names alone. It is an element of ChainsResponse.Chains.
//
// Nothing here is read from the parts' text but whether each part is
// text and how it is compressed; the time order, the checks and the
// line numbers of a chain come from describing it.
type ChainEntry struct {
	// Path is the chain's handle: its directory joined with its name.
	// It is the active file's path, whether that file exists or not.
	Path string `json:"path" doc:"The chain's handle: its directory joined with its name, which is the active file's path whether that file exists or not. The other /v1/logs routes take it as path."`
	// Name is the active file's name.
	Name string `json:"name" doc:"The chain's name: the name of its active file (the one without a number or date), such as syslog or app.log."`
	// Parts are the file names in the provisional order, oldest first.
	Parts []string `json:"parts" nullable:"false" doc:"The names of the chain's files in the directory, in the provisional order, oldest first: by the number or date in each name in its rotation scheme's direction, then by modification time; the active file last. One file in several encodings is listed once, as the encoding rx reads."`
	// HasActive says whether the active file exists.
	HasActive bool `json:"has_active" doc:"Whether the active file (the one named like the chain) exists and is a part."`
	// Missing names the absent numbers of a numbered chain.
	Missing []string `json:"missing" nullable:"false" doc:"The names an absent part would have, for each number missing between the lowest expected number (0 when a .0 part exists, else 1) and the highest present one, without a compression suffix. Empty for a chain whose parts are all dated."`
	// Size is the sum of the parts' file sizes.
	Size int64 `json:"size" doc:"The sum of the sizes in bytes of the files listed in parts, as stored (compressed for a compressed part)."`
	// CompressionFormats are the distinct formats among the parts.
	CompressionFormats []string `json:"compression_formats" nullable:"false" doc:"The distinct compression formats of the parts, sorted: gzip, bz2, xz, zstd (seekable zstd included). A plain part adds none."`
	// IsIndexed says whether every frozen part has a current line index.
	IsIndexed bool `json:"is_indexed" doc:"Whether every part but the active file has a current line index, as is_indexed of /v1/tree tells for one file."`
}

// ChainsResponse is the body of GET /v1/logs/chains and of each
// directory `rx logs list --json` prints: the log chains of one
// directory, sorted by name, case-insensitive.
type ChainsResponse struct {
	// Path is the listed directory, absolute.
	Path string `json:"path" doc:"The listed directory, as an absolute path."`
	// Chains are the directory's chains.
	Chains []ChainEntry `json:"chains" nullable:"false" doc:"The directory's log chains, sorted by name, case-insensitive. Empty when it has none."`
}

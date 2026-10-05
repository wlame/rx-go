package rxtypes

// SamplesRequest is the request shape for the samples endpoint / CLI.
//
// At least one of Offsets or Lines must be populated. If both are set,
// Offsets wins (matching Python's branching in web.py).
type SamplesRequest struct {
	Path          string   `json:"path"`
	Offsets       []int64  `json:"offsets,omitempty"`
	Lines         []string `json:"lines,omitempty"` // may contain ranges like "100-200" or negative "-50"
	BeforeContext int      `json:"before_context"`
	AfterContext  int      `json:"after_context"`
}

// JSON KEY ORDERING NOTE:
//
// The SamplesResponse below uses map[string]*  for Offsets, Lines, and
// Samples. Go's encoding/json marshals maps in ALPHABETICAL key order,
// while Python's Pydantic preserves insertion order (Python 3.7+ dict
// semantics). A request like --offsets=1000,500,2000 produces:
//
//   Python JSON: {"1000": ..., "500": ..., "2000": ...}  (insertion order)
//   Go JSON:     {"1000": ..., "2000": ..., "500": ...}  (alphabetical)
//
// If rx-viewer or any CLI consumer iterates via Object.keys()/items()
// expecting request-matching order, they'll see different output
// between the two backends. At v1 we DOCUMENT this divergence rather
// than restructure the wire type into an ordered-pair slice — the
// the parity tests will confirm whether the frontend is actually
// affected.
//
// If a parity test reveals a frontend dependency on ordering, the fix is
// one of:
//
//   - Change the Go type to []KeyValue pairs with explicit order
//     (breaks JSON shape — frontend must migrate).
//   - Add a MarshalJSON on SamplesResponse that writes keys in an
//     explicit side-channel order (preserves shape but doubles the
//     amount of state the handler has to track).
//   - Document the ordering as "implementation-defined" and ask the
//     frontend to sort client-side.

// SamplesResponse is the response shape for GET /v1/samples.
//
// Offsets maps a string-encoded byte offset (or a range like "100-200")
// to a 1-based line number. Lines is the inverse: a string line number
// or range mapped to a starting byte offset. Samples maps the same key
// used in Offsets or Lines to the retrieved context lines (which INCLUDE
// the target line itself at the center of the slice).
//
// BYTE-OFFSET PRECISION: Offsets and Lines use int64 for the values
// because they represent file byte offsets, which can exceed int32
// range on files >2 GB. All other byte-offset fields in this package
// also use int64 (see index.CompressedOffset, trace.Match.Offset,
// etc.); matching that convention keeps the wire type honest on any
// build target.. JSON-wise there is
// no visible difference — Go marshals both int and int64 as plain
// numbers and Python parses them as unbounded int, so this is a
// precision fix with no frontend-visible change.
//
// CompressionFormat is nil for uncompressed files. CLICommand is set only
// on GET requests that come from the web API (nil on direct CLI use).
//
// schema-documented fields must emit
// explicit null when unset. Python's SamplesResponse emits both
// compression_format and cli_command as null on default values, so
// CLICommand is typed as *string (not plain string with omitempty) to
// preserve that distinction.
type SamplesResponse struct {
	Path              string              `json:"path"`
	Offsets           map[string]int64    `json:"offsets"`
	Lines             map[string]int64    `json:"lines"`
	BeforeContext     int                 `json:"before_context"`
	AfterContext      int                 `json:"after_context"`
	Samples           map[string][]string `json:"samples"`
	IsCompressed      bool                `json:"is_compressed"`
	CompressionFormat *string             `json:"compression_format"`
	CLICommand        *string             `json:"cli_command"`
	// Timestamps maps each time query of a timestamps-mode request to
	// the line it found: the line at T for a single time, the first line
	// of a range. -1 when no line is at T, or the range holds none; its
	// sample is then null. Empty in the other modes.
	Timestamps map[string]int64 `json:"timestamps" doc:"Each time query of a timestamps request mapped to the line it found: the first line whose own timestamp is at or after the time, or a range's first line; -1 when there is none (its sample is null). Empty in the other modes."`
	// TimeFormat is the file's timestamp format, or nil when none is
	// recognized in the first mebibyte of its text. Present in every
	// mode.
	TimeFormat *SamplesTimeFormat `json:"time_format" doc:"The file's timestamp format, in every mode; null when no format is recognized in the first mebibyte of its text."`
	// LineTimestamps maps each key of Samples to the effective timestamp
	// of each line of its sample, in the same order: milliseconds since
	// the Unix epoch, as a UTC instant, or nil for a line that has none.
	// A line's effective timestamp is its own, or else the own timestamp
	// of the nearest earlier line that has one when that line starts at
	// most RX_TIMESTAMP_LOOKBACK_KB KiB before it. A key whose sample is
	// null has a null entry. The whole map is nil (null) when the file
	// has no timestamp format. Present in every mode.
	LineTimestamps map[string][]*int64 `json:"line_timestamps" nullable:"true" doc:"Each key of samples mapped to the effective timestamp of each line of its sample, in order: milliseconds since the Unix epoch as a UTC instant, or null for a line without one. A line's effective timestamp is its own, or the own timestamp of the nearest earlier line that has one when that line starts at most RX_TIMESTAMP_LOOKBACK_KB KiB before it; a zone-less file's wall clock is read in RX_LOG_TZ, and under the request's file_tz every line's written wall clock is read in that zone. A key whose sample is null maps to null. The whole field is null when the file has no timestamp format. Present in every mode."`
}

// SamplesTimeFormat is the timestamp format of a file a samples answer
// read: the format family, whether its timestamps carry zones, and the
// zone assumed for a timestamp that carries none. Nullable for the
// reason LineLengthStats gives.
type SamplesTimeFormat struct {
	_           struct{} `nullable:"true"`
	Format      string   `json:"format" enum:"iso,clf,ctime,syslog,slash,dotted,epoch" doc:"The timestamp format of the lines."`
	HasZone     bool     `json:"has_zone" doc:"Whether most timestamps carry a zone."`
	AssumedZone string   `json:"assumed_zone" doc:"The zone a timestamp without one is read in: RX_LOG_TZ (default UTC) for a file whose timestamps carry no zone, UTC for one whose timestamps do; the request's file_tz, whatever the file, when it names one."`
}

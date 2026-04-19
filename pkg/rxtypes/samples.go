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

// JSON KEY ORDERING NOTE (Stage 8 Reviewer 1 High #3 / Finding 5):
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
// Stage 9 parity tests will confirm whether the frontend is actually
// affected.
//
// If Stage 9 reveals a frontend dependency on ordering, the fix is
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
// build target. See Stage 8 Reviewer 3 High #14. JSON-wise there is
// no visible difference — Go marshals both int and int64 as plain
// numbers and Python parses them as unbounded int, so this is a
// precision fix with no frontend-visible change.
//
// CompressionFormat is nil for uncompressed files. CLICommand is set only
// on GET requests that come from the web API (nil on direct CLI use).
//
// Stage 9 Round 2 S2 user rule: schema-documented fields must emit
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
}

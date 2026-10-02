package sigscan

import (
	"fmt"
	"sort"

	"github.com/wlame/rx-go/internal/signatures"
)

// EngineVersion is the version of the scan engine. It enters the key
// of every cached finding and of every file's masked ranges, so a
// change that can change any answer of the engine bumps it: a finding,
// an excerpt, a masked range, or MaxBodiesPerSignature.
const EngineVersion = 2

// MaxBodiesPerSignature is how many bodies of one redacting block or run
// signature a scan records exactly (Result.MaskedRanges): far more than
// a log holds, and far more than the findings cap. Past it the scan
// fails closed: every line from the first line of the last body it
// recorded to the end of the text is masked. It bounds the memory the
// bodies take (one stored entry per body, per chunk in flight and in
// the merge).
const MaxBodiesPerSignature = 100_000

// The defaults of the Options fields left at zero. They equal the
// defaults of RX_SIGNATURES_LINE_BYTES and RX_FINDINGS_MAX_PER_SIGNATURE,
// which the commands read into Options.
const (
	DefaultLineBytes = 65536
	DefaultCap       = 10000
)

// Options are the settings of one scan. The commands and routes fill
// them from the environment (RX_WORKERS, RX_SIGNATURES_LINE_BYTES,
// RX_FINDINGS_MAX_PER_SIGNATURE); this package never reads it.
type Options struct {
	// Workers is how many goroutines walk lines at once; 0 or less
	// means the CPU count.
	Workers int
	// LineBytes is how many bytes of each line the matchers see (the
	// line view); 0 or less means DefaultLineBytes.
	LineBytes int
	// Cap is the most findings of one signature the result keeps, the
	// first ones by line; 0 or less means DefaultCap. Count stays exact
	// past it.
	Cap int
	// LookbackBytes is how far back, in bytes of text, a finding whose
	// first line has no timestamp of its own takes the timestamp of an
	// earlier line (RX_TIMESTAMP_LOOKBACK_KB in bytes): the nearest
	// stamped line counts when it starts at most this many bytes before
	// the finding's first line. 0 or less keeps only a line's own
	// timestamp, as the setting's 0 does.
	LookbackBytes int
	// Redactors are the loaded signatures with `redact: true`, selected
	// or not (signatures.Catalogue.Redactors): every excerpt of every
	// finding is masked by all of them and by the selected redacting
	// signatures, so no excerpt shows a value one of them matches on its
	// line: an anchor's match values, or the values an entropy or
	// base64-run measure finds. A signature here that does not redact,
	// or that has neither (another measure, a blank or same run), masks
	// nothing.
	//
	// The redacting blocks and `match` runs among them, and among the
	// selected signatures, are also walked for their bodies, gated by
	// their keyword clauses: every line a block joined after its anchor
	// (a block shorter than its min_lines included), and every line of a
	// run after its first. Those lines are Result.MaskedRanges, and a
	// finding whose first line is one of them has the excerpt
	// `[masked, N bytes]`. The body of a PEM key is its key material, and
	// it holds no value a mask could find.
	Redactors []*signatures.Signature
	// Progress, when set, is called as the file is read with the bytes
	// of the file read so far and the file's size, both as stored on
	// disk (compressed bytes for a compressed file). Calls never overlap
	// and read never goes down.
	Progress func(read, total int64)
}

// Finding is one place in the text where a signature matched.
//
// Lines are 1-based positions in the text; offsets are byte positions
// in the text (the decompressed stream for a compressed file), with
// EndOffset exclusive: the offset of the line after the finding's last
// line, which includes that line's ending, or the end of the text for
// a last line without one.
type Finding struct {
	StartLine, EndLine     int64
	StartOffset, EndOffset int64
	// TimeMS is the time of the finding's first line, in milliseconds
	// since the Unix epoch in the file's own frame, as an index records
	// it (the UTC instant for a file whose timestamps carry zones, the
	// wall clock read as UTC for one whose timestamps carry none): the
	// line's own timestamp, or that of the nearest earlier stamped line
	// when it starts at most Options.LookbackBytes before. Nil when
	// there is none, and in a file without timestamps.
	TimeMS *int64
	// Excerpt is the first line's view with every value a redacting
	// signature matches on it masked (Options.Redactors), each byte that
	// is not UTF-8 then replaced by U+FFFD, cut to 200 bytes on a
	// character boundary: always valid UTF-8. When the first line lies in
	// Result.MaskedRanges, the excerpt is MaskedLineText of its length
	// instead.
	Excerpt string
	// ValueStart and ValueEnd are bytes in the first line, counted from
	// the line's first byte in the text: the value of a regex signature
	// (its capture group of the first match, the whole match by
	// default), the first token or run an entropy or base64-run measure
	// finds, and the anchor's first match of a redacting block or match
	// run. Nil otherwise.
	ValueStart, ValueEnd *int
	// Masked is true when the finding's text is masked: its signature
	// redacts, a redacting signature (Options.Redactors, or a selected
	// one) masked a value in the excerpt, or the first line lies in
	// Result.MaskedRanges.
	Masked bool
	// firstLen is the length of the first line without its ending, the
	// N of the excerpt when that line lies in a masked range. Only the
	// merge reads it, and it clears it before the result is returned, so
	// a finding read back from a cache equals the one a scan returned.
	firstLen int64
}

// Series is the bucket series a density measure keeps beside its
// findings.
type Series struct {
	BucketMS, StartMS int64
	Counts            []int64
}

// SignatureResult is what a scan found for one signature.
type SignatureResult struct {
	// Findings are the first Cap findings by (start line, end line).
	Findings []Finding
	// Count is the number of findings in the text, those past the cap
	// included.
	Count int64
	// Series is set for a density measure only.
	Series *Series
}

// Result is what a scan found, by signature id.
type Result struct {
	BySignature map[string]*SignatureResult
	// MaskedRanges are the lines of the text that no output shows: the
	// bodies of every redacting block and run (Options.Redactors), as
	// [first, last] line numbers, 1-based and both included, sorted,
	// with ranges that overlap or touch joined. Empty when there is
	// none, and for an empty selection, which reads nothing. They are
	// the same for every chunking and storage of the text.
	MaskedRanges [][2]int64
	// BytesRead is how many bytes of the file the scan read, as stored
	// on disk: the whole file, plus what was read twice at chunk edges,
	// plus the head of the file read for its timestamp format.
	BytesRead int64
}

// MaskedLineText is what every output shows in place of a line that
// lies in a masked range: "[masked, N bytes]", N the line's length in
// bytes without its line ending.
func MaskedLineText(length int64) string {
	return fmt.Sprintf("[masked, %d bytes]", length)
}

// InMaskedRanges reports whether line lies in one of ranges, which are
// sorted and disjoint as Result.MaskedRanges are (a binary search).
func InMaskedRanges(ranges [][2]int64, line int64) bool {
	// sort.Search finds the first range that ends at or after line; line
	// is in a range only when that range also starts at or before it.
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] >= line })
	return i < len(ranges) && ranges[i][0] <= line
}

package rxtypes

import "cmp"

// Finding is one place in one file where a signature matched. Every
// answer that carries findings (the trace answer of `rx trace
// --signatures`, and the findings routes) uses this one shape.
//
// Lines are 1-based positions in the file's text and offsets byte
// positions in it (the decompressed stream for a compressed file), the
// coordinates of every other rx answer; EndOffset is exclusive: where
// the line after the finding's last line starts, or the end of the text.
//
// Excerpt, Lines, Before and After never show a value a redacting
// signature matches on their line: such a value is masked (all but a
// few leading characters become `*`), and every line after the first
// line of a block or run finding of a redacting signature is replaced by
// `[masked, N bytes]`. Each line is valid UTF-8 (a byte that is not part
// of a valid character becomes U+FFFD) and holds at most
// RX_MAX_LINE_TEXT_BYTES bytes of its line, cut on a character boundary.
type Finding struct {
	// Signature is the id of the signature that matched.
	Signature string `json:"signature" doc:"Id of the signature that matched."`
	// File is the id of the file in the answer's files map ("f1"); it is
	// left out where an answer holds one file only.
	File string `json:"file,omitempty" doc:"Id of the file in the answer's files map; left out where an answer holds one file."`
	// StartLine and EndLine are the finding's first and last line,
	// 1-based and inclusive.
	StartLine int64 `json:"start_line" doc:"First line of the finding, 1-based."`
	EndLine   int64 `json:"end_line" doc:"Last line of the finding, 1-based, inclusive."`
	// StartOffset and EndOffset are byte offsets in the text, EndOffset
	// exclusive.
	StartOffset int64 `json:"start_offset" doc:"Byte offset in the text where the first line starts."`
	EndOffset   int64 `json:"end_offset" doc:"Byte offset in the text just past the last line and its line ending (exclusive)."`
	// TimeMS is the time of the first line, in milliseconds since the
	// Unix epoch: its own timestamp, or that of the nearest earlier
	// stamped line within RX_TIMESTAMP_LOOKBACK_KB. Null when there is
	// none.
	TimeMS *int64 `json:"time_ms" doc:"Time of the first line in milliseconds since the Unix epoch (its own timestamp, or that of the nearest earlier stamped line within RX_TIMESTAMP_LOOKBACK_KB); null when there is none."`
	// Excerpt is the first line, masked, cut to 200 bytes.
	Excerpt string `json:"excerpt" doc:"The first line, with redacted values masked, cut to 200 bytes on a character boundary."`
	// ValueStart and ValueEnd are bytes in the first line, counted from
	// its first byte: the value a regex signature captured, the token a
	// value measure found, or the anchor's first match of a redacting
	// block or run.
	ValueStart *int `json:"value_start,omitempty" doc:"Start of the finding's value, in bytes from the first line's first byte."`
	ValueEnd   *int `json:"value_end,omitempty" doc:"End of the finding's value (exclusive), in bytes from the first line's first byte."`
	// Masked is true when anything in the excerpt or the lines of the
	// finding was masked, or the signature itself redacts.
	Masked bool `json:"masked,omitempty" doc:"True when a value in the excerpt or the lines was masked, or the signature redacts."`
	// Lines are the finding's lines, StartLine to EndLine, each without
	// its line ending. Trace answers always fill them, until the text
	// budget (RX_FINDINGS_TEXT_MAX_BYTES) runs out.
	Lines []string `json:"lines,omitempty" nullable:"false" doc:"The finding's lines, first to last, each without its line ending; left out once the answer's text budget (RX_FINDINGS_TEXT_MAX_BYTES) is spent."`
	// Before and After are the lines just before StartLine and just
	// after EndLine that were asked for (rx trace -B, -A, --context).
	Before []string `json:"before,omitempty" nullable:"false" doc:"Lines just before the first line, as many as were asked for and the file has."`
	After  []string `json:"after,omitempty" nullable:"false" doc:"Lines just after the last line, as many as were asked for and the file has."`
	// LinesTruncated is true when the text budget ran out at or before
	// this finding, so its lines are left out.
	LinesTruncated bool `json:"lines_truncated,omitempty" doc:"True when the answer's text budget ran out at or before this finding, so lines, before and after are left out."`
}

// CompareFindings orders two findings the way every answer lists them:
// by file id (by its number, f2 before f10), then first line, then last
// line, then signature id. It returns a negative number, 0 or a positive
// number, like strings.Compare.
func CompareFindings(a, b *Finding) int {
	return cmp.Or(
		CompareIDs(a.File, b.File),
		cmp.Compare(a.StartLine, b.StartLine),
		cmp.Compare(a.EndLine, b.EndLine),
		cmp.Compare(a.Signature, b.Signature),
	)
}

// SignatureRef describes one signature of an answer's selection: what
// a reader of a finding needs to show it, without the catalogue.
type SignatureRef struct {
	Name  string `json:"name" doc:"Short title of the signature."`
	Group string `json:"group" doc:"The signature's group."`
	Level string `json:"level" doc:"info, low, medium, high or critical."`
	Kind  string `json:"kind" doc:"regex, block, run or measure."`
}

// FindingCount is how many findings one signature has in one file.
type FindingCount struct {
	// Count is every finding in the file, those past the cap included.
	Count int64 `json:"count" doc:"Findings of the signature in the file, those past RX_FINDINGS_MAX_PER_SIGNATURE included."`
	// Stored is how many of them the answer holds: the first ones by
	// line, at most RX_FINDINGS_MAX_PER_SIGNATURE.
	Stored int `json:"stored" doc:"How many of them are kept: the first ones by line, at most RX_FINDINGS_MAX_PER_SIGNATURE."`
}

// FileFindingSummary is what a scan of one file found, by signature.
type FileFindingSummary struct {
	// State is "full" (every default signature has a cached or fresh
	// answer for the file), "partial" or "none".
	State string `json:"state" doc:"full: every signature of the default set has an answer for the file; partial: some signatures have one; none: no signature has one."`
	// Signatures holds the count of every selected signature.
	Signatures map[string]FindingCount `json:"signatures" doc:"Findings counts of each selected signature, by id."`
}

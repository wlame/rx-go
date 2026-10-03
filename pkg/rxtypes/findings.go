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
// With masking on (`rx trace --mask-secrets`, `rx serve --mask-secrets`
// or RX_MASK_SECRETS; for an HTTP answer, the server's mode), Excerpt,
// Lines, Before and After never show a value a redacting signature
// matches on their line: such a value is masked (all but a few leading
// characters become `*`), and every line after the first line of a
// block or run finding of a redacting signature is replaced by
// `[masked, N bytes]`. With masking off, the default, they show the
// file's text as it is and Masked is never set. Each line is valid
// UTF-8 (a byte that is not part
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
	// Excerpt is the first line, masked when masking is on, cut to 200
	// bytes.
	Excerpt string `json:"excerpt" doc:"The first line, with redacted values masked when the server masks secrets, cut to 200 bytes on a character boundary."`
	// ValueStart and ValueEnd are bytes in the first line, counted from
	// its first byte: the value a regex signature captured, the token a
	// value measure found, or the anchor's first match of a redacting
	// block or run.
	ValueStart *int `json:"value_start,omitempty" doc:"Start of the finding's value, in bytes from the first line's first byte."`
	ValueEnd   *int `json:"value_end,omitempty" doc:"End of the finding's value (exclusive), in bytes from the first line's first byte."`
	// Masked is true when anything in the excerpt, the lines of the
	// finding or the lines before and after it was masked, or the
	// signature itself redacts and masking is on. Never true with masking
	// off.
	Masked bool `json:"masked,omitempty" doc:"True when a value in the excerpt, the lines, or the lines before and after was masked, or the signature redacts and the server masks secrets. Never true on a server that does not mask them, the default."`
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

// FindingSeries is the bucket series a density signature keeps with its
// findings: how many lines fell in each bucket of time, from the bucket
// that starts at StartMS on.
type FindingSeries struct {
	BucketMS int64   `json:"bucket_ms" doc:"Length of each bucket in milliseconds."`
	StartMS  int64   `json:"start_ms" doc:"Start of the first bucket, in milliseconds since the Unix epoch, as an instant read the way the answer reads the file's times."`
	Counts   []int64 `json:"counts" nullable:"false" doc:"Lines in each bucket, in time order."`
}

// FindingsResponse is the findings of one file, as the findings cache
// holds them: the body of GET /v1/findings and the result of a findings
// task (POST /v1/findings), which is the same answer with no filter and
// the first page.
//
// The summary members (State to Series, Total and CLICommand) describe
// the requested selection; Findings is one page of the stored findings
// that pass the request's filters, and Ticks, when asked for, places
// those findings on a timeline.
type FindingsResponse struct {
	State      string                   `json:"state" enum:"full,partial,none" doc:"full: every signature of the default set has a current cache entry for the file; partial: at least one signature has one (an opt-in one included), and not full; none: none has."`
	Checked    []string                 `json:"checked" nullable:"false" doc:"The signatures of the selection the cache answers, in catalogue order."`
	Missing    []string                 `json:"missing" nullable:"false" doc:"The signatures of the selection the cache does not answer (never scanned, or stale), in catalogue order. A scan (POST /v1/findings) fills them."`
	Counts     map[string]FindingCount  `json:"counts" doc:"The findings counts of every checked signature, by id."`
	Series     map[string]FindingSeries `json:"series" doc:"The bucket series of every checked density signature, by id; a series whose start cannot be read in the request's file_tz is left out."`
	Total      int                      `json:"total" doc:"How many stored findings of the checked signatures pass the filters: the length of the list the page is cut from."`
	Findings   []Finding                `json:"findings" nullable:"false" doc:"One page of the stored findings that pass the filters: from offset, at most limit. In the one order (first line, last line, signature id), or the reverse with before_line."`
	Ticks      *FindingsTicks           `json:"ticks" doc:"The filtered findings placed on a timeline, with ticks=W; null without it."`
	CLICommand string                   `json:"cli_command" doc:"The rx command that gives the same findings."`
}

// FindingsTicks places the filtered findings of a file on a timeline of
// Width columns over the file's time range, the range GET /v1/time-range
// gives for the same file_tz: column k holds the findings whose time t
// has floor((t - start_ms) * width / (end_ms - start_ms + 1)) = k.
type FindingsTicks struct {
	_       struct{}     `nullable:"true"`
	Width   int          `json:"width" doc:"The number of columns asked for (ticks=W)."`
	StartMS *int64       `json:"start_ms" doc:"The file's first timestamp as GET /v1/time-range gives it (first_ms) for the same file_tz; null when it gives none."`
	EndMS   *int64       `json:"end_ms" doc:"The file's last timestamp as GET /v1/time-range gives it (last_ms) for the same file_tz; null when it gives none."`
	Columns []TickColumn `json:"columns" nullable:"false" doc:"Each column that holds at least one filtered finding with a time, in column order. Empty when start_ms or end_ms is null. A finding without a time, or with a time outside start_ms to end_ms, is in no column."`
}

// TickColumn is one column of a timeline that holds findings.
type TickColumn struct {
	Column     int                  `json:"column" doc:"The column, from 0 to width - 1."`
	Signatures map[string]TickCount `json:"signatures" doc:"The findings of the column, by signature id."`
}

// TickCount is how many findings of one signature fall in one column,
// and the first of them.
type TickCount struct {
	Count     int   `json:"count" doc:"How many findings of the signature fall in the column."`
	FirstLine int64 `json:"first_line" doc:"The first line of the first of them, in line order."`
}

// FindingsScanRequest is the body of POST /v1/findings: the file to
// scan and the signatures to scan it for.
type FindingsScanRequest struct {
	Path       string `json:"path" doc:"The file to scan. Must be inside a configured --search-root."`
	Signatures string `json:"signatures,omitempty" required:"false" default:"default" maxLength:"4096" doc:"The selection, as rx trace --signatures= takes it: comma-separated ids, groups and tags, all, default, -word to remove (traceback,sql; all,-pii). Default: the default set."`
	Force      bool   `json:"force,omitempty" required:"false" default:"false" doc:"Scan every selected signature again, those with a current cache entry too."`
}

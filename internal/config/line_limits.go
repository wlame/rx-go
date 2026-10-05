package config

// Bounds on what one matched or context line can put into a trace
// answer, and so into memory while ripgrep's output is read.
//
// ripgrep writes one JSON event per line it reports, holding the whole
// line and an object of about 50 bytes for every submatch on it, and
// puts no limit on either. Without a bound, one line of 100 MB on which
// the pattern matches every character is an event of about 5 GB, and
// one request can exhaust the server. With these bounds an event costs
// at most about MaxLineTextBytes plus MaxSubmatchesPerLine small
// records, whatever the line holds; the line's offset, number and
// length stay exact, and the answer marks what it left out.
const (
	// DefaultMaxLineTextBytes is how many bytes of a line's text a
	// trace answer holds. A longer line is cut there (at the start of a
	// UTF-8 character) and marked line_text_truncated.
	DefaultMaxLineTextBytes = 1 << 20

	// DefaultMaxSubmatchesPerLine is how many submatches a trace answer
	// lists for one line. Past it, or past the end of a cut line_text,
	// the rest are left out and the match is marked
	// submatches_truncated.
	DefaultMaxSubmatchesPerLine = 10_000
)

// MaxLineTextBytes returns RX_MAX_LINE_TEXT_BYTES, from 1 byte to
// 256 MiB, or DefaultMaxLineTextBytes.
func MaxLineTextBytes() int {
	return MaxLineTextBytesSetting.Value()
}

// MaxSubmatchesPerLine returns RX_MAX_SUBMATCHES_PER_LINE, from 1 to
// 1,000,000, or DefaultMaxSubmatchesPerLine.
func MaxSubmatchesPerLine() int {
	return MaxSubmatchesPerLineSetting.Value()
}

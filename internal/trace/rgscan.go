package trace

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/config"
)

// ============================================================================
// Bounded decoding of ripgrep's --json events
// ============================================================================
//
// ripgrep writes one JSON event per line it reports, and an event holds
// the whole line plus an object of about 50 bytes for every submatch on
// it. Neither has a limit, so a decoder that reads an event whole needs
// memory in proportion to the line: a 100 MB line on which the pattern
// matches every character is an event of about 5 GB. encoding/json
// cannot help, not even its streaming json.Decoder.Token: a string is
// one token, so the 100 MB line arrives as one 100 MB string.
//
// eventScanner is a small JSON reader made for rg's event shape. It
// walks the event straight from the stream and never holds more of it
// than this:
//
//   - a string is decoded into a boundedText, which keeps its first N
//     bytes and only counts the rest, so the line's exact length is
//     known without holding the line;
//   - once a line holds RX_MAX_SUBMATCHES_PER_LINE submatches, or a
//     submatch starts past the end of the text kept, the rest of the
//     submatches array is skipped in a tight byte loop;
//   - every number is small and read whole.
//
// The fields that come after the line's text in rg's output
// (line_number, absolute_offset) are read as usual, so every match keeps
// its exact offset and line number however long the line is.

// ErrMalformedEvent reports output from ripgrep that is not a JSON
// event of the shape rx reads, or that ends in the middle of one.
var ErrMalformedEvent = errors.New("ripgrep output is not a valid JSON event")

// malformed builds an ErrMalformedEvent with what went wrong.
func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformedEvent, fmt.Sprintf(format, args...))
}

// eventLimits bounds what one line can put into an event.
type eventLimits struct {
	// lineTextBytes is how many bytes of a line's text are kept.
	lineTextBytes int
	// submatches is how many submatches of one line are kept.
	submatches int
}

// currentEventLimits reads the limits from the environment. It is
// called once per stream, so a test can change them with t.Setenv.
func currentEventLimits() eventLimits {
	return eventLimits{
		lineTextBytes: config.MaxLineTextBytes(),
		submatches:    config.MaxSubmatchesPerLine(),
	}
}

// lineBreakRoom is the room kept past the text limit for a line's
// break ("\n" or "\r\n"). A line whose text fits the limit is then kept
// whole, break included, exactly as ripgrep wrote it.
const lineBreakRoom = 2

// maxPathBytes is how much of an event's path is kept. rx feeds rg on
// stdin, so the path is "<stdin>"; the bound only guards the parser.
const maxPathBytes = 4096

// maxStatsBytes bounds the raw JSON of a stats or elapsed object, which
// ripgrep writes in a few hundred bytes.
const maxStatsBytes = 64 * 1024

// ----------------------------------------------------------------------------
// String sinks
// ----------------------------------------------------------------------------

// stringSink receives a JSON string's decoded bytes, a run at a time.
// The run is only valid during the call.
type stringSink interface {
	write(p []byte) error
}

// boundedText keeps the first keep bytes written to it and counts all of
// them. It also remembers the last two bytes, which tell whether the
// text ends with a line break and how long that break is.
type boundedText struct {
	kept []byte
	keep int
	size int
	tail [2]byte // the last two bytes written; tail[1] is the last
}

// reset empties b for a new string, of which it will keep keep bytes.
// The kept slice's memory is reused.
func (b *boundedText) reset(keep int) {
	b.kept = b.kept[:0]
	b.keep = max(keep, 0)
	b.size = 0
	b.tail = [2]byte{}
}

func (b *boundedText) write(p []byte) error {
	if room := b.keep - len(b.kept); room > 0 {
		b.kept = append(b.kept, p[:min(room, len(p))]...)
	}
	b.size += len(p)
	switch {
	case len(p) >= 2:
		b.tail = [2]byte{p[len(p)-2], p[len(p)-1]}
	case len(p) == 1:
		b.tail = [2]byte{b.tail[1], p[0]}
	}
	return nil
}

// lineBreakLen is the length of the line break the text ends with: 2
// for "\r\n", 1 for "\n", 0 for none. trimTrailingNewline strips the
// same bytes.
func (b *boundedText) lineBreakLen() int {
	if b.size < 1 || b.tail[1] != '\n' {
		return 0
	}
	if b.size >= 2 && b.tail[0] == '\r' {
		return 2
	}
	return 1
}

// base64Sink decodes a base64 string as it streams by and hands the
// decoded bytes to dst. ripgrep sends a line that is not valid UTF-8
// as {"bytes": "<base64>"}, and the limits apply to the bytes it
// stands for, as they do to text.
type base64Sink struct {
	dst     *boundedText
	pending [4]byte // characters of a quad not yet complete
	n       int     // how many of pending are in use
	out     []byte  // scratch for decoded runs, reused
}

func (b *base64Sink) reset(dst *boundedText) {
	b.dst = dst
	b.n = 0
}

func (b *base64Sink) write(p []byte) error {
	// Complete a quad the previous run left open.
	for b.n > 0 && len(p) > 0 {
		b.pending[b.n] = p[0]
		b.n++
		p = p[1:]
		if b.n == len(b.pending) {
			b.n = 0
			if err := b.decode(b.pending[:]); err != nil {
				return err
			}
		}
	}
	whole := len(p) / 4 * 4
	if whole > 0 {
		if err := b.decode(p[:whole]); err != nil {
			return err
		}
	}
	b.n = copy(b.pending[:], p[whole:])
	return nil
}

// decode decodes whole quads into dst.
func (b *base64Sink) decode(quads []byte) error {
	if need := base64.StdEncoding.DecodedLen(len(quads)); cap(b.out) < need {
		b.out = make([]byte, need)
	}
	n, err := base64.StdEncoding.Decode(b.out[:cap(b.out)], quads)
	if err != nil {
		return malformed("invalid base64 payload: %v", err)
	}
	return b.dst.write(b.out[:n])
}

// finish checks that the string ended on a whole quad, as padded base64
// does.
func (b *base64Sink) finish() error {
	if b.n != 0 {
		return malformed("base64 payload ends inside a quad")
	}
	return nil
}

// characterCut is where b may be cut so that it keeps at most n bytes
// without splitting a character. b must hold more than n bytes.
//
// A character is what Go reads from UTF-8 bytes: a valid sequence of one
// to four bytes, or a single byte that is not part of one, which an
// answer's JSON shows as U+FFFD. Byte n starts a character unless it is a
// continuation byte of a valid sequence that starts before it, and such
// a sequence starts at most utf8.UTFMax-1 bytes before n. So the cut
// looks at no more than those bytes, whatever the length of b: it is n,
// or the start of the sequence that holds byte n. The text the cut keeps
// therefore reads as the start of the whole text.
//
// When b ends inside a sequence that is valid so far (the rest of the
// line was not kept), the cut treats that sequence as a character and
// stops before it. A scan and a trace-cache hit keep the same bytes of a
// line, so both cut it at the same place.
func characterCut(b []byte, n int) int {
	// utf8.RuneStart is true for every byte except a continuation byte
	// (10xxxxxx), which can only be the second to fourth byte of a
	// sequence. A byte that can start one is always a character's first
	// byte, so the cut may go before it.
	if utf8.RuneStart(b[n]) {
		return n
	}
	for start := n - 1; start >= 0 && start >= n-(utf8.UTFMax-1); start-- {
		if !utf8.RuneStart(b[start]) {
			continue
		}
		// b[start] is the nearest byte before n that can begin a
		// sequence. Byte n belongs to that sequence only if it is valid
		// and long enough to reach n.
		seq := b[start:]
		// utf8.FullRune is false only when seq ends part-way through a
		// sequence that is valid so far: the bytes kept stop inside it.
		if !utf8.FullRune(seq) {
			return start
		}
		// utf8.DecodeRune returns (RuneError, 1) for a byte that does not
		// begin a valid sequence; a valid sequence comes back with its
		// length (3 for U+FFFD written in the file itself).
		r, size := utf8.DecodeRune(seq)
		isValidSequence := r != utf8.RuneError || size > 1
		if isValidSequence && start+size > n {
			return start
		}
		return n
	}
	// No byte in reach can begin a sequence that holds byte n, so byte n
	// is a stray continuation byte: a character of its own.
	return n
}

// ----------------------------------------------------------------------------
// The event being read
// ----------------------------------------------------------------------------

// rawSubmatch is one submatch as read, before the line's cut is known
// for certain: where its kept text lies in the scanner's arena, the
// length of its whole text, and its position in the line.
type rawSubmatch struct {
	from, to   int
	size       int
	start, end int
}

// eventFields gathers every field any event type has; the event's type
// decides which ones it reports. Gathering them all lets the data object
// come before or after "type", and its fields in any order.
type eventFields struct {
	typ            RgEventType
	path           RgText
	hasLines       bool
	lineNumber     int
	absoluteOffset int64
	binaryOffset   *int64
	stats          RgStats
	elapsedTotal   RgElapsed

	// hasSubmatches is false when the event has no submatches array,
	// which reads as nil rather than an empty list.
	hasSubmatches bool
	submatches    []rawSubmatch
	// submatchesDropped is set when the parser left submatches out.
	submatchesDropped bool

	// lineCut is how many bytes of the line's text are kept, and
	// lineCutKnown whether the line has been read yet. A submatch that
	// starts at or past lineCut is left out.
	lineCut       int
	lineTruncated bool
	lineCutKnown  bool
}

// ----------------------------------------------------------------------------
// The scanner
// ----------------------------------------------------------------------------

// eventScanner reads rg's events from a stream with bounded memory. The
// buffers it holds are reused from one event to the next.
type eventScanner struct {
	r      *bufio.Reader
	limits eventLimits

	key      boundedText // the object key being read
	short    boundedText // short strings: the event type
	line     boundedText // the line's text
	pathText boundedText
	subText  boundedText // one submatch's text while it is read
	b64      base64Sink
	// arena holds the kept text of the line's submatches, one after
	// another. Their total is bounded by the line text limit.
	arena  []byte
	fields eventFields
}

func newEventScanner(r io.Reader, limits eventLimits) *eventScanner {
	return &eventScanner{r: bufio.NewReaderSize(r, eventReadBufferSize), limits: limits}
}

// buffered returns the bytes the reader holds, filling it first when it
// is empty. The slice is valid until the next read from s.r.
func (s *eventScanner) buffered() ([]byte, error) {
	if s.r.Buffered() == 0 {
		if _, err := s.r.Peek(1); err != nil {
			return nil, err
		}
	}
	return s.r.Peek(s.r.Buffered())
}

// unexpectedEnd turns a read failure inside an event into the error to
// report: the end of the output is a malformed (cut-off) event, any
// other failure is the reader's own error.
func unexpectedEnd(err error) error {
	if errors.Is(err, io.EOF) || err == nil {
		return malformed("the output ends inside an event")
	}
	return err
}

// peekNonSpace skips whitespace and returns the next byte without
// consuming it.
func (s *eventScanner) peekNonSpace() (byte, error) {
	for {
		b, err := s.r.Peek(1)
		if len(b) == 0 {
			return 0, err
		}
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			_, _ = s.r.Discard(1)
		default:
			return b[0], nil
		}
	}
}

// expect consumes the next non-space byte, which must be want.
func (s *eventScanner) expect(want byte) error {
	c, err := s.peekNonSpace()
	if err != nil {
		return unexpectedEnd(err)
	}
	if c != want {
		return malformed("expected %q, found %q", want, c)
	}
	_, _ = s.r.Discard(1)
	return nil
}

// skipLine discards the rest of the current line, its break included,
// without holding it. It is how the scanner gets past a malformed event.
func (s *eventScanner) skipLine() {
	for {
		_, err := s.r.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

// ---- objects -----------------------------------------------------------------

// startObject consumes an object's '{' and reports whether the object
// is empty (its '}' is then consumed too).
func (s *eventScanner) startObject() (empty bool, err error) {
	if err = s.expect('{'); err != nil {
		return false, err
	}
	c, err := s.peekNonSpace()
	if err != nil {
		return false, unexpectedEnd(err)
	}
	if c == '}' {
		_, _ = s.r.Discard(1)
		return true, nil
	}
	return false, nil
}

// nextKey reads an object key and the ':' after it. The key is valid
// until the next string is read.
func (s *eventScanner) nextKey() ([]byte, error) {
	// No key rx reads is longer than this; a longer one is unknown
	// either way, and keeping a prefix of it cannot match a known key.
	const maxKeyBytes = 64
	s.key.reset(maxKeyBytes + 1)
	if err := s.readString(&s.key); err != nil {
		return nil, err
	}
	if err := s.expect(':'); err != nil {
		return nil, err
	}
	return s.key.kept, nil
}

// afterValue consumes the ',' or '}' after an object's value and
// reports whether another field follows.
func (s *eventScanner) afterValue() (more bool, err error) {
	return s.afterElement('}')
}

// afterElement consumes the ',' or closing byte after a container's
// element and reports whether another element follows.
func (s *eventScanner) afterElement(closing byte) (more bool, err error) {
	c, err := s.peekNonSpace()
	if err != nil {
		return false, unexpectedEnd(err)
	}
	_, _ = s.r.Discard(1)
	switch c {
	case ',':
		return true, nil
	case closing:
		return false, nil
	default:
		return false, malformed("expected ',' or %q, found %q", closing, c)
	}
}

// ---- strings -----------------------------------------------------------------

// readString reads a JSON string and writes its decoded bytes to sink.
// Plain runs go to the sink straight from the read buffer; only escapes
// are decoded one at a time.
func (s *eventScanner) readString(sink stringSink) error {
	if err := s.expect('"'); err != nil {
		return err
	}
	for {
		buf, err := s.buffered()
		if len(buf) == 0 {
			return unexpectedEnd(err)
		}
		i := plainRunLength(buf)
		if i > 0 {
			if err := sink.write(buf[:i]); err != nil {
				return err
			}
		}
		if i == len(buf) {
			_, _ = s.r.Discard(i)
			continue
		}
		switch c := buf[i]; c {
		case '"':
			_, _ = s.r.Discard(i + 1)
			return nil
		case '\\':
			_, _ = s.r.Discard(i + 1)
			if err := s.readEscape(sink); err != nil {
				return err
			}
		default:
			// Left unconsumed: a raw line break here is where the next
			// line starts, and skipLine must find it.
			_, _ = s.r.Discard(i)
			return malformed("control character %#x inside a string", c)
		}
	}
}

// plainRunLength is how many bytes at the start of buf need no decoding:
// everything up to a quote, a backslash or a control character.
func plainRunLength(buf []byte) int {
	for i, c := range buf {
		if c == '"' || c == '\\' || c < 0x20 {
			return i
		}
	}
	return len(buf)
}

// readEscape decodes the escape after a backslash.
func (s *eventScanner) readEscape(sink stringSink) error {
	c, err := s.r.ReadByte()
	if err != nil {
		return unexpectedEnd(err)
	}
	if simple, ok := simpleEscapes[c]; ok {
		return sink.write([]byte{simple})
	}
	if c != 'u' {
		return malformed("invalid escape \\%c", c)
	}
	r, err := s.readHex4()
	if err != nil {
		return err
	}
	if utf16.IsSurrogate(r) {
		r = s.readLowSurrogate(r)
	}
	var enc [utf8.UTFMax]byte
	return sink.write(enc[:utf8.EncodeRune(enc[:], r)])
}

// simpleEscapes maps the one-character JSON escapes to their bytes.
var simpleEscapes = map[byte]byte{
	'"': '"', '\\': '\\', '/': '/',
	'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t',
}

// readHex4 reads the four hex digits of a \u escape.
func (s *eventScanner) readHex4() (rune, error) {
	var digits [4]byte
	if _, err := io.ReadFull(s.r, digits[:]); err != nil {
		return 0, unexpectedEnd(err)
	}
	r, ok := hex4(digits[:])
	if !ok {
		return 0, malformed("invalid \\u escape %q", digits[:])
	}
	return r, nil
}

// hex4 decodes four hex digits into the UTF-16 code unit they spell.
func hex4(digits []byte) (rune, bool) {
	var r rune
	for _, c := range digits {
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(v)
	}
	return r, true
}

// readLowSurrogate completes a UTF-16 surrogate pair whose first half
// is high, as encoding/json does: when a \u escape holding the second
// half follows, both become one character; otherwise high becomes
// U+FFFD and what follows is read on its own.
func (s *eventScanner) readLowSurrogate(high rune) rune {
	next, _ := s.r.Peek(6)
	if len(next) < 6 || next[0] != '\\' || next[1] != 'u' {
		return unicode.ReplacementChar
	}
	low, ok := hex4(next[2:6])
	if !ok {
		return unicode.ReplacementChar
	}
	r := utf16.DecodeRune(high, low)
	if r == unicode.ReplacementChar {
		return r
	}
	_, _ = s.r.Discard(6)
	return r
}

// readShortString reads a string of which only the first maxBytes
// matter, such as an event type.
func (s *eventScanner) readShortString(maxBytes int) (string, error) {
	s.short.reset(maxBytes)
	if err := s.readString(&s.short); err != nil {
		return "", err
	}
	return string(s.short.kept), nil
}

// readPayload reads ripgrep's {"text": …} or {"bytes": …} object, or
// null, into dst, keeping keep bytes of the string it stands for. A
// "bytes" payload is base64 of bytes that are not valid UTF-8; dst gets
// the decoded bytes, so both forms hold the line's own bytes.
func (s *eventScanner) readPayload(dst *boundedText, keep int) error {
	dst.reset(keep)
	c, err := s.peekNonSpace()
	if err != nil {
		return unexpectedEnd(err)
	}
	if c == 'n' {
		return s.readLiteral("null")
	}
	empty, err := s.startObject()
	for more := !empty; err == nil && more; more, err = s.afterValue() {
		var key []byte
		if key, err = s.nextKey(); err != nil {
			break
		}
		switch string(key) {
		case "text":
			dst.reset(keep)
			err = s.readString(dst)
		case "bytes":
			dst.reset(keep)
			s.b64.reset(dst)
			if err = s.readString(&s.b64); err == nil {
				err = s.b64.finish()
			}
		default:
			err = s.skipValue()
		}
		if err != nil {
			break
		}
	}
	return err
}

// payloadText turns a read payload into the RgText rx reports: the kept
// bytes, the whole payload's size, and whether the bytes kept are all
// of it.
func payloadText(b *boundedText) RgText {
	return RgText{Text: string(b.kept), Size: b.size, Truncated: len(b.kept) < b.size}
}

// ---- numbers and literals ------------------------------------------------------

// readLiteral consumes the exact bytes of word (null, true or false).
func (s *eventScanner) readLiteral(word string) error {
	if _, err := s.peekNonSpace(); err != nil {
		return unexpectedEnd(err)
	}
	got, err := s.r.Peek(len(word))
	if string(got) != word {
		if err != nil {
			return unexpectedEnd(err)
		}
		return malformed("expected %s, found %q", word, got)
	}
	_, _ = s.r.Discard(len(word))
	return nil
}

// readIntOrNull reads an integer, or null, which reads as (0, false).
func (s *eventScanner) readIntOrNull() (int64, bool, error) {
	c, err := s.peekNonSpace()
	if err != nil {
		return 0, false, unexpectedEnd(err)
	}
	if c == 'n' {
		return 0, false, s.readLiteral("null")
	}
	// An int64 has at most 20 characters; anything longer is not one.
	const maxNumberBytes = 24
	var digits [maxNumberBytes]byte
	n := 0
	for {
		b, peekErr := s.r.Peek(1)
		if len(b) == 0 {
			if n > 0 && errors.Is(peekErr, io.EOF) {
				break
			}
			return 0, false, unexpectedEnd(peekErr)
		}
		if !isNumberByte(b[0]) {
			break
		}
		if n == maxNumberBytes {
			return 0, false, malformed("number too long")
		}
		digits[n] = b[0]
		n++
		_, _ = s.r.Discard(1)
	}
	v, err := strconv.ParseInt(string(digits[:n]), 10, 64)
	if err != nil {
		return 0, false, malformed("expected an integer, found %q", digits[:n])
	}
	return v, true, nil
}

func isNumberByte(c byte) bool {
	return (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E'
}

// isScalarByte reports whether c can be part of a number or of the
// literals true, false and null.
func isScalarByte(c byte) bool {
	return isNumberByte(c) || (c >= 'a' && c <= 'z')
}

// ---- skipping --------------------------------------------------------------------

// skipValue consumes one JSON value of any kind without keeping it.
func (s *eventScanner) skipValue() error {
	return s.passValue(0, nil)
}

// skipRestOfArray consumes the rest of the array the scanner is inside,
// its closing ']' included, without keeping it.
func (s *eventScanner) skipRestOfArray() error {
	return s.passValue(1, nil)
}

// captureValue consumes one JSON value and returns its raw bytes. A
// value longer than limit is an error.
func (s *eventScanner) captureValue(limit int) ([]byte, error) {
	rec := &capture{limit: limit}
	err := s.passValue(0, rec)
	return rec.raw, err
}

// capture collects the bytes passValue consumes, up to a limit.
type capture struct {
	raw   []byte
	limit int
}

func (c *capture) add(p []byte) error {
	if len(c.raw)+len(p) > c.limit {
		return malformed("value longer than %d bytes", c.limit)
	}
	c.raw = append(c.raw, p...)
	return nil
}

// passValue consumes JSON text until a value is complete: the value that
// starts at the next byte when depth is 0, or the container the scanner
// is inside when depth is 1. It tracks only what decides where the value
// ends (nesting, strings and their escapes), one buffered run at a time,
// so it skips megabytes of submatches quickly and holds none of them.
// rec, when set, receives every byte consumed.
func (s *eventScanner) passValue(depth int, rec *capture) error {
	if depth == 0 {
		if _, err := s.peekNonSpace(); err != nil {
			return unexpectedEnd(err)
		}
	}
	var st passState
	st.depth = depth
	for {
		buf, err := s.buffered()
		if len(buf) == 0 {
			// A number or literal may end the stream itself.
			if st.inScalar && errors.Is(err, io.EOF) {
				return nil
			}
			return unexpectedEnd(err)
		}
		used, done, perr := st.scan(buf)
		if rec != nil {
			if err := rec.add(buf[:used]); err != nil {
				return err
			}
		}
		_, _ = s.r.Discard(used)
		if perr != nil || done {
			return perr
		}
	}
}

// passState is where passValue is inside the JSON text, kept across
// buffered runs.
type passState struct {
	depth    int
	inString bool
	escaped  bool
	inScalar bool // inside a number or literal that is the whole value
}

// scan advances over buf and returns how many bytes it consumed and
// whether the value is complete.
func (st *passState) scan(buf []byte) (used int, done bool, err error) {
	for i, c := range buf {
		if st.inString {
			switch {
			case st.escaped:
				st.escaped = false
			case c == '\\':
				st.escaped = true
			case c == '"':
				st.inString = false
				if st.depth == 0 {
					return i + 1, true, nil
				}
			}
			continue
		}
		if st.inScalar {
			if isScalarByte(c) {
				continue
			}
			// The scalar ended; c belongs to what follows it.
			return i, true, nil
		}
		switch c {
		case '"':
			st.inString = true
		case '{', '[':
			st.depth++
		case '}', ']':
			st.depth--
			if st.depth < 0 {
				return i, false, malformed("unexpected %q", c)
			}
			if st.depth == 0 {
				return i + 1, true, nil
			}
		case ' ', '\t', '\r', '\n', ',', ':':
			if st.depth == 0 {
				return i, false, malformed("expected a value, found %q", c)
			}
		default:
			if !isScalarByte(c) {
				return i, false, malformed("unexpected %q", c)
			}
			if st.depth == 0 {
				st.inScalar = true
			}
		}
	}
	return len(buf), false, nil
}

// ---- events --------------------------------------------------------------------

// readEvent reads one top-level value: an event, or null, which yields
// (nil, nil). An event of a type rx does not model comes back with
// ErrUnknownEvent.
func (s *eventScanner) readEvent() (*RgEvent, error) {
	c, err := s.peekNonSpace()
	if err != nil {
		return nil, unexpectedEnd(err)
	}
	if c == 'n' {
		return nil, s.readLiteral("null")
	}
	s.resetFields()
	f := &s.fields
	empty, err := s.startObject()
	for more := !empty; err == nil && more; more, err = s.afterValue() {
		var key []byte
		if key, err = s.nextKey(); err != nil {
			break
		}
		switch string(key) {
		case "type":
			var typ string
			typ, err = s.readShortString(32)
			f.typ = RgEventType(typ)
		case "data":
			err = s.readData()
		default:
			err = s.skipValue()
		}
		if err != nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return s.event()
}

// resetFields clears the event being read, keeping the memory of its
// submatch list and text arena for the next one.
func (s *eventScanner) resetFields() {
	subs := s.fields.submatches[:0]
	s.fields = eventFields{submatches: subs}
	s.arena = s.arena[:0]
}

// readData reads an event's data object into s.fields.
func (s *eventScanner) readData() error {
	empty, err := s.startObject()
	for more := !empty; err == nil && more; more, err = s.afterValue() {
		var key []byte
		if key, err = s.nextKey(); err != nil {
			break
		}
		if err = s.readDataField(string(key)); err != nil {
			break
		}
	}
	return err
}

// readDataField reads the value of one field of an event's data object.
func (s *eventScanner) readDataField(key string) error {
	f := &s.fields
	switch key {
	case "path":
		err := s.readPayload(&s.pathText, maxPathBytes)
		f.path = payloadText(&s.pathText)
		return err
	case "lines":
		err := s.readPayload(&s.line, s.limits.lineTextBytes+lineBreakRoom)
		f.hasLines = true
		f.lineCut, f.lineTruncated = s.lineCut()
		f.lineCutKnown = true
		return err
	case "line_number":
		n, _, err := s.readIntOrNull()
		f.lineNumber = int(n)
		return err
	case "absolute_offset":
		n, _, err := s.readIntOrNull()
		f.absoluteOffset = n
		return err
	case "binary_offset":
		n, ok, err := s.readIntOrNull()
		if ok {
			f.binaryOffset = &n
		}
		return err
	case "submatches":
		return s.readSubmatches()
	case "stats":
		return s.readSmallObject(&f.stats)
	case "elapsed_total":
		return s.readSmallObject(&f.elapsedTotal)
	default:
		return s.skipValue()
	}
}

// readSmallObject decodes a value ripgrep keeps small (statistics) with
// encoding/json.
func (s *eventScanner) readSmallObject(dst any) error {
	raw, err := s.captureValue(maxStatsBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return malformed("%v", err)
	}
	return nil
}

// lineCut is how many bytes of the line just read are kept, and whether
// that leaves part of it out. A line whose text (its break aside) fits
// the limit is kept whole, break included; a longer one is cut at the
// limit, at the start of a character.
func (s *eventScanner) lineCut() (cut int, truncated bool) {
	b := &s.line
	if b.size-b.lineBreakLen() <= s.limits.lineTextBytes {
		return len(b.kept), false
	}
	return characterCut(b.kept, s.limits.lineTextBytes), true
}

// readSubmatches reads the submatches array, keeping at most
// limits.submatches entries and, once the line is known to be cut, only
// those that start inside the text kept. ripgrep lists submatches in
// order of position, so the first one left out ends the array: the rest
// is skipped without being decoded.
func (s *eventScanner) readSubmatches() error {
	f := &s.fields
	f.hasSubmatches = true
	if err := s.expect('['); err != nil {
		return err
	}
	c, err := s.peekNonSpace()
	if err != nil {
		return unexpectedEnd(err)
	}
	if c == ']' {
		_, _ = s.r.Discard(1)
		return nil
	}
	for {
		if len(f.submatches) == s.limits.submatches {
			f.submatchesDropped = true
			return s.skipRestOfArray()
		}
		sm, err := s.readSubmatch()
		if err != nil {
			return err
		}
		if f.lineCutKnown && f.lineTruncated && sm.start >= f.lineCut {
			f.submatchesDropped = true
			return s.skipRestOfArray()
		}
		f.submatches = append(f.submatches, sm)
		more, err := s.afterElement(']')
		if err != nil || !more {
			return err
		}
	}
}

// readSubmatch reads one {"match": …, "start": n, "end": n} object. Its
// text goes into the arena, which all of the line's submatches share and
// which keeps no more than the line text limit in total. Submatches do
// not overlap and come in order, so the ones that start inside the kept
// line text never need more than that.
func (s *eventScanner) readSubmatch() (rawSubmatch, error) {
	var sm rawSubmatch
	empty, err := s.startObject()
	for more := !empty; err == nil && more; more, err = s.afterValue() {
		var key []byte
		if key, err = s.nextKey(); err != nil {
			break
		}
		switch string(key) {
		case "match":
			room := s.limits.lineTextBytes + lineBreakRoom - len(s.arena)
			err = s.readPayload(&s.subText, room)
			sm.size = s.subText.size
			sm.from = len(s.arena)
			s.arena = append(s.arena, s.subText.kept...)
			sm.to = len(s.arena)
		case "start":
			var n int64
			n, _, err = s.readIntOrNull()
			sm.start = int(n)
		case "end":
			var n int64
			n, _, err = s.readIntOrNull()
			sm.end = int(n)
		default:
			err = s.skipValue()
		}
		if err != nil {
			break
		}
	}
	return sm, err
}

// event builds the RgEvent of the type read from the gathered fields.
func (s *eventScanner) event() (*RgEvent, error) {
	f := &s.fields
	ev := &RgEvent{Type: f.typ}
	switch f.typ {
	case RgEventBegin:
		ev.Begin = &RgBeginData{Path: f.path}
	case RgEventMatch:
		subs, dropped := s.boundedSubmatches()
		ev.Match = &RgMatchData{
			Path:                f.path,
			Lines:               s.lineText(),
			LineNumber:          f.lineNumber,
			AbsoluteOffset:      f.absoluteOffset,
			Submatches:          subs,
			SubmatchesTruncated: dropped,
		}
	case RgEventContext:
		subs, _ := s.boundedSubmatches()
		ev.Context = &RgContextData{
			Path:           f.path,
			Lines:          s.lineText(),
			LineNumber:     f.lineNumber,
			AbsoluteOffset: f.absoluteOffset,
			Submatches:     subs,
		}
	case RgEventEnd:
		ev.End = &RgEndData{Path: f.path, BinaryOffset: f.binaryOffset, Stats: f.stats}
	case RgEventSummary:
		ev.Summary = &RgSummaryData{ElapsedTotal: f.elapsedTotal, Stats: f.stats}
	default:
		return ev, ErrUnknownEvent
	}
	return ev, nil
}

// lineText is the line as the answer reports it: whole when it fits the
// limit, else its first bytes up to the cut, marked Truncated. Size is
// always the whole line's, break included.
func (s *eventScanner) lineText() RgText {
	f := &s.fields
	if !f.hasLines {
		return RgText{}
	}
	return RgText{
		Text:      string(s.line.kept[:f.lineCut]),
		Size:      s.line.size,
		Truncated: f.lineTruncated,
	}
}

// boundedSubmatches returns the submatches the answer keeps and whether
// the list may leave some out. On a cut line only those that start
// inside the kept text stay, and one that runs past the cut keeps its
// true Start and End and the part of its text that the kept line holds
// (Truncated).
//
// A cut line always reports the list as possibly incomplete, whether or
// not ripgrep found a submatch past the cut, and a trace-cache hit that
// cuts the line applies the same rule (submatchesFromSpans).
func (s *eventScanner) boundedSubmatches() ([]RgSubmatch, bool) {
	f := &s.fields
	dropped := f.submatchesDropped || f.lineTruncated
	if !f.hasSubmatches {
		return nil, dropped
	}
	out := make([]RgSubmatch, 0, len(f.submatches))
	for _, sm := range f.submatches {
		text := s.arena[sm.from:sm.to]
		need := sm.size
		if f.lineTruncated {
			if sm.start >= f.lineCut {
				dropped = true
				continue
			}
			// The submatch keeps the part of its bytes that the kept
			// line holds. The line was cut at the start of a character,
			// so that part ends on one too.
			need = min(need, f.lineCut-sm.start)
		}
		if len(text) < need {
			// The parser kept less of this text than the answer needs. It
			// cannot happen with ripgrep's field order; the submatch is
			// left out rather than reported with a shortened text.
			dropped = true
			continue
		}
		out = append(out, RgSubmatch{
			Match: RgText{Text: string(text[:need]), Size: sm.size, Truncated: need < sm.size},
			Start: sm.start,
			End:   sm.end,
		})
	}
	return out, dropped
}

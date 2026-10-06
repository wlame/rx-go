package journal

// This file reads the payload of a DATA object, "FIELD=value", and
// decodes it when it is stored compressed. The framing of each
// compression is systemd's (src/basic/compress.c, v257.13): an xz stream,
// a zstd frame, or for lz4 the decoded size as a little-endian uint64
// followed by one raw lz4 block.
//
// SECURITY: the renderer reads seven fields. A value of any other field
// costs its name search alone, whatever its size: its stored bytes are
// read as far as the name needs, at most nameStoredLimit of them in one
// read, and decoded only as far as the decoder must go to give the name
// (zstd: its first block, and the frame checksum when that block is the
// last; xz: about 4 KiB of text through its dictionary buffer; lz4: the
// sequences that hold the name's bytes, and of a long match only its
// first length bytes). So a core dump kept as one field
// costs at most 129 KiB of reading, and a value of a few stored bytes
// that decodes to 128 MiB costs a block of decoding, not 128 MiB per
// entry that lists it. Such a value is never damage for its size. A
// value of a field the renderer reads is read whole, so it may be at
// most compression.WindowLimit (128 MiB), stored or decoded, and the
// shown values of one entry may be at most 128 MiB together
// (walker.readEntry); past either bound the entry is damaged. A size the
// file declares is checked before any buffer is made from it
// (checkDeclaredSize): against those bounds, and, for a zstd frame or an
// lz4 block, against the most text its stored bytes can hold, so that a
// value of a few stored bytes cannot make the reader allocate 128 MiB.
// A decoder stops at the bound whatever the stream holds. An xz stream
// may declare a dictionary of at most 16 MiB (maxXZDictionary), in the
// name search too: the decoder makes the whole dictionary a block
// declares before it reads the block.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"

	"github.com/wlame/rx-go/internal/compression"
)

// The object flags that say how a DATA object's payload is stored
// (journal-def.h). At most one may be set; the other bits of the flags
// byte do not say anything about the payload (COMPRESSION_FROM_OBJECT,
// journal-def.h).
const (
	objectCompressedXZ   = 1
	objectCompressedLZ4  = 2
	objectCompressedZstd = 4
	objectCompressedMask = objectCompressedXZ | objectCompressedLZ4 | objectCompressedZstd
)

// maxValue is the largest value, stored or decoded, rx reads.
const maxValue = compression.WindowLimit

// maxXZDictionary is the largest dictionary an xz value may declare:
// 16 MiB. systemd writes its xz values with a 1 MiB dictionary
// (compress_blob_xz, compress.c), and the dictionary of xz's default
// preset is 8 MiB; a stream that declares more is damage. The LZMA2
// decoder makes the whole dictionary a block declares when the block
// header leaves the text's size out, so without this bound a value of 60
// stored bytes could make the reader allocate 128 MiB.
const maxXZDictionary = 16 << 20

// payloadDecoder decodes one compressed payload, and says what its
// stored bytes declare before they are decoded.
type payloadDecoder struct {
	name string
	// decode decodes stored into a new slice of at most limit bytes; a
	// value that passes limit is refused (tooLarge).
	decode func(stored []byte, limit int) ([]byte, error)
	// declaredSize returns the decoded size stored declares up front,
	// from which the decoder makes its buffer, and 0 when it declares
	// none. It is nil for a format that never declares one (xz), whose
	// decoder grows its buffer as it decodes.
	declaredSize func(stored []byte) uint64
	// mostText returns the most text n stored bytes can decode to, by the
	// format's densest encoding.
	mostText func(n int) uint64
}

// payloadDecoders holds the decoder of each compression flag.
var payloadDecoders = map[uint8]payloadDecoder{
	objectCompressedXZ:   {name: "xz", decode: decodeXZ},
	objectCompressedLZ4:  {name: "lz4", decode: decodeLZ4, declaredSize: lz4DeclaredSize, mostText: lz4MostText},
	objectCompressedZstd: {name: "zstd", decode: decodeZstd, declaredSize: zstdDeclaredSize, mostText: zstdMostText},
}

// errValueTooLarge is a decoder's error for a value larger than maxValue,
// errNotDecoded its error for bytes that do not decode, and
// errXZDictionaryTooLarge its error for an xz stream that declares a
// dictionary above maxXZDictionary.
var (
	errValueTooLarge        = errors.New("the value is larger than 128 MiB")
	errNotDecoded           = errors.New("the value does not decompress")
	errXZDictionaryTooLarge = errors.New("the xz stream declares a dictionary above 16 MiB")
)

// errPastEntryBudget is the error of a shown value that would take the
// shown values of its entry past maxEntryValues. It is not damage of the
// DATA object, whose answer is the same for every entry that lists it:
// it is not cached, and readEntry turns it into damage of the entry.
var errPastEntryBudget = errors.New("the shown values of the entry pass 128 MiB together")

// tooLarge is a decoder's error for a value that passes limit: the
// entry's budget when limit is what the budget had left, the bound of
// one value when limit is that bound.
func tooLarge(limit int) error {
	if limit < maxValue {
		return errPastEntryBudget
	}
	return errValueTooLarge
}

// xzDictionaryDamage is the damage of the DATA object what whose xz
// stream declares a dictionary above maxXZDictionary.
func xzDictionaryDamage(what objectRef) error {
	return damaged("%s declares an xz dictionary of more than 16 MiB", what)
}

// dataPayloadOffset is where a DATA object's payload starts.
func (f *journalFile) dataPayloadOffset() uint64 {
	if f.compact {
		return dataPayloadOffsetCompact
	}
	return dataPayloadOffsetRegular
}

// namePrefixBytes is how much of a value is decoded to learn its field
// name: systemd's longest field name, 64 bytes, and the '='.
const namePrefixBytes = 64 + 1

// nameStoredLimit is the most stored bytes of a compressed value the
// name search reads: 129 KiB. A value whose name lies further on in its
// stored bytes is damage. The limit holds a zstd block of 128 KiB with
// the frame's and the block's headers and the checksum, and so the start
// of the text of any zstd value a compressor writes; it is more than an
// xz stream needs for its first 4 KiB of text (a block header of at most
// 1 KiB and one LZMA2 chunk of at most 64 KiB). For lz4, only a run of
// literals can place the name beyond the limit, never a match. A run of
// L literal bytes is announced by about L/255 length bytes before its
// first byte, so when the first 65 bytes of a value's text reach into a
// run longer than about 32 MiB, of bytes lz4 finds no match in, the name
// search needs more stored bytes than the limit and the value is damage
// here, though systemd, which reads the length bytes however many there
// are, gives its name. A match of any length is written as its offset
// and then its length bytes, and the name search stops reading those
// once the match covers the bytes it still lacks (decodeLZ4Prefix).
const nameStoredLimit = 129 << 10

// readData reads the DATA object at offset and returns its payload when
// it holds a field the renderer reads, and nil when it holds any other
// field or no '='. A DATA object that fails its checks, or whose value
// does not decode as far as the answer needs, is damage; so is a value of
// a field the renderer reads whose stored bytes pass 128 MiB, which is
// refused before it is read whole. A value of another field is never
// damage for its size.
//
// room is what the budget of the entry being read has left for the
// payload of a field the renderer reads (maxEntryValues at most). A
// payload that would pass it is refused with errPastEntryBudget, before
// it is read whole when it is stored plain, and before it is decoded
// when its stored bytes declare its size.
//
// Reads: the object's first bytes come with one ReadAt into the walker's
// probe. When the probe does not hold what the answer needs, one more
// ReadAt reads it: nothing for an uncompressed value of a field the
// renderer does not read (the probe holds its name); the whole stored
// payload for an uncompressed value of a field it reads; up to
// nameStoredLimit stored bytes for the name of a compressed value. A
// compressed value of a field the renderer reads is then decoded from
// those bytes, and only one whose stored bytes pass nameStoredLimit takes
// a third ReadAt, for the rest. Every other object takes at most two, and
// a value of a field the renderer does not read is read at most as far as
// the probe and nameStoredLimit stored bytes.
//
// The payload returned is the caller's; it does not alias the walker's
// buffers.
func (w *walker) readData(offset uint64, room int) ([]byte, error) {
	f := w.f
	start := f.dataPayloadOffset()
	o, err := f.readObject(offset, objectData, start+1, w.dataProbe)
	if err != nil {
		return nil, err
	}
	what := objectRef{objectData, offset}
	if err = checkDataLinks(o, what); err != nil {
		return nil, err
	}
	storedSize := o.size - start
	compressed := o.flags & objectCompressedMask
	decoder, isCompressed := payloadDecoders[compressed]
	switch {
	case compressed != 0 && !isCompressed:
		return nil, damaged("%s has the compression flags %#x, more than one compression", what, compressed)
	case !isCompressed:
		// The probe holds the payload's first bytes: 950 at least, or
		// all of them.
		if !namesRenderedField(o.head[start:]) {
			return nil, nil
		}
		if err = checkShownSize(storedSize, what); err != nil {
			return nil, err
		}
		if storedSize > uint64(room) { // #nosec G115 -- room is not negative
			return nil, errPastEntryBudget
		}
		return f.ownStored(o, start, o.head[start:], what)
	}
	// Compressed: the name first, from at most nameStoredLimit stored
	// bytes; the whole value only for a field the renderer reads.
	nameStored := w.nameStoredBuffer(min(storedSize, nameStoredLimit))
	if err = f.readStored(nameStored, o, start, o.head[start:], what); err != nil {
		return nil, err
	}
	name, err := w.decodeName(nameStored, storedSize > nameStoredLimit, compressed, decoder.name, what)
	w.decoded += int64(len(name))
	if err != nil || !namesRenderedField(name) {
		return nil, err
	}
	if err = checkShownSize(storedSize, what); err != nil {
		return nil, err
	}
	stored := nameStored
	if storedSize > nameStoredLimit {
		if stored, err = f.ownStored(o, start, nameStored, what); err != nil {
			return nil, err
		}
	}
	// The decoder writes the value into a new slice, so stored may be the
	// walker's buffer.
	value, err := decodeStored(decoder, stored, room, what)
	w.decoded += int64(len(value))
	return value, err
}

// checkShownSize refuses the stored value of a field the renderer reads
// when it passes 128 MiB, before it is read whole.
func checkShownSize(storedSize uint64, what objectRef) error {
	if storedSize > maxValue {
		return damaged("%s holds a value of %d bytes, more than 128 MiB", what, storedSize)
	}
	return nil
}

// namesRenderedField reports whether payload, or its first bytes, starts
// with the name of a field the renderer reads and '='.
func namesRenderedField(payload []byte) bool {
	name, _, hasSeparator := bytes.Cut(payload[:min(len(payload), namePrefixBytes)], []byte("="))
	if !hasSeparator {
		return false
	}
	_, read := renderedField(name)
	return read
}

// readStored fills dst with the first len(dst) stored payload bytes of
// the DATA object o: those of have, the first stored bytes already read
// (the probe's at least), and the rest with one ReadAt. The caller has
// checked that dst ends within the object.
func (f *journalFile) readStored(dst []byte, o object, start uint64, have []byte, what objectRef) error {
	copied := copy(dst, have)
	if copied == len(dst) {
		return nil
	}
	rest := dst[copied:]
	_, err := f.readAtLeast(rest, o.offset+start+uint64(copied), len(rest)) // #nosec G115 -- a byte count
	if errors.Is(err, errShortRead) {
		return damaged("the file ends inside %s", what)
	}
	return err
}

// ownStored returns the whole stored payload of the DATA object o in a
// slice of its own, from the bytes of have and one ReadAt for the rest.
// The caller has checked that the payload is at most maxValue bytes.
func (f *journalFile) ownStored(o object, start uint64, have []byte, what objectRef) ([]byte, error) {
	stored := make([]byte, o.size-start)
	if err := f.readStored(stored, o, start, have, what); err != nil {
		return nil, err
	}
	return stored, nil
}

// nameStoredBuffer returns the walker's buffer for the stored bytes of a
// name search, n bytes long; it grows to nameStoredLimit at most and is
// reused for every name search.
func (w *walker) nameStoredBuffer(n uint64) []byte {
	if uint64(cap(w.nameStored)) < n {
		w.nameStored = make([]byte, n, nameStoredLimit)
	}
	return w.nameStored[:n]
}

// storedReader reads a value's stored bytes from memory for a name
// search, and notes when a read finds no more (atEnd), so that a value
// cut at nameStoredLimit can be told from one whose stream ends there. It
// has no Bytes method on purpose: given a reader with Bytes and Len, the
// zstd decoder's Reset would decode the whole frame at once.
type storedReader struct {
	b     []byte
	atEnd bool
}

func (s *storedReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		s.atEnd = true
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// decodeName decodes the first namePrefixBytes bytes of the text of a
// compressed value from its stored bytes, or all of it when it is
// shorter. cut says the stored bytes stop at nameStoredLimit before the
// value's end: running out of them then is damage, as the name lies
// further on. The returned slice is valid until the next call.
//
// The decoder goes only as far as it must to give those bytes: for zstd
// its first block, and the frame checksum when that block is the last;
// for xz about 4 KiB of text, through its dictionary buffer; for lz4 the
// sequences that hold them, and of a long match only the length bytes
// those bytes need.
func (w *walker) decodeName(stored []byte, cut bool, compressed uint8, codec string, what objectRef) ([]byte, error) {
	var n int
	var atEnd bool
	var err error
	if compressed == objectCompressedLZ4 {
		n, atEnd, err = decodeLZ4Prefix(stored, w.nameBuf[:])
	} else {
		w.stored = storedReader{b: stored}
		var r io.Reader
		if r, err = w.namePrefixReader(compressed, &w.stored); err == nil {
			n, err = readPrefix(r, w.nameBuf[:])
		}
		atEnd = w.stored.atEnd
	}
	switch {
	case cut && atEnd && (err != nil || n < namePrefixBytes):
		return nil, damaged("%s needs more than %d of its stored bytes to give its field name (%s)", what, nameStoredLimit, codec)
	case err == nil:
		return w.nameBuf[:n], nil
	case errors.Is(err, compression.ErrDictionaryTooLarge):
		// ErrDictionaryTooLarge wraps ErrTooLargeToDecode, so it is told
		// apart first: the bound it reports is maxXZDictionary.
		return nil, xzDictionaryDamage(what)
	case errors.Is(err, compression.ErrTooLargeToDecode), compression.IsWindowRefusal(err):
		return nil, damaged("%s decompresses to more than 128 MiB (%s)", what, codec)
	}
	return nil, damaged("%s does not decompress (%s)", what, codec)
}

// errLZ4Truncated is decodeLZ4Prefix's error for a block that ends inside
// a sequence before the bytes asked for.
var errLZ4Truncated = fmt.Errorf("%w: the lz4 block ends inside a sequence", errNotDecoded)

// decodeLZ4Prefix decodes into dst the first len(dst) bytes of the text of
// a value systemd stores with lz4 (its decoded size as a little-endian
// uint64, then one raw block), or all of it when it is shorter, as
// systemd's decompress_startswith_lz4 does with LZ4_decompress_safe_partial
// (compress.c). It returns the bytes decoded, and atEnd when it used every
// stored byte it was given.
//
// The lz4 block format is a run of sequences. Each starts with a token
// byte: its high four bits are the number of literal bytes that follow,
// its low four bits the length of the match after them, less 4; a value
// of 15 continues in the next bytes, each added until one is not 255.
// After the literals come a 2-byte little-endian offset back into the
// text and the match, copied from there byte by byte (it may overlap
// what it writes). The last sequence ends after its literals.
//
// It stops as soon as dst is full, and reads only the stored bytes that
// dst needs. Of a long run of literals, those are all of its length
// bytes, which come before the literals, and the literals it copies. Of a
// long match, those are its offset and only the length bytes that cover
// what dst still lacks: they come after the offset, and each one after
// can only make the match longer. So a value whose text is a short name
// and a match of tens of MiB, written with one length byte per 255 bytes
// of the match, gives its name from a few stored bytes. A match at
// offset 0 or one that reaches before the start of the text, or a block
// that ends inside a sequence before dst is full, is an error.
//
// It does not read the size the value declares, as systemd's partial
// decode does not. A declared size shorter than the text must not cut
// what it gives: cut before the '=' of a field the text shows, the name
// would pass as one of a field the text does not show, and the entry
// would be shown without the field instead of left out. The whole decode
// of a shown value (decodeLZ4) holds the block to the declared size.
func decodeLZ4Prefix(stored, dst []byte) (n int, atEnd bool, err error) {
	if len(stored) <= lz4SizePrefix {
		return 0, true, fmt.Errorf("%w: %d bytes, no block after the size", errNotDecoded, len(stored))
	}
	src := stored[lz4SizePrefix:]
	ip := 0 // the next byte of src to read
	// length reads the continuation bytes of a length whose 4 bits were
	// 15, starting from base. It stops early, with the length read so
	// far, once that reaches needed: each byte after can only add to it.
	// It returns false when src ends inside the length bytes it reads.
	length := func(base, needed int) (int, bool) {
		for base < needed {
			if ip >= len(src) {
				return 0, false
			}
			b := src[ip]
			ip++
			base += int(b)
			if b != 255 {
				return base, true
			}
		}
		return base, true
	}
	for n < len(dst) {
		if ip >= len(src) {
			return n, true, nil // the block ended after its last literals
		}
		token := src[ip]
		ip++
		literals := int(token >> 4)
		if literals == 15 {
			// The literals come after their length bytes, so every one of
			// those is read, however many there are.
			var ok bool
			if literals, ok = length(literals, math.MaxInt); !ok {
				return n, true, errLZ4Truncated
			}
		}
		available := len(src) - ip
		copied := copy(dst[n:], src[ip:ip+min(literals, available)])
		n += copied
		if n == len(dst) {
			return n, false, nil
		}
		if literals > available {
			return n, true, errLZ4Truncated
		}
		ip += literals
		if ip == len(src) {
			return n, true, nil // the last sequence: literals only
		}
		if ip+2 > len(src) {
			return n, true, errLZ4Truncated
		}
		offset := int(src[ip]) | int(src[ip+1])<<8
		ip += 2
		if offset == 0 || offset > n {
			return n, false, fmt.Errorf("%w: a match at offset %d of %d bytes of text", errNotDecoded, offset, n)
		}
		match := int(token&15) + 4
		if token&15 == 15 {
			// The match copies text already decoded, so its length bytes
			// are read only until the match covers the bytes dst still
			// lacks; it then fills dst and the loop ends.
			var ok bool
			if match, ok = length(match, len(dst)-n); !ok {
				return n, true, errLZ4Truncated
			}
		}
		// Byte by byte, as the match may overlap the bytes it writes.
		for ; match > 0 && n < len(dst); match-- {
			dst[n] = dst[n-offset]
			n++
		}
	}
	return n, false, nil
}

// namePrefixReader returns a reader of the text of the zstd or xz stream
// stored that decodes only as far as it is read.
//
// zstd: one streaming decoder serves the whole walk (Reset starts it on
// a new stream); with one goroutine (WithDecoderConcurrency(1)) it decodes
// on the caller's goroutine, one block (at most 128 KiB of text) at a
// time. It holds a window as large as the largest frame read declares,
// at most WindowLimit, allocated once and reused.
//
// xz: compression.NewXzPrefixReader, whose dictionary is namePrefixBytes
// (4 KiB at least), whatever the stream declares. A stream that declares
// a dictionary above maxXZDictionary is refused, as the whole decode
// refuses it, so that the value of a field the text does not show is
// held to the bound of a shown one.
func (w *walker) namePrefixReader(compressed uint8, stored io.Reader) (io.Reader, error) {
	if compressed == objectCompressedXZ {
		return compression.NewXzPrefixReader(stored, maxXZDictionary, namePrefixBytes)
	}
	if w.zstdNames == nil {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(compression.WindowLimit),
			zstd.WithDecoderMaxMemory(compression.WindowLimit))
		if err != nil {
			return nil, err
		}
		w.zstdNames = d
	}
	if err := w.zstdNames.Reset(stored); err != nil {
		return nil, err
	}
	return w.zstdNames, nil
}

// readPrefix reads from r until buf is full or the text ends, and
// returns how many bytes it read. Only io.EOF ends the text; any other
// error, io.ErrUnexpectedEOF included (a stream cut short), is returned.
func readPrefix(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		k, err := r.Read(buf[n:])
		n += k
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// decodeStored decodes the payload of a compressed DATA object of a
// field the renderer reads, a failure being damage. The size its stored
// bytes declare is checked first (checkDeclaredSize), and the decoder
// stops at room, what the entry's budget has left; a payload that passes
// room gives errPastEntryBudget, which is not damage of the object.
func decodeStored(decoder payloadDecoder, stored []byte, room int, what objectRef) ([]byte, error) {
	if err := checkDeclaredSize(decoder, stored, room, what); err != nil {
		return nil, err
	}
	value, err := decoder.decode(stored, room)
	switch {
	case errors.Is(err, errPastEntryBudget):
		return nil, err
	case errors.Is(err, errValueTooLarge):
		return nil, damaged("%s decompresses to more than 128 MiB (%s)", what, decoder.name)
	case errors.Is(err, errXZDictionaryTooLarge):
		return nil, xzDictionaryDamage(what)
	case err != nil:
		return nil, damaged("%s does not decompress (%s)", what, decoder.name)
	}
	return value, nil
}

// checkDeclaredSize checks the decoded size the stored bytes of a shown
// value declare, before the decoder makes a buffer of that size. Above
// 128 MiB it is damage, as a value that decodes past it is. Above the
// most text the stored bytes can hold it is damage too: no stream of
// that format gives it, and a value of a few stored bytes that declares
// 128 MiB would otherwise cost a 128 MiB buffer per DATA object that
// holds one. Above room, what the entry's budget has left, it is
// errPastEntryBudget. A format that declares no size up front (xz) is
// held to the same bounds as it decodes instead.
func checkDeclaredSize(decoder payloadDecoder, stored []byte, room int, what objectRef) error {
	if decoder.declaredSize == nil {
		return nil
	}
	declared := decoder.declaredSize(stored)
	switch {
	case declared > maxValue:
		return damaged("%s decompresses to more than 128 MiB (%s)", what, decoder.name)
	case declared > decoder.mostText(len(stored)):
		return damaged("%s declares %d bytes of text, which its %d stored bytes cannot hold (%s)", what, declared, len(stored), decoder.name)
	case declared > uint64(room): // #nosec G115 -- room is not negative
		return errPastEntryBudget
	}
	return nil
}

// checkDataLinks checks the links of a DATA object as check_object does
// (journal-file.c:939-962): its first entry and its entry count are both
// 0 or both set, and every offset it holds is a multiple of 8.
func checkDataLinks(o object, what objectRef) error {
	entryOffset, nEntries := le.Uint64(o.head[offsetDataEntryOffset:]), le.Uint64(o.head[offsetDataNEntries:])
	if (entryOffset == 0) != (nEntries == 0) {
		return damaged("%s links to its first entry at offset %d but counts %d entries", what, entryOffset, nEntries)
	}
	for _, at := range []int{offsetDataNextHash, offsetDataNextField, offsetDataEntryOffset, offsetDataEntryArray} {
		if le.Uint64(o.head[at:])%8 != 0 {
			return damaged("%s holds the offset %d, not a multiple of 8", what, le.Uint64(o.head[at:]))
		}
	}
	return nil
}

// decodeZstd decodes the zstd frames of stored with a pooled decoder,
// into a value of at most limit bytes. The pool's decoders refuse a
// frame that declares more than WindowLimit and stop one that grows past
// it. DecodeAll makes the buffer of a frame that declares its content
// size from that size, before it decodes the frame, so the caller checks
// the declared sizes first (checkDeclaredSize, zstdDeclaredSize). A frame
// that declares no size grows its buffer as it decodes, and is held to
// limit once it is decoded; systemd writes no such frame.
func decodeZstd(stored []byte, limit int) ([]byte, error) {
	d := compression.AcquireDecoder()
	// defer runs ReleaseDecoder when decodeZstd returns, so the decoder
	// goes back to the pool whichever way it returns.
	defer compression.ReleaseDecoder(d)
	value, err := d.DecodeAll(stored, nil)
	switch {
	case compression.IsWindowRefusal(err):
		return nil, errValueTooLarge
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errNotDecoded, err)
	case len(value) > limit:
		return nil, tooLarge(limit)
	}
	return value, nil
}

// The zstd block sizes the bound of zstdMostText rests on.
const (
	// zstdMaxBlockText is the most text one zstd block gives, of any
	// kind: 128 KiB (Block_Maximum_Size, RFC 8878 section 3.1.1.2.3).
	zstdMaxBlockText = 128 << 10
	// zstdLeastBlockWithText is the fewest stored bytes of a block that
	// gives text: its 3-byte header and one byte. An RLE block, one byte
	// repeated, is exactly that; a raw block holds each byte of its text;
	// a compressed block holds at least a literals header and a sequences
	// header after its own header.
	zstdLeastBlockWithText = 4
	// zstdBlockHeaderBytes is the size of a block header.
	zstdBlockHeaderBytes = 3
	// zstdChecksumBytes is the size of the checksum after a frame's last
	// block, when its header asks for one.
	zstdChecksumBytes = 4
)

// zstdMostText returns the most text n stored bytes of zstd frames can
// decode to. Every block that gives text takes at least 4 stored bytes
// and gives at most 128 KiB, and frame headers (6 bytes at least),
// skippable frames and checksums give none, so n bytes hold at most n/4
// such blocks; one block more is a margin. A real value never comes near
// the bound: 128 MiB of zero bytes, the densest text there is, is 1,024
// RLE blocks in 4 KiB and a frame header, which allow more than 128 MiB.
func zstdMostText(n int) uint64 {
	return (uint64(n)/zstdLeastBlockWithText + 1) * zstdMaxBlockText // #nosec G115 -- a byte count
}

// zstdDeclaredSize returns the sum of the content sizes the zstd frames
// of stored declare, reading only frame and block headers. A frame that
// declares no size adds nothing.
//
// SECURITY: DecodeAll decodes every frame of its input, one after the
// other, and makes each frame's buffer from the size that frame
// declares, so the sizes of all the frames are added, not only the first
// one's. systemd writes one frame per value; a value of several frames
// comes from a crafted file, which could otherwise hide a frame that
// declares 128 MiB behind a small first one.
//
// The walk takes the frames as the decoder reads them: a skippable frame
// is passed over by the size its header gives, a frame's blocks by their
// headers (a raw or compressed block holds as many bytes as its header
// says, an RLE block one), and the checksum after a frame's last block
// when its header asks for one. It ends where the bytes end, or where a
// header does not parse or a block runs past the end: the decoder stops
// with an error at that place too, so it sizes no frame after it.
func zstdDeclaredSize(stored []byte) uint64 {
	var declared uint64
	for len(stored) > 0 {
		var h zstd.Header
		// DecodeAndStrip reads a frame header (or a skippable frame's) and
		// returns the bytes after it.
		rest, err := h.DecodeAndStrip(stored)
		if err != nil {
			return declared
		}
		if h.Skippable {
			if uint64(h.SkippableSize) > uint64(len(rest)) {
				return declared
			}
			stored = rest[h.SkippableSize:]
			continue
		}
		if h.HasFCS {
			// The sum stops at the largest uint64: any size that large is
			// refused all the same.
			declared += min(h.FrameContentSize, math.MaxUint64-declared)
		}
		rest, ok := skipZstdBlocks(rest)
		if !ok {
			return declared
		}
		if h.HasCheckSum {
			if len(rest) < zstdChecksumBytes {
				return declared
			}
			rest = rest[zstdChecksumBytes:]
		}
		stored = rest
	}
	return declared
}

// skipZstdBlocks passes over the blocks of a frame, from the first block
// header in b to the end of its last block, and returns the bytes after
// it. It returns false when b ends first or a block has the reserved
// type, which the decoder refuses too. A block header is 3 bytes, little
// endian: bit 0 says the block is the frame's last, bits 1-2 give its
// type (0 raw, 1 RLE, 2 compressed, 3 reserved) and bits 3-23 its size.
func skipZstdBlocks(b []byte) ([]byte, bool) {
	const (
		rleBlock      = 1
		reservedBlock = 3
	)
	for {
		if len(b) < zstdBlockHeaderBytes {
			return nil, false
		}
		header := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		b = b[zstdBlockHeaderBytes:]
		size := int(header >> 3)
		switch (header >> 1) & 3 {
		case rleBlock:
			size = 1 // the byte it repeats; its size is the text's
		case reservedBlock:
			return nil, false
		}
		if size > len(b) {
			return nil, false
		}
		b = b[size:]
		if header&1 != 0 {
			return b, true
		}
	}
}

// decodeXZ decodes the xz streams of stored into a value of at most
// limit bytes. A stream does not say its decoded size up front, so the
// value is read into a buffer that grows, and the read stops once it
// passes limit. A block that declares a dictionary above maxXZDictionary,
// in any stream, is refused before its dictionary is made.
func decodeXZ(stored []byte, limit int) ([]byte, error) {
	r, err := compression.NewXzReader(bytes.NewReader(stored), maxXZDictionary)
	if err == nil {
		var value []byte
		if value, err = readValue(r, limit); err == nil {
			return value, nil
		}
	}
	switch {
	case errors.Is(err, errValueTooLarge):
		return nil, tooLarge(limit)
	case errors.Is(err, compression.ErrDictionaryTooLarge):
		return nil, errXZDictionaryTooLarge
	}
	return nil, fmt.Errorf("%w: %w", errNotDecoded, err)
}

// firstValueBuffer is the first size of the buffer readValue reads into.
const firstValueBuffer = 4 << 10

// readValue reads r to its end into one buffer that doubles when it is
// full, so that a value of n bytes costs about 2n bytes of buffers (an
// append-grown buffer, as io.ReadAll's, grows by a quarter at large sizes
// and costs about 5n). It returns errValueTooLarge, without growing
// further, once the value passes limit; the buffer never holds more than
// limit bytes.
func readValue(r io.Reader, limit int) ([]byte, error) {
	buf := make([]byte, 0, min(firstValueBuffer, limit))
	for {
		if len(buf) == cap(buf) {
			// Read one byte before growing, so that a value that fills
			// the buffer exactly does not double it to find its end.
			var probe [1]byte
			n, err := r.Read(probe[:])
			if n == 0 {
				if errors.Is(err, io.EOF) {
					return buf, nil
				}
				if err != nil {
					return nil, err
				}
				continue
			}
			if len(buf) == limit {
				return nil, errValueTooLarge
			}
			grown := make([]byte, len(buf), min(2*cap(buf), limit))
			copy(grown, buf)
			grown = append(grown, probe[0])
			buf = grown
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			if err != nil {
				return nil, err
			}
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// lz4SizePrefix is the size of the decoded-size field before an lz4
// block.
const lz4SizePrefix = 8

// lz4MaxRatio bounds the text of an lz4 block: less than 255 bytes per
// stored byte. A literal costs one stored byte. A match costs a token, a
// 2-byte offset and, past its first 19 bytes, one length byte per 255
// bytes or fewer, so a match of 255m + 18 bytes at most costs m + 3.
const lz4MaxRatio = 255

// lz4DeclaredSize returns the decoded size an lz4 value declares, the
// uint64 before its block, and 0 when it holds nothing after that size
// (decodeLZ4 refuses it).
func lz4DeclaredSize(stored []byte) uint64 {
	if len(stored) <= lz4SizePrefix {
		return 0
	}
	return le.Uint64(stored)
}

// lz4MostText returns the most text the block of an lz4 value of n
// stored bytes can decode to.
func lz4MostText(n int) uint64 {
	return lz4MaxRatio * uint64(max(n-lz4SizePrefix, 0)) // #nosec G115 -- not negative
}

// decodeLZ4 decodes systemd's lz4 framing: the decoded size, then a raw
// block that must decode to exactly that many bytes
// (decompress_blob_lz4, compress.c). A declared size above limit is
// refused before the value's buffer is made from it.
func decodeLZ4(stored []byte, limit int) ([]byte, error) {
	if len(stored) <= lz4SizePrefix {
		return nil, fmt.Errorf("%w: %d bytes, no block after the size", errNotDecoded, len(stored))
	}
	size := le.Uint64(stored)
	if size > uint64(limit) { // #nosec G115 -- limit is not negative
		return nil, tooLarge(limit)
	}
	value := make([]byte, size)
	n, err := lz4.UncompressBlock(stored[lz4SizePrefix:], value)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNotDecoded, err)
	}
	if uint64(n) != size { // #nosec G115 -- n is a byte count
		return nil, fmt.Errorf("%w: the block holds %d bytes, its size says %d", errNotDecoded, n, size)
	}
	return value, nil
}

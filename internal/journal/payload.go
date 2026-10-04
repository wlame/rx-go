package journal

// This file reads the payload of a DATA object, "FIELD=value", and
// decodes it when it is stored compressed. The framing of each
// compression is systemd's (src/basic/compress.c, v257.13): an xz stream,
// a zstd frame, or for lz4 the decoded size as a little-endian uint64
// followed by one raw lz4 block.
//
// SECURITY: a stored value of more than compression.WindowLimit (128
// MiB) is damage, in any field, before any of it is read. The renderer
// reads seven fields. A value of any other field is checked as far as its
// decoder must go to give the name (zstd: its first block, and the frame
// checksum when that block is the last; xz: about 4 KiB of text through
// its dictionary buffer), from at most nameStoredLimit of its stored
// bytes read at once, so that a value of a few stored bytes that decodes
// to 128 MiB costs a block of decoding, not 128 MiB per entry that lists
// it. An lz4 value is decoded whole, since lz4 decodes at most 255 bytes
// per stored byte. A value of a field the renderer reads may decode to at
// most 128 MiB; a larger one is damage. A size the file declares is
// checked against that limit before any buffer is made from it, and a
// decoder stops at the limit whatever the stream holds.

import (
	"bytes"
	"errors"
	"fmt"
	"io"

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

// payloadDecoder decodes one compressed payload.
type payloadDecoder struct {
	name   string
	decode func(stored []byte) ([]byte, error)
}

// payloadDecoders holds the decoder of each compression flag.
var payloadDecoders = map[uint8]payloadDecoder{
	objectCompressedXZ:   {"xz", decodeXZ},
	objectCompressedLZ4:  {"lz4", decodeLZ4},
	objectCompressedZstd: {"zstd", decodeZstd},
}

// errValueTooLarge is a decoder's error for a value larger than maxValue,
// and errNotDecoded its error for bytes that do not decode.
var (
	errValueTooLarge = errors.New("the value is larger than 128 MiB")
	errNotDecoded    = errors.New("the value does not decompress")
)

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

// nameStoredLimit is the most stored bytes of a zstd or xz value the
// name search reads: 129 KiB. That holds a zstd block of 128 KiB with the
// frame's and the block's headers and the checksum, and so the start of
// the text of any zstd value a compressor writes; and it is more than an
// xz stream needs for its first 4 KiB of text (a block header of at most
// 1 KiB and one LZMA2 chunk of at most 64 KiB). A value whose name lies
// further on in its stored bytes is damage.
const nameStoredLimit = 129 << 10

// readData reads the DATA object at offset and returns its payload when
// it holds a field the renderer reads, and nil when it holds any other
// field or no '='. A DATA object that fails its checks, whose stored
// value is over 128 MiB, or whose value does not decode as far as the
// answer needs, is damage.
//
// Reads: the object's first bytes come with one ReadAt into the walker's
// probe. When the probe does not hold the stored payload, one more ReadAt
// reads the rest the answer needs: nothing for an uncompressed value of
// a field the renderer does not read (the probe holds its name); the
// whole stored payload for an uncompressed or lz4 value otherwise; up to
// nameStoredLimit stored bytes for the name of a zstd or xz value. A zstd
// or xz value of a field the renderer reads is then decoded from those
// bytes, and only one whose stored bytes pass nameStoredLimit takes a
// third ReadAt, for the rest. Every other object takes at most two.
//
// The payload returned is the caller's; it does not alias the walker's
// buffers.
func (w *walker) readData(offset uint64) ([]byte, error) {
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
	case storedSize > maxValue:
		// Checked before any byte of the value is read, for every field.
		return nil, damaged("%s holds a value of %d bytes, more than 128 MiB", what, storedSize)
	case !isCompressed:
		// The probe holds the payload's first bytes: 950 at least, or
		// all of them.
		if !namesRenderedField(o.head[start:]) {
			return nil, nil
		}
		return f.ownStored(o, start, o.head[start:], what)
	case compressed == objectCompressedLZ4:
		var stored, value []byte
		if stored, err = f.ownStored(o, start, o.head[start:], what); err != nil {
			return nil, err
		}
		value, err = decodeStored(decoder, stored, what)
		w.decoded += int64(len(value))
		if err != nil || !namesRenderedField(value) {
			return nil, err
		}
		return value, nil
	}
	// zstd or xz: the name first, from at most nameStoredLimit stored
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
	stored := nameStored
	if storedSize > nameStoredLimit {
		if stored, err = f.ownStored(o, start, nameStored, what); err != nil {
			return nil, err
		}
	}
	// The decoder writes the value into a new slice, so stored may be the
	// walker's buffer.
	value, err := decodeStored(decoder, stored, what)
	w.decoded += int64(len(value))
	return value, err
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
// zstd or xz value from its stored bytes, or all of it when it is
// shorter. cut says the stored bytes stop at nameStoredLimit before the
// value's end: running out of them then is damage, as the name lies
// further on. The returned slice is valid until the next call.
//
// The decoder goes only as far as it must to give those bytes: for zstd
// its first block, and the frame checksum when that block is the last;
// for xz about 4 KiB of text, through its dictionary buffer.
func (w *walker) decodeName(stored []byte, cut bool, compressed uint8, codec string, what objectRef) ([]byte, error) {
	w.stored = storedReader{b: stored}
	n := 0
	r, err := w.namePrefixReader(compressed, &w.stored)
	if err == nil {
		n, err = readPrefix(r, w.nameBuf[:])
	}
	switch {
	case cut && w.stored.atEnd && (err != nil || n < namePrefixBytes):
		return nil, damaged("%s needs more than %d of its stored bytes to give its field name (%s)", what, nameStoredLimit, codec)
	case err == nil:
		return w.nameBuf[:n], nil
	case errors.Is(err, compression.ErrTooLargeToDecode), compression.IsWindowRefusal(err):
		return nil, damaged("%s decompresses to more than 128 MiB (%s)", what, codec)
	}
	return nil, damaged("%s does not decompress (%s)", what, codec)
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
// (4 KiB at least), whatever the stream declares.
func (w *walker) namePrefixReader(compressed uint8, stored io.Reader) (io.Reader, error) {
	if compressed == objectCompressedXZ {
		return compression.NewXzPrefixReader(stored, compression.WindowLimit, namePrefixBytes)
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

// decodeStored decodes the payload of a compressed DATA object, a failure
// being damage.
func decodeStored(decoder payloadDecoder, stored []byte, what objectRef) ([]byte, error) {
	value, err := decoder.decode(stored)
	switch {
	case errors.Is(err, errValueTooLarge):
		return nil, damaged("%s decompresses to more than 128 MiB (%s)", what, decoder.name)
	case err != nil:
		return nil, damaged("%s does not decompress (%s)", what, decoder.name)
	}
	return value, nil
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

// decodeZstd decodes a zstd frame with a pooled decoder. The pool's
// decoders refuse a frame that declares more than WindowLimit and stop
// one that grows past it, before they allocate for it.
func decodeZstd(stored []byte) ([]byte, error) {
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
	case len(value) > maxValue:
		return nil, errValueTooLarge
	}
	return value, nil
}

// decodeXZ decodes an xz stream. The stream does not say its decoded
// size up front, so the value is read into a buffer that grows.
func decodeXZ(stored []byte) ([]byte, error) {
	r, err := compression.NewXzReader(bytes.NewReader(stored), compression.WindowLimit)
	if err == nil {
		var value []byte
		if value, err = readValue(r); err == nil {
			return value, nil
		}
	}
	switch {
	case errors.Is(err, errValueTooLarge), errors.Is(err, compression.ErrTooLargeToDecode):
		return nil, errValueTooLarge
	}
	return nil, fmt.Errorf("%w: %w", errNotDecoded, err)
}

// firstValueBuffer is the first size of the buffer readValue reads into.
const firstValueBuffer = 4 << 10

// readValue reads r to its end into one buffer that doubles when it is
// full, so that a value of n bytes costs about 2n bytes of buffers (an
// append-grown buffer, as io.ReadAll's, grows by a quarter at large sizes
// and costs about 5n). It returns errValueTooLarge, without growing
// further, once the value passes maxValue.
func readValue(r io.Reader) ([]byte, error) {
	buf := make([]byte, 0, firstValueBuffer)
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
			if len(buf) == maxValue {
				return nil, errValueTooLarge
			}
			grown := make([]byte, len(buf), min(2*cap(buf), maxValue))
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

// decodeLZ4 decodes systemd's lz4 framing: the decoded size, then a raw
// block that must decode to exactly that many bytes
// (decompress_blob_lz4, compress.c).
func decodeLZ4(stored []byte) ([]byte, error) {
	if len(stored) <= lz4SizePrefix {
		return nil, fmt.Errorf("%w: %d bytes, no block after the size", errNotDecoded, len(stored))
	}
	size := le.Uint64(stored)
	if size > maxValue {
		return nil, errValueTooLarge
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

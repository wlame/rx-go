package journal

// This file reads the payload of a DATA object, "FIELD=value", and
// decodes it when it is stored compressed. The framing of each
// compression is systemd's (src/basic/compress.c, v257.13): an xz stream,
// a zstd frame, or for lz4 the decoded size as a little-endian uint64
// followed by one raw lz4 block.
//
// SECURITY: the renderer reads seven fields. A value of any other field
// is decoded only as far as its name (namePrefixBytes), whatever its
// size, so that a value of a few stored bytes that decodes to 128 MiB
// costs a few bytes of decoding, not 128 MiB per entry that lists it;
// an lz4 value is decoded whole, since lz4 decodes at most 255 bytes
// per stored byte. A value of a field the renderer reads may be at most
// compression.WindowLimit (128 MiB), stored or decoded; a larger one is
// damage. A size the file declares is checked against that limit before
// any buffer is made from it, and a decoder stops at the limit whatever
// the stream holds.

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

// readData reads the DATA object at offset and returns its payload when
// it holds a field the renderer reads, and nil when it holds any other
// field or no '='. A DATA object that fails its checks, or whose value
// does not decode as far as the answer needs, is damage.
//
// The object's first bytes come with one ReadAt into the walker's probe;
// a value of a field the renderer reads takes a second ReadAt when the
// probe does not hold it. The payload returned is the caller's; it does
// not alias the probe.
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
		return f.storedValue(o, start, what)
	case compressed == objectCompressedLZ4:
		var value []byte
		value, err = f.decodeValue(o, start, decoder, what)
		w.decoded += int64(len(value))
		if err != nil || !namesRenderedField(value) {
			return nil, err
		}
		return value, nil
	}
	// zstd or xz: the name first, the whole value only for a field the
	// renderer reads.
	name, err := w.decodeName(o, start, compressed, decoder.name, what)
	w.decoded += int64(len(name))
	if err != nil || !namesRenderedField(name) {
		return nil, err
	}
	value, err := f.decodeValue(o, start, decoder, what)
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

// storedValue returns the payload of an uncompressed DATA object,
// refusing one of more than 128 MiB before it is read.
func (f *journalFile) storedValue(o object, start uint64, what objectRef) ([]byte, error) {
	if storedSize := o.size - start; storedSize > maxValue {
		return nil, damaged("%s holds a value of %d bytes, more than 128 MiB", what, storedSize)
	}
	return f.storedPayload(o, start, what)
}

// decodeValue decodes the whole payload of a compressed DATA object,
// refusing a stored payload of more than 128 MiB before it is read.
func (f *journalFile) decodeValue(o object, start uint64, decoder payloadDecoder, what objectRef) ([]byte, error) {
	if storedSize := o.size - start; storedSize > maxValue {
		return nil, damaged("%s holds a value of %d bytes, more than 128 MiB", what, storedSize)
	}
	if uint64(len(o.head)) == o.size {
		// The first read holds the whole payload. The decoder writes
		// the value into a new slice, so it may read from the probe.
		return decodeStored(decoder, o.head[start:], what)
	}
	stored, err := f.storedPayload(o, start, what)
	if err != nil {
		return nil, err
	}
	return decodeStored(decoder, stored, what)
}

// decodeName decodes the first namePrefixBytes bytes of the value of a
// zstd or xz DATA object, or all of it when it is shorter, reading the
// stored bytes from the probe and then from the file as the decoder asks
// for them. The returned slice is valid until the next call.
func (w *walker) decodeName(o object, start uint64, compressed uint8, codec string, what objectRef) ([]byte, error) {
	rest := &fileSection{f: w.f, off: o.offset + uint64(len(o.head)), end: o.offset + o.size}
	stored := io.MultiReader(bytes.NewReader(o.head[start:]), rest)
	var name []byte
	r, err := w.namePrefixReader(compressed, stored)
	if err == nil {
		var n int
		n, err = readPrefix(r, w.nameBuf[:])
		name = w.nameBuf[:n]
	}
	switch {
	case err == nil:
		return name, nil
	case errors.Is(rest.err, errShortRead):
		return nil, damaged("the file ends inside %s", what)
	case rest.err != nil:
		// The file could not be read: an I/O error, not damage.
		return nil, rest.err
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
	// stored is a MultiReader, not a *bytes.Buffer: given one of those,
	// Reset would decode the whole frame at once.
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

// fileSection reads the bytes of the journal from off to end, and keeps
// the first error of its reads, so that a failed read can be told from a
// decoder's own error. It reads only within one object, which lies
// before the end of what may be read.
type fileSection struct {
	f        *journalFile
	off, end uint64
	err      error
}

func (s *fileSection) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.off >= s.end {
		return 0, io.EOF
	}
	p = p[:min(uint64(len(p)), s.end-s.off)]
	n, err := s.f.readAtLeast(p, s.off, 1)
	s.off += uint64(n) // #nosec G115 -- a byte count
	if err != nil {
		s.err = err
		return n, err
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

// storedPayload returns the payload bytes of the DATA object o as they
// are stored, in a new slice: copied from the first read when it holds
// them, else completed by a second ReadAt. The caller has checked that
// the payload is at most maxValue bytes.
func (f *journalFile) storedPayload(o object, start uint64, what objectRef) ([]byte, error) {
	stored := make([]byte, o.size-start)
	inHead := o.head[start:] // the payload's first bytes, which the first read holds
	copy(stored, inHead)
	if len(inHead) == len(stored) {
		return stored, nil
	}
	// The rest follows the object's first bytes in the file.
	rest := stored[len(inHead):]
	if _, err := f.readAtLeast(rest, o.offset+uint64(len(o.head)), len(rest)); err != nil {
		if errors.Is(err, errShortRead) {
			return nil, damaged("the file ends inside %s", what)
		}
		return nil, err
	}
	return stored, nil
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

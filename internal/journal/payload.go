package journal

// This file reads the payload of a DATA object, "FIELD=value", and
// decodes it when it is stored compressed. The framing of each
// compression is systemd's (src/basic/compress.c, v257.13): an xz stream,
// a zstd frame, or for lz4 the decoded size as a little-endian uint64
// followed by one raw lz4 block.
//
// SECURITY: a value, stored or decoded, may be at most
// compression.WindowLimit (128 MiB); a larger one is damage. A size the
// file declares is checked against that limit before any buffer is made
// from it, and a decoder stops at the limit whatever the stream holds.

import (
	"bytes"
	"errors"
	"fmt"
	"io"

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

// readPayload reads and decodes the payload of the DATA object at offset,
// with at most two ReadAt calls: the object's first bytes into probe, and
// the rest of a payload they do not hold. The payload returned is the
// caller's; it does not alias probe.
func (f *journalFile) readPayload(offset uint64, probe []byte) ([]byte, error) {
	start := f.dataPayloadOffset()
	o, err := f.readObject(offset, objectData, start+1, probe)
	if err != nil {
		return nil, err
	}
	what := objectRef{objectData, offset}
	if err = checkDataLinks(o, what); err != nil {
		return nil, err
	}
	compressed := o.flags & objectCompressedMask
	decoder, isCompressed := payloadDecoders[compressed]
	if compressed != 0 && !isCompressed {
		return nil, damaged("%s has the compression flags %#x, more than one compression", what, compressed)
	}
	storedSize := o.size - start
	if storedSize > maxValue {
		return nil, damaged("%s holds a value of %d bytes, more than 128 MiB", what, storedSize)
	}
	if isCompressed && uint64(len(o.head)) == o.size {
		// The first read holds the whole payload. The decoder writes
		// the value into a new slice, so it may read from probe.
		return decodeStored(decoder, o.head[start:], what)
	}
	stored, err := f.storedPayload(o, start, what)
	if err != nil || !isCompressed {
		return stored, err
	}
	return decodeStored(decoder, stored, what)
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

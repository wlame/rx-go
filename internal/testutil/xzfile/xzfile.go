// Package xzfile builds xz files for tests: files written by the xz
// writer rx's tests already use, and the same files with a block
// header that declares another dictionary size, the way a crafted file
// declares one.
//
// An xz block header names the dictionary its LZMA2 data needs in one
// byte, and a decoder reserves that much memory before it decodes the
// block. The byte is covered by the header's CRC32, so a test that
// changes it also writes the CRC32 again, as a crafted file would.
package xzfile

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/ulikunitz/xz"
)

// lzma2FilterID and lzma2PropsSize start the one filter entry of an xz
// block header that holds LZMA2 data; the dictionary byte follows them.
const (
	lzma2FilterID  = 0x21
	lzma2PropsSize = 0x01
)

// Block header flags: the reserved bits, and the two that say an
// optional size field follows the flags.
const (
	reservedFlags           = 0x3C
	compressedSizePresent   = 0x40
	uncompressedSizePresent = 0x80
)

// Encode compresses text with the xz writer configured by cfg (its zero
// value writes one block, a CRC64 check and an 8 MiB dictionary).
func Encode(t testing.TB, text []byte, cfg xz.WriterConfig) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := cfg.NewWriter(&out)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	if _, err := w.Write(text); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	return out.Bytes()
}

// BlockHeader is where one LZMA2 block header lies in a file.
type BlockHeader struct {
	Start      int // its first byte, the size byte
	Len        int // its length, CRC32 included
	Dictionary int // its dictionary size byte
}

// BlockHeaders returns every LZMA2 block header of body, in file order.
//
// It finds a block header by what makes one valid: a size byte, flags
// with no reserved bit set, an LZMA2 filter entry, and a CRC32 that
// matches. A stretch of compressed data that passes all of that by
// chance is about one in four billion per position, which no test file
// here comes near.
func BlockHeaders(body []byte) []BlockHeader {
	var found []BlockHeader
	for start := range body {
		if header, ok := blockHeaderAt(body, start); ok {
			found = append(found, header)
		}
	}
	return found
}

// blockHeaderAt returns the block header that starts at start, and
// false when no valid LZMA2 block header starts there.
func blockHeaderAt(body []byte, start int) (BlockHeader, bool) {
	if body[start] == 0 {
		return BlockHeader{}, false
	}
	headerLen := (int(body[start]) + 1) * 4
	if start+headerLen > len(body) {
		return BlockHeader{}, false
	}
	header := body[start : start+headerLen]
	crcAt := headerLen - 4
	if crc32.ChecksumIEEE(header[:crcAt]) != binary.LittleEndian.Uint32(header[crcAt:]) {
		return BlockHeader{}, false
	}
	flags := header[1]
	if flags&reservedFlags != 0 {
		return BlockHeader{}, false
	}
	pos := 2
	for _, present := range []byte{compressedSizePresent, uncompressedSizePresent} {
		if flags&present == 0 {
			continue
		}
		_, n := binary.Uvarint(header[pos:crcAt])
		if n <= 0 {
			return BlockHeader{}, false
		}
		pos += n
	}
	if pos+3 > crcAt || header[pos] != lzma2FilterID || header[pos+1] != lzma2PropsSize {
		return BlockHeader{}, false
	}
	return BlockHeader{Start: start, Len: headerLen, Dictionary: start + pos + 2}, true
}

// WithDictionaryCode returns a copy of body whose block number block
// (from 0) declares the dictionary size code instead of its own, with
// the header's CRC32 written again. A code of 30 declares 128 MiB, 36
// declares 1 GiB, 38 2 GiB and 40 4 GiB less one byte; 16 declares
// 1 MiB and 17 declares 1.5 MiB.
func WithDictionaryCode(t testing.TB, body []byte, block int, code byte) []byte {
	t.Helper()
	headers := BlockHeaders(body)
	if block >= len(headers) {
		t.Fatalf("the file has %d block headers; want block %d", len(headers), block)
	}
	header := headers[block]
	out := bytes.Clone(body)
	out[header.Dictionary] = code
	crcAt := header.Start + header.Len - 4
	binary.LittleEndian.PutUint32(out[crcAt:], crc32.ChecksumIEEE(out[header.Start:crcAt]))
	return out
}

// LZMA2 chunk control bytes: the end of the data, and an uncompressed
// chunk that resets the dictionary or keeps it. An uncompressed chunk
// holds at most 64 KiB.
const (
	lzma2End                   = 0x00
	lzma2StoredResetDictionary = 0x01
	lzma2Stored                = 0x02
	lzma2MaxStoredChunk        = 64 << 10
)

// SizedStoredBlocks returns an xz file of one stream with a block for
// each element of texts, in order. Each block holds its text as
// uncompressed LZMA2 chunks, and its header declares both its
// compressed and its uncompressed size, as `xz -T` writes them, and
// the dictionary size code given (30 declares 128 MiB). The stream has
// no check. The xz writer the other helpers use never declares the
// sizes, so this is how a test gets a block that does.
func SizedStoredBlocks(t testing.TB, texts [][]byte, dictionaryCode byte) []byte {
	t.Helper()
	streamFlags := []byte{0x00, 0x00} // no check
	out := append([]byte{0xFD, '7', 'z', 'X', 'Z', 0x00}, streamFlags...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(streamFlags))

	index := []byte{0x00} // the index indicator
	index = binary.AppendUvarint(index, uint64(len(texts)))
	for _, text := range texts {
		data := storedLZMA2(text)
		header := sizedBlockHeader(uint64(len(data)), uint64(len(text)), dictionaryCode)
		out = append(out, header...)
		out = append(out, data...)
		out = appendZeroPadding(out, len(header)+len(data))
		index = binary.AppendUvarint(index, uint64(len(header)+len(data)))
		index = binary.AppendUvarint(index, uint64(len(text)))
	}
	index = appendZeroPadding(index, len(index))
	index = binary.LittleEndian.AppendUint32(index, crc32.ChecksumIEEE(index))
	out = append(out, index...)

	footer := binary.LittleEndian.AppendUint32(nil, uint32(len(index)/4-1)) //nolint:gosec // a test index is small
	footer = append(footer, streamFlags...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(footer))
	out = append(out, footer...)
	return append(out, 'Y', 'Z')
}

// storedLZMA2 is text as LZMA2 data of uncompressed chunks, the first
// of which resets the dictionary, followed by the end marker.
func storedLZMA2(text []byte) []byte {
	var data []byte
	control := byte(lzma2StoredResetDictionary)
	for len(text) > 0 {
		n := min(len(text), lzma2MaxStoredChunk)
		data = append(data, control)
		data = binary.BigEndian.AppendUint16(data, uint16(n-1)) //nolint:gosec // n <= 64 KiB
		data = append(data, text[:n]...)
		text = text[n:]
		control = lzma2Stored
	}
	return append(data, lzma2End)
}

// sizedBlockHeader is a block header that declares both sizes, one
// LZMA2 filter with the dictionary size code given, and its CRC32.
func sizedBlockHeader(compressedSize, uncompressedSize uint64, dictionaryCode byte) []byte {
	header := []byte{0, compressedSizePresent | uncompressedSizePresent}
	header = binary.AppendUvarint(header, compressedSize)
	header = binary.AppendUvarint(header, uncompressedSize)
	header = append(header, lzma2FilterID, lzma2PropsSize, dictionaryCode)
	header = appendZeroPadding(header, len(header))
	header[0] = byte((len(header)+4)/4 - 1) //nolint:gosec // a header of a few bytes
	return binary.LittleEndian.AppendUint32(header, crc32.ChecksumIEEE(header))
}

// appendZeroPadding appends the zero bytes that bring a length of n
// to a multiple of four.
func appendZeroPadding(b []byte, n int) []byte {
	return append(b, make([]byte, (4-n%4)%4)...)
}

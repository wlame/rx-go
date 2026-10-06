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

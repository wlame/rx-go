package compression

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"

	"github.com/ulikunitz/xz/lzma"
)

// ErrDictionaryTooLarge reports an xz block whose header declares a
// dictionary above the limit the reader was given. It wraps
// ErrTooLargeToDecode.
var ErrDictionaryTooLarge = fmt.Errorf("%w: an xz block declares a dictionary above the limit", ErrTooLargeToDecode)

// The parts of the xz container format (https://tukaani.org/xz/xz-file-format.txt)
// the reader checks, with the values it requires.
var (
	xzHeaderMagic = []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}
	xzFooterMagic = []byte{'Y', 'Z'}
)

const (
	xzStreamHeaderLen = 12 // magic, two flag bytes, CRC32
	xzStreamFooterLen = 12 // CRC32, backward size, two flag bytes, magic
	xzLZMA2FilterID   = 0x21
	xzLZMA2PropsSize  = 1
	// xzMaxVarintLen is the longest variable-length integer the format
	// allows: nine bytes of seven bits, and a tenth holding the top bit.
	xzMaxVarintLen = 10

	// Block header flags: the filter count less one, the reserved bits,
	// and the two optional size fields.
	xzFilterCountMask         = 0x03
	xzReservedBlockFlags      = 0x3C
	xzCompressedSizePresent   = 0x40
	xzUncompressedSizePresent = 0x80
)

// xzChecks are the integrity checks an xz stream may name, by the check
// id its flags hold: each block's text is hashed with it and the result
// compared with the bytes after the block. An id not listed is refused,
// as the xz package refuses it.
var xzChecks = map[byte]func() hash.Hash{
	0x00: nil, // none
	0x01: func() hash.Hash { return crc32.NewIEEE() },
	0x04: func() hash.Hash { return crc64.New(crc64.MakeTable(crc64.ECMA)) },
	0x0A: sha256.New,
}

// NewXzReader returns a reader of the text of the xz file src, which
// refuses a block whose header declares a dictionary above
// dictionaryLimit bytes, with an error wrapping ErrDictionaryTooLarge.
//
// SECURITY: an LZMA2 decoder reserves the whole dictionary a block
// header declares before it decodes the block, and a header can declare
// up to 4 GiB, so a 64-byte file can cost that much memory. The only
// code here that reserves a dictionary is the call this reader makes
// after it has read and checked the header that sizes it; the LZMA2
// decoder (lzma.Reader2) reads only a block's data, never a block
// header. Whatever bytes a block's data is followed by, this reader is
// the one that reads them as padding, a check and the next header, so
// no header reaches a decoder unchecked. Memory holds one block's
// dictionary at a time, at most dictionaryLimit, plus a block header of
// at most 1 KiB.
//
// It accepts what the xz package's reader accepts: one or more streams
// with zero padding between and after them, each with any check type
// that package knows, and blocks with or without their size fields. It
// checks every CRC32, each block's check and size fields, and each
// stream's index and footer against the blocks it read. The index is
// checked through a running digest of the blocks' sizes rather than a
// list of them, so memory does not grow with the number of blocks.
//
// The first stream's header is read here, so a file that is not xz is
// refused before any Read. The reader decodes on the caller's
// goroutine; it starts none and needs no Close.
func NewXzReader(src io.Reader, dictionaryLimit uint64) (io.Reader, error) {
	x := &xzReader{src: src, dictionaryLimit: dictionaryLimit}
	if err := x.startFirstStream(); err != nil {
		return nil, err
	}
	return x, nil
}

// xzReader is the reader NewXzReader returns. It moves through a file
// as: stream header, then blocks, then the index and footer that end
// the stream, then padding and the next stream or the end.
type xzReader struct {
	src             io.Reader
	dictionaryLimit uint64

	// The stream being read: its check id, the number of blocks read so
	// far, and a digest of their (unpadded size, text size) pairs, which
	// the stream's index must reproduce.
	inStream    bool
	checkID     byte
	blocks      uint64
	blockDigest hash.Hash

	block *xzBlock // the block being read, nil between blocks
	err   error    // the first error, returned by every later Read
}

// Read copies text into p. It returns io.EOF after the last stream.
//
// Go note: having this method makes an *xzReader an io.Reader.
func (x *xzReader) Read(p []byte) (int, error) {
	if x.err != nil {
		return 0, x.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Each pass returns text or finishes a block, so the loop runs once
	// per block the call crosses.
	for {
		if x.block == nil {
			if err := x.nextBlock(); err != nil {
				x.err = err
				return 0, err
			}
		}
		n, err := x.block.Read(p)
		if errors.Is(err, io.EOF) {
			err = x.finishBlock()
			if err == nil && n == 0 {
				continue
			}
		}
		if err != nil {
			x.err = err
			return n, err
		}
		return n, nil
	}
}

// nextBlock reads up to the next block header, checks it and opens the
// block: it reads the index and footer of a stream that ends on the way,
// and the padding and header of the stream that follows. It returns
// io.EOF after the last stream.
func (x *xzReader) nextBlock() error {
	for {
		if !x.inStream {
			if err := x.startNextStream(); err != nil {
				return err
			}
		}
		var first [1]byte
		if err := readFull(x.src, first[:]); err != nil {
			return err
		}
		// A zero where a block header would start is the index
		// indicator: the stream has no more blocks.
		if first[0] == 0 {
			if err := x.endStream(); err != nil {
				return err
			}
			continue
		}
		return x.openBlock(first[0])
	}
}

// startFirstStream reads the header of the file's first stream, which
// padding may not precede.
func (x *xzReader) startFirstStream() error {
	var header [xzStreamHeaderLen]byte
	if err := readFull(x.src, header[:]); err != nil {
		return err
	}
	return x.startStream(header)
}

// startNextStream skips the padding after a stream, four zero bytes at
// a time, and reads the next stream's header. It returns io.EOF when
// the file ends after the padding.
func (x *xzReader) startNextStream() error {
	var header [xzStreamHeaderLen]byte
	for {
		n, err := io.ReadFull(x.src, header[:4])
		if n == 0 && errors.Is(err, io.EOF) {
			return io.EOF
		}
		if err != nil {
			return unexpectedEOF(err)
		}
		if !allZero(header[:4]) {
			break
		}
	}
	if err := readFull(x.src, header[4:]); err != nil {
		return err
	}
	return x.startStream(header)
}

// startStream checks a stream header and starts the stream it opens.
func (x *xzReader) startStream(header [xzStreamHeaderLen]byte) error {
	if !bytes.Equal(header[:6], xzHeaderMagic) {
		return errors.New("xz: invalid stream header magic bytes")
	}
	if crc32.ChecksumIEEE(header[6:8]) != binary.LittleEndian.Uint32(header[8:]) {
		return errors.New("xz: stream header checksum error")
	}
	if header[6] != 0 {
		return errors.New("xz: invalid stream flags")
	}
	if _, known := xzChecks[header[7]]; !known {
		return errors.New("xz: unsupported check type")
	}
	x.inStream = true
	x.checkID = header[7]
	x.blocks = 0
	x.blockDigest = sha256.New()
	return nil
}

// openBlock reads the rest of the block header whose size byte is
// sizeByte, checks it, and opens the block's decoder.
func (x *xzReader) openBlock(sizeByte byte) error {
	headerLen := (uint64(sizeByte) + 1) * 4 // at most 1024 bytes
	header := make([]byte, headerLen)
	header[0] = sizeByte
	if err := readFull(x.src, header[1:]); err != nil {
		return err
	}
	fields, err := parseXzBlockHeader(header)
	if err != nil {
		return err
	}
	dictionary, err := lzma.DecodeDictCap(fields.dictionaryCode)
	if err != nil {
		return fmt.Errorf("xz: block header: %w", err)
	}
	// SECURITY: this check comes before the decoder below is created,
	// which is what reserves the dictionary.
	if dictionary < 0 || uint64(dictionary) > x.dictionaryLimit {
		return fmt.Errorf("%w: %d bytes, more than the %d allowed", ErrDictionaryTooLarge, dictionary, x.dictionaryLimit)
	}
	block := &xzBlock{
		headerLen:        headerLen,
		declaredCompSize: fields.compressedSize,
		declaredTextSize: fields.uncompressedSize,
		compressed:       countingReader{r: x.src},
		check:            newXzCheck(x.checkID),
	}
	dictionary = dictionaryForBlock(dictionary, fields.uncompressedSize)
	text, err := lzma.Reader2Config{DictCap: int(dictionary)}.NewReader2(&block.compressed)
	if err != nil {
		return fmt.Errorf("xz: block: %w", err)
	}
	block.text = text
	x.block = block
	return nil
}

// dictionaryForBlock is the dictionary to reserve for a block whose
// header names declared bytes, already checked against the limit, and
// may declare the size of the block's text.
//
// SECURITY: a header can name a dictionary far larger than the block it
// heads, and the decoder reserves the whole of it before reading the
// block. A file of thousands of tiny blocks that each name 128 MiB
// would reserve 128 MiB per block. Every xz block starts its LZMA2 data
// with a dictionary reset (the decoder refuses data that does not), so
// no match reaches further back than the block's own text, and a
// dictionary as long as that text decodes it exactly as the declared
// one would. When the header declares the text size, the dictionary is
// cut to it, but never below lzma.MinDictCap (4 KiB), the least the
// decoder accepts; a block whose text runs past its declared size is
// refused by xzBlock.Read. A header that leaves the size out gets the
// dictionary it names: nothing before the data says how long the text
// is.
func dictionaryForBlock(declared int64, textSize optionalSize) int64 {
	if !textSize.present {
		return declared
	}
	// textSize.value can be up to 2^63-1; comparing it as a uint64
	// before converting keeps the result within declared.
	if textSize.value >= uint64(declared) { //nolint:gosec // declared is between lzma.MinDictCap and the limit
		return declared
	}
	return max(int64(textSize.value), lzma.MinDictCap) //nolint:gosec // below declared, so it fits
}

// xzBlockFields are the fields of a block header the reader uses.
type xzBlockFields struct {
	compressedSize   optionalSize
	uncompressedSize optionalSize
	dictionaryCode   byte
}

// optionalSize is a size a block header may hold or leave out.
type optionalSize struct {
	value   uint64
	present bool
}

// exceededBy reports whether n is more than the size, when there is one.
func (s optionalSize) exceededBy(n uint64) bool { return s.present && n > s.value }

// notReachedBy reports whether n is less than the size, when there is one.
func (s optionalSize) notReachedBy(n uint64) bool { return s.present && n < s.value }

// parseXzBlockHeader checks a whole block header, its size byte first
// and its CRC32 last, and returns its fields. It accepts one filter,
// LZMA2, the only one the decoder supports, and any number of zero
// bytes of padding after it, as the xz package does.
func parseXzBlockHeader(header []byte) (xzBlockFields, error) {
	crcAt := len(header) - 4
	if crc32.ChecksumIEEE(header[:crcAt]) != binary.LittleEndian.Uint32(header[crcAt:]) {
		return xzBlockFields{}, errors.New("xz: checksum error for block header")
	}
	flags := header[1]
	if flags&xzReservedBlockFlags != 0 {
		return xzBlockFields{}, errors.New("xz: reserved block header flags set")
	}
	if flags&xzFilterCountMask != 0 {
		return xzBlockFields{}, errors.New("xz: unsupported filter count")
	}
	body := bytes.NewReader(header[2:crcAt])
	var fields xzBlockFields
	var err error
	if flags&xzCompressedSizePresent != 0 {
		if fields.compressedSize, err = readXzSize(body); err != nil {
			return xzBlockFields{}, err
		}
	}
	if flags&xzUncompressedSizePresent != 0 {
		if fields.uncompressedSize, err = readXzSize(body); err != nil {
			return xzBlockFields{}, err
		}
	}
	id, _, err := readXzVarint(body)
	if err != nil {
		return xzBlockFields{}, unexpectedEOF(err)
	}
	if id != xzLZMA2FilterID {
		return xzBlockFields{}, errors.New("xz: unsupported filter id")
	}
	var props [2]byte // the properties' size, then the dictionary size code
	if err := readFull(body, props[:]); err != nil {
		return xzBlockFields{}, err
	}
	if props[0] != xzLZMA2PropsSize {
		return xzBlockFields{}, errors.New("xz: wrong LZMA2 filter properties size")
	}
	fields.dictionaryCode = props[1]
	padding := header[crcAt-body.Len() : crcAt]
	if !allZero(padding) {
		return xzBlockFields{}, errors.New("xz: non-zero block header padding")
	}
	return fields, nil
}

// readXzSize reads a size field of a block header: a variable-length
// integer below 2^63.
func readXzSize(r io.Reader) (optionalSize, error) {
	v, _, err := readXzVarint(r)
	if err != nil {
		return optionalSize{}, unexpectedEOF(err)
	}
	if v >= 1<<63 {
		return optionalSize{}, errors.New("xz: size overflow in block header")
	}
	return optionalSize{value: v, present: true}, nil
}

// finishBlock reads what follows a block's data — the padding that
// aligns the block to four bytes, and its check — compares the check
// with the text's, and records the block's sizes for the index.
func (x *xzReader) finishBlock() error {
	block := x.block
	x.block = nil
	if err := block.checkSizesAtEnd(); err != nil {
		return err
	}
	compressed := block.compressed.n
	var trailer [3 + sha256.Size]byte // padding of at most 3 bytes, the longest check
	padding := trailer[:padLen(compressed)]
	if err := readFull(x.src, padding); err != nil {
		return err
	}
	if !allZero(padding) {
		return errors.New("xz: non-zero block padding")
	}
	var want []byte
	if block.check != nil {
		want = xzCheckSum(block.check)
	}
	stored := trailer[3 : 3+len(want)]
	if err := readFull(x.src, stored); err != nil {
		return err
	}
	if !bytes.Equal(stored, want) {
		return errors.New("xz: checksum error for block")
	}
	unpadded := block.headerLen + compressed + uint64(len(want))
	writeXzRecord(x.blockDigest, unpadded, block.textSize)
	x.blocks++
	return nil
}

// writeXzRecord adds one block's (unpadded size, text size) pair to the
// digest d. The blocks a stream held and the records of its index go
// through it alike, so the two digests are equal exactly when the
// pairs are, in order.
func writeXzRecord(d hash.Hash, unpadded, textSize uint64) {
	var pair [16]byte
	binary.LittleEndian.PutUint64(pair[:8], unpadded)
	binary.LittleEndian.PutUint64(pair[8:], textSize)
	_, _ = d.Write(pair[:]) // a hash's Write never fails
}

// endStream reads a stream's index, whose indicator byte has been read,
// and its footer, and checks both against the blocks the stream held.
func (x *xzReader) endStream() error {
	// The index's CRC32 covers its indicator byte and everything up to
	// the CRC32 itself.
	indexCRC := crc32.NewIEEE()
	_, _ = indexCRC.Write([]byte{0}) // a hash's Write never fails
	index := io.TeeReader(x.src, indexCRC)

	count, indexLen, err := readXzVarint(index)
	if err != nil {
		return unexpectedEOF(err)
	}
	if count != x.blocks {
		return fmt.Errorf("xz: index length is %d; want %d", count, x.blocks)
	}
	// The loop runs once per block the stream held, which the file's
	// bytes already bound.
	records := sha256.New()
	for range count {
		unpadded, n1, err := readXzVarint(index)
		if err != nil {
			return unexpectedEOF(err)
		}
		textSize, n2, err := readXzVarint(index)
		if err != nil {
			return unexpectedEOF(err)
		}
		indexLen += n1 + n2
		writeXzRecord(records, unpadded, textSize)
	}
	if !bytes.Equal(records.Sum(nil), x.blockDigest.Sum(nil)) {
		return errors.New("xz: index records do not match the blocks")
	}
	var tail [3 + 4]byte // padding of at most 3 bytes, then the CRC32
	padding := tail[:padLen(indexLen+1)]
	if err := readFull(index, padding); err != nil {
		return err
	}
	if !allZero(padding) {
		return errors.New("xz: non-zero byte in index padding")
	}
	indexLen += uint64(len(padding))
	want := indexCRC.Sum32()
	stored := tail[3:]
	if err := readFull(x.src, stored); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(stored) != want {
		return errors.New("xz: wrong checksum for index")
	}
	indexSize := indexLen + 1 + 4 // the indicator and the CRC32 too
	if err := x.readFooter(indexSize); err != nil {
		return err
	}
	x.inStream = false
	return nil
}

// readFooter reads and checks a stream footer: its CRC32, the index size
// it records, which must be indexSize, and the stream flags, which must
// be the header's.
func (x *xzReader) readFooter(indexSize uint64) error {
	var footer [xzStreamFooterLen]byte
	if err := readFull(x.src, footer[:]); err != nil {
		return err
	}
	if !bytes.Equal(footer[10:], xzFooterMagic) {
		return errors.New("xz: footer magic invalid")
	}
	if crc32.ChecksumIEEE(footer[4:10]) != binary.LittleEndian.Uint32(footer[:4]) {
		return errors.New("xz: footer checksum error")
	}
	if (uint64(binary.LittleEndian.Uint32(footer[4:8]))+1)*4 != indexSize {
		return errors.New("xz: index size in footer wrong")
	}
	if footer[8] != 0 || footer[9] != x.checkID {
		return errors.New("xz: footer flags incorrect")
	}
	return nil
}

// xzBlock is one block being decoded.
type xzBlock struct {
	headerLen        uint64
	declaredCompSize optionalSize
	declaredTextSize optionalSize
	compressed       countingReader
	text             io.Reader // the LZMA2 decoder over compressed
	check            hash.Hash // nil for a stream without checks
	textSize         uint64
}

// Read copies the block's text into p, hashing it for the check and
// holding it to the sizes the header declares. It returns io.EOF at the
// end of the block's data.
func (b *xzBlock) Read(p []byte) (int, error) {
	n, err := b.text.Read(p)
	if b.check != nil {
		_, _ = b.check.Write(p[:n]) // a hash's Write never fails
	}
	b.textSize += uint64(n) //nolint:gosec // Read returns 0 <= n <= len(p)
	if b.declaredTextSize.exceededBy(b.textSize) {
		return n, errors.New("xz: wrong uncompressed size for block")
	}
	if b.declaredCompSize.exceededBy(b.compressed.n) {
		return n, errors.New("xz: wrong compressed size for block")
	}
	return n, err
}

// checkSizesAtEnd checks, at the end of the block's data, that it was
// as long as its header declares.
func (b *xzBlock) checkSizesAtEnd() error {
	if b.declaredTextSize.notReachedBy(b.textSize) || b.declaredCompSize.notReachedBy(b.compressed.n) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// newXzCheck returns a new hash for the check id, nil for none.
func newXzCheck(id byte) hash.Hash {
	if newHash := xzChecks[id]; newHash != nil {
		return newHash()
	}
	return nil
}

// xzCheckSum is the value of h as an xz file stores it: a CRC32 or a
// CRC64 in little-endian byte order, a SHA-256 digest as it is.
func xzCheckSum(h hash.Hash) []byte {
	switch sum := h.(type) {
	case hash.Hash64:
		return binary.LittleEndian.AppendUint64(nil, sum.Sum64())
	case hash.Hash32:
		return binary.LittleEndian.AppendUint32(nil, sum.Sum32())
	default:
		return h.Sum(nil)
	}
}

// readXzVarint reads one variable-length integer of the xz format, a
// byte at a time, and returns it with the number of bytes it took. It
// refuses an integer longer than ten bytes, or whose tenth byte holds
// more than the 64th bit, as the xz package does.
func readXzVarint(r io.Reader) (value, length uint64, err error) {
	var b [1]byte
	for i := range uint64(xzMaxVarintLen) {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, i, err
		}
		if b[0] < 0x80 {
			if i == xzMaxVarintLen-1 && b[0] > 1 {
				return 0, i + 1, errors.New("xz: variable-length integer overflows 64 bits")
			}
			return value | uint64(b[0])<<(7*i), i + 1, nil
		}
		value |= uint64(b[0]&0x7F) << (7 * i)
	}
	return 0, xzMaxVarintLen, errors.New("xz: variable-length integer overflows 64 bits")
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n uint64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += uint64(n) //nolint:gosec // Read returns 0 <= n <= len(p)
	return n, err
}

// readFull fills buf from r. The end of r before buf is full is
// io.ErrUnexpectedEOF: inside an xz file, nothing may end early.
func readFull(r io.Reader, buf []byte) error {
	_, err := io.ReadFull(r, buf)
	return unexpectedEOF(err)
}

// unexpectedEOF turns io.EOF into io.ErrUnexpectedEOF and returns any
// other error as it is.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// padLen is how many bytes of padding bring n to a multiple of four.
func padLen(n uint64) int {
	return int((4 - n%4) % 4)
}

// allZero reports whether every byte of p is zero.
func allZero(p []byte) bool {
	for _, c := range p {
		if c != 0 {
			return false
		}
	}
	return true
}

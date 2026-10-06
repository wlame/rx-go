package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"

	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// zeroReader gives zero bytes without end.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestDecodeLZ4_RefusesADeclaredSizeAbove128MiBBeforeMakingItsBuffer(t *testing.T) {
	for _, size := range []uint64{maxValue + 1, 1 << 40, ^uint64(0)} {
		stored := make([]byte, lz4SizePrefix+16)
		le.PutUint64(stored, size)
		var err error
		allocated := allocatedBytes(func() { _, err = decodeLZ4(stored, maxValue) })
		if !errors.Is(err, errValueTooLarge) {
			t.Errorf("a declared size of %d gives %v, want errValueTooLarge", size, err)
		}
		if allocated > 1<<20 {
			t.Errorf("a declared size of %d allocated %d bytes before the refusal", size, allocated)
		}
	}
}

func TestDecodeZstd_RefusesAStreamedFrameThatDecodesToMoreThan128MiB(t *testing.T) {
	// A frame written in a stream does not declare its size, so the
	// decoder finds out only as it decodes.
	var frame bytes.Buffer
	w, err := zstd.NewWriter(&frame, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for range 129 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeZstd(frame.Bytes(), 0, maxValue); !errors.Is(err, errValueTooLarge) {
		t.Errorf("a %d-byte frame of 129 MiB gives %v, want errValueTooLarge", frame.Len(), err)
	}
}

func TestReadValue_AcceptsExactly128MiBAndRefusesOneByteMore(t *testing.T) {
	// The value lives only inside the if, so it can be freed before the
	// second read.
	if value, err := readValue(io.LimitReader(zeroReader{}, maxValue), maxValue); err != nil || len(value) != maxValue {
		t.Errorf("a value of exactly 128 MiB gives %d bytes, %v", len(value), err)
	}
	if _, err := readValue(io.LimitReader(zeroReader{}, maxValue+1), maxValue); !errors.Is(err, errValueTooLarge) {
		t.Errorf("a value of 128 MiB and one byte gives %v, want errValueTooLarge", err)
	}
}

// lz4Stored returns text as systemd stores an lz4 value: its size as a
// little-endian uint64, then one block.
func lz4Stored(t *testing.T, text []byte) []byte {
	t.Helper()
	out := make([]byte, lz4SizePrefix+lz4.CompressBlockBound(len(text)))
	le.PutUint64(out, uint64(len(text)))
	n, err := lz4.CompressBlock(text, out[lz4SizePrefix:], nil)
	if err != nil || n == 0 {
		t.Fatalf("compress %d bytes: %d, %v", len(text), n, err)
	}
	return out[:lz4SizePrefix+n]
}

// lz4Compressor compresses src into dst as one lz4 block and returns the
// block's size, or 0 when src does not compress.
type lz4Compressor struct {
	name     string
	compress func(src, dst []byte) (int, error)
}

// lz4Compressors returns pierrec's lz4 compressors: its default one, its
// port of the reference implementation's fast one, which systemd calls,
// and its high-compression levels. Each keeps tables it clears on every
// call, so the returned compressors serve one goroutine.
func lz4Compressors() []lz4Compressor {
	reference := &lz4.CompressorCCompat{}
	compressors := []lz4Compressor{
		{"fast", func(src, dst []byte) (int, error) { return lz4.CompressBlock(src, dst, nil) }},
		{"reference fast", reference.CompressBlock},
	}
	levels := []lz4.CompressionLevel{lz4.Level1, lz4.Level2, lz4.Level3, lz4.Level4, lz4.Level5, lz4.Level6, lz4.Level7, lz4.Level8, lz4.Level9}
	for i, level := range levels {
		hc := &lz4.CompressorHC{Level: level}
		compressors = append(compressors, lz4Compressor{fmt.Sprintf("high compression %d", i+1), hc.CompressBlock})
	}
	return compressors
}

// lz4PrefixTexts are values whose first bytes the prefix decoder must
// give: short ones, ones of many sequences, and ones where a long match
// gives the rest of the name search's 65 bytes, from right after the
// name, from further on, or ending exactly at the 65th byte.
var lz4PrefixTexts = [][]byte{
	bytes.Repeat([]byte("MESSAGE=abc"), 200),
	randomWords(4<<10, 3),
	append([]byte("COREDUMP="), bytes.Repeat([]byte{0}, 10_000)...),
	append([]byte("MESSAGE="), bytes.Repeat([]byte("A"), 16<<10)...),
	append([]byte("MESSAGE=a match starts after these words: "), bytes.Repeat([]byte("B"), 4<<10)...),
	append(append([]byte("BLOB="), bytes.Repeat([]byte("ab"), 30)...), append([]byte("|"), randomWords(1<<10, 4)...)...),
	[]byte("SHORT=value of fewer than 65 bytes, but compressed anyway, aaaaaaaaaaaaaa"),
}

// FuzzDecodeLZ4Prefix checks the prefix decoder against pierrec's full
// decoder. The input, compressed by each of pierrec's compressors, gives
// a block whose prefix of each length is the start of its full decode,
// and so of the input. The input as a block, whatever its bytes, ends the
// decoder without a panic, and where the full decoder accepts it, the
// prefix is the start of the full decode.
func FuzzDecodeLZ4Prefix(f *testing.F) {
	for _, text := range lz4PrefixTexts {
		f.Add(text)
	}
	compressors := lz4Compressors()
	f.Fuzz(func(t *testing.T, text []byte) {
		for _, c := range compressors {
			checkLZ4PrefixOfCompressed(t, c, text)
		}
		checkLZ4PrefixOfAnyBlock(t, text)
	})
}

// checkLZ4PrefixOfCompressed compresses text with c and checks that every
// prefix the decoder gives of the block is the start of the full decode.
func checkLZ4PrefixOfCompressed(t *testing.T, c lz4Compressor, text []byte) {
	t.Helper()
	stored := make([]byte, lz4SizePrefix+lz4.CompressBlockBound(len(text)))
	le.PutUint64(stored, uint64(len(text)))
	n, err := c.compress(text, stored[lz4SizePrefix:])
	if err != nil {
		t.Fatalf("%s: compress %d bytes: %v", c.name, len(text), err)
	}
	if n == 0 {
		return // text lz4 does not compress, which systemd stores as it is
	}
	stored = stored[:lz4SizePrefix+n]
	full := make([]byte, len(text))
	if m, err := lz4.UncompressBlock(stored[lz4SizePrefix:], full); err != nil || m != len(text) || !bytes.Equal(full, text) {
		t.Fatalf("%s: the full decode of the block gives %d bytes, %v; want the %d-byte text", c.name, m, err, len(text))
	}
	for _, size := range []int{1, 9, namePrefixBytes, len(full)} {
		dst := make([]byte, size)
		got, _, err := decodeLZ4Prefix(stored, dst)
		if want := full[:min(size, len(full))]; err != nil || !bytes.Equal(dst[:got], want) {
			t.Errorf("%s: a %d-byte prefix gives %d bytes, %v; want the first %d of the full decode", c.name, size, got, err, len(want))
		}
	}
}

// checkLZ4PrefixOfAnyBlock decodes the prefix of block, any bytes, and
// checks it against pierrec's full decode where that accepts the block.
// pierrec takes an empty block for an empty text; systemd's decoder
// (LZ4_decompress_safe_partial) and decodeLZ4Prefix refuse it.
func checkLZ4PrefixOfAnyBlock(t *testing.T, block []byte) {
	t.Helper()
	stored := append(make([]byte, lz4SizePrefix), block...)
	dst := make([]byte, namePrefixBytes)
	n, _, err := decodeLZ4Prefix(stored, dst)
	full := make([]byte, 64<<10)
	m, fullErr := lz4.UncompressBlock(block, full)
	if len(block) == 0 || fullErr != nil {
		return
	}
	if want := full[:min(m, len(dst))]; err != nil || !bytes.Equal(dst[:n], want) {
		t.Errorf("a block the full decoder accepts gives %q, %v; want %q", dst[:n], err, want)
	}
}

func TestDecodeLZ4Prefix_NeedsOnlyTheStoredBytesItDecodes(t *testing.T) {
	texts := map[string][]byte{
		// A long first run of literals: of its bytes the decoder needs
		// only those it copies.
		"a long run of literals": randomWords(64<<10, 5),
		// A long match after the name, of 1 MiB: its offset comes before
		// its 4 KiB of length bytes, and the first of these already cover
		// the bytes still missing.
		"a long match": append([]byte("COREDUMP="), make([]byte, 1<<20)...),
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			stored := lz4Stored(t, text)
			const cut = lz4SizePrefix + 200
			if len(stored) <= cut {
				t.Fatalf("the text is stored in %d bytes, not more than %d", len(stored), cut)
			}
			dst := make([]byte, namePrefixBytes)
			n, atEnd, err := decodeLZ4Prefix(stored[:cut], dst)
			if err != nil || atEnd || !bytes.Equal(dst[:n], text[:namePrefixBytes]) {
				t.Errorf("from %d stored bytes: %q, at end %v, %v", cut, dst[:n], atEnd, err)
			}
		})
	}
}

func TestDecodeLZ4Prefix_GivesTheBlocksTextWhateverSizeTheValueDeclares(t *testing.T) {
	text := bytes.Repeat([]byte("MESSAGE=abc"), 200)
	stored := lz4Stored(t, text)
	for _, size := range []uint64{0, 3, 17, 1 << 40} {
		le.PutUint64(stored, size)
		dst := make([]byte, namePrefixBytes)
		n, _, err := decodeLZ4Prefix(stored, dst)
		if err != nil || !bytes.Equal(dst[:n], text[:namePrefixBytes]) {
			t.Errorf("a declared size of %d gives %q, %v; want %q", size, dst[:n], err, text[:namePrefixBytes])
		}
	}
}

func TestDecodeLZ4Prefix_RefusesBadBlocks(t *testing.T) {
	stored := lz4Stored(t, bytes.Repeat([]byte("MESSAGE=abc"), 200))
	block := stored[lz4SizePrefix:]
	// A match that reaches before the start: token 0x00 (no literal, a
	// match of 4 bytes), offset 1, with nothing decoded yet.
	before := append(append([]byte(nil), stored[:lz4SizePrefix]...), 0x00, 0x01, 0x00)
	rows := map[string]struct {
		stored []byte
		atEnd  bool
	}{
		"no block after the size":          {stored[:lz4SizePrefix], true},
		"a match before the start":         {before, false},
		"a block cut inside its first run": {append(append([]byte(nil), stored[:lz4SizePrefix]...), block[:2]...), true},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			n, atEnd, err := decodeLZ4Prefix(row.stored, make([]byte, namePrefixBytes))
			if err == nil || atEnd != row.atEnd {
				t.Errorf("gives %d bytes, at end %v, %v; want an error, at end %v", n, atEnd, err, row.atEnd)
			}
		})
	}
}

func TestReadValue_StopsAtTheLimitItIsGiven(t *testing.T) {
	const limit = 1000
	if value, err := readValue(io.LimitReader(zeroReader{}, limit), limit); err != nil || len(value) != limit {
		t.Errorf("a value of exactly the limit gives %d bytes, %v", len(value), err)
	}
	var err error
	allocated := allocatedBytes(func() { _, err = readValue(io.LimitReader(zeroReader{}, maxValue), limit) })
	if !errors.Is(err, errValueTooLarge) {
		t.Errorf("a value past the limit gives %v, want errValueTooLarge", err)
	}
	if allocated > 64<<10 {
		t.Errorf("a value past a limit of %d bytes allocated %d bytes", limit, allocated)
	}
}

// The decoder decodes every frame of a value and makes each frame's
// buffer from the content size that frame declares, so every frame's
// size counts. The walk reads only frame and block headers.
func TestZstdDeclaredSize_AddsTheContentSizeOfEveryFrame(t *testing.T) {
	text := zstdBlock(true, zstdRawBlock, []byte("MESSAGE=x"))
	// A frame whose header asks for a checksum, which takes four bytes
	// after its last block.
	withChecksum := zstdFrameDeclaring(9, true, text)
	withChecksum[4] |= 0x04
	withChecksum = append(withChecksum, 1, 2, 3, 4)
	skippable := []byte{0x50, 0x2A, 0x4D, 0x18, 3, 0, 0, 0, 'a', 'b', 'c'}
	rows := []struct {
		name   string
		stored []byte
		want   uint64
	}{
		{"one frame", zstdFrameDeclaring(9, true, text), 9},
		{"a frame that declares no size", zstdFrame(text), 0},
		{"a second frame that declares 120 MiB", slices.Concat(zstdFrameDeclaring(9, true, text), zstdFrameDeclaring(120<<20, false, text)), 9 + 120<<20},
		{"a skippable frame first", slices.Concat(skippable, zstdFrameDeclaring(50, false, text)), 50},
		{"a frame with a checksum first", slices.Concat(withChecksum, zstdFrameDeclaring(7, false, text)), 9 + 7},
		{"a frame of an RLE block first", slices.Concat(zstdFrameDeclaring(5, true, zstdRLE(true, 5, 'r')), zstdFrameDeclaring(7, false, text)), 5 + 7},
		{"a frame cut inside its block", zstdFrameDeclaring(100, true, text)[:20], 100},
		{"bytes after the frames that are no frame", slices.Concat(zstdFrameDeclaring(9, true, text), []byte{1, 2, 3}), 9},
	}
	for _, row := range rows {
		if got := zstdDeclaredSize(row.stored); got != row.want {
			t.Errorf("%s: %d, want %d", row.name, got, row.want)
		}
	}
}

// A zstd block that gives text takes at least 4 stored bytes and gives at
// most 128 KiB, and an lz4 block gives less than 255 times its size, so
// a declared size above that bound is refused before a buffer is made
// for it; a size at the bound is left to the decoder. No real value is
// at the bound: it is an upper bound with room to spare.
func TestCheckDeclaredSize_RefusesMoreTextThanTheStoredBytesCanHold(t *testing.T) {
	what := objectRef{objectData, 4096}
	zstdOf := func(size uint64) []byte {
		return zstdFrameDeclaring(size, false, zstdBlock(true, zstdRawBlock, []byte("MESSAGE=x")))
	}
	zstdMost := (uint64(len(zstdOf(0)))/4 + 1) * (128 << 10)
	lz4Of := func(size uint64) []byte { return lz4LiteralsValue(size, []byte("MESSAGE=x")) }
	lz4Most := 255 * uint64(len(lz4Of(0))-lz4SizePrefix)
	rows := []struct {
		name   string
		flags  uint8
		stored []byte
		room   int
		// reason is the damage reason; "" for no damage.
		reason string
		// budget is set when the value is refused for the room the
		// entry has left.
		budget bool
	}{
		{"zstd at the most its stored bytes can hold", objectCompressedZstd, zstdOf(zstdMost), maxValue, "", false},
		{"zstd one byte more", objectCompressedZstd, zstdOf(zstdMost + 1), maxValue, "stored bytes cannot hold (zstd)", false},
		{"zstd above 128 MiB", objectCompressedZstd, zstdOf(maxValue + 1), maxValue, "decompresses to more than 128 MiB (zstd)", false},
		{"zstd above the room the entry has left", objectCompressedZstd, zstdOf(1000), 999, "", true},
		{"lz4 at the most its stored bytes can hold", objectCompressedLZ4, lz4Of(lz4Most), maxValue, "", false},
		{"lz4 one byte more", objectCompressedLZ4, lz4Of(lz4Most + 1), maxValue, "stored bytes cannot hold (lz4)", false},
		{"lz4 above 128 MiB", objectCompressedLZ4, lz4Of(1 << 40), maxValue, "decompresses to more than 128 MiB (lz4)", false},
		{"lz4 above the room the entry has left", objectCompressedLZ4, lz4Of(1000), 999, "", true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := checkDeclaredSize(payloadDecoders[row.flags], row.stored, row.room, what)
			reason, isDamage := damageReason(err)
			switch {
			case row.budget:
				if !errors.Is(err, errPastEntryBudget) {
					t.Errorf("gives %v, want errPastEntryBudget", err)
				}
			case row.reason == "":
				if err != nil {
					t.Errorf("gives %v, want no refusal", err)
				}
			case !isDamage || !strings.Contains(reason, row.reason):
				t.Errorf("gives %v, want damage holding %q", err, row.reason)
			}
		})
	}
}

// The densest values zstd and lz4 write, 4 MiB of zero bytes after a
// name, pass the check of their declared size and decode.
func TestCheckDeclaredSize_PassesTheDensestValuesZstdAndLZ4Write(t *testing.T) {
	payload := append([]byte("MESSAGE="), make([]byte, 4<<20)...)
	what := objectRef{objectData, 4096}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	values := map[string]struct {
		flags  uint8
		stored []byte
	}{
		"zstd of RLE blocks":           {objectCompressedZstd, zstdZerosValue("MESSAGE", len(payload))},
		"zstd at its best compression": {objectCompressedZstd, encoder.EncodeAll(payload, nil)},
	}
	for _, c := range lz4Compressors() {
		block := make([]byte, lz4.CompressBlockBound(len(payload)))
		n, err := c.compress(payload, block)
		if err != nil || n == 0 {
			t.Fatalf("lz4 %s: %d, %v", c.name, n, err)
		}
		stored := binary.LittleEndian.AppendUint64(nil, uint64(len(payload)))
		values["lz4 "+c.name] = struct {
			flags  uint8
			stored []byte
		}{objectCompressedLZ4, append(stored, block[:n]...)}
	}
	for name, v := range values {
		decoder := payloadDecoders[v.flags]
		declared, err := checkDeclaredSize(decoder, v.stored, maxValue, what)
		if err != nil {
			t.Errorf("%s (%d stored bytes): %v", name, len(v.stored), err)
			continue
		}
		if value, err := decoder.decode(v.stored, declared, maxValue); err != nil || !bytes.Equal(value, payload) {
			t.Errorf("%s: %d bytes, %v", name, len(value), err)
		}
	}
}

// journald's xz values declare a 1 MiB dictionary. The whole decode of
// a shown value refuses a stream that declares more than 16 MiB before
// its dictionary is made, in any stream of the value, not only the
// first.
func TestDecodeXZ_RefusesAStreamThatDeclaresADictionaryAbove16MiB(t *testing.T) {
	payload := append([]byte("MESSAGE="), bytes.Repeat([]byte("x"), 100)...)
	plain := journalfile.Stored(t, journalfile.XZ, payload)
	second := journalfile.Stored(t, journalfile.XZ, []byte("more"))
	rows := []struct {
		name   string
		stored []byte
		want   []byte
	}{
		{"a dictionary of 16 MiB", xzWithDictionary(t, plain, 24), payload},
		{"a dictionary of 24 MiB", xzWithDictionary(t, plain, 25), nil},
		{"a second stream with a dictionary of 128 MiB", slices.Concat(plain, xzWithDictionary(t, second, 30)), nil},
	}
	for _, row := range rows {
		value, err := decodeXZ(row.stored, maxValue)
		switch {
		case row.want != nil && (err != nil || !bytes.Equal(value, row.want)):
			t.Errorf("%s: %q, %v; want %q", row.name, value, err, row.want)
		case row.want == nil && !errors.Is(err, errXZDictionaryTooLarge):
			t.Errorf("%s: %v, want errXZDictionaryTooLarge", row.name, err)
		}
	}
}

// A zstd value of 1,023 frames of 128 KiB, each declaring its size, in
// 17 KB of stored bytes. The decoder makes the buffer of a frame that
// declares its size before it decodes the frame; given no buffer, it
// makes one per frame, of the text decoded so far and that frame's
// size, and copies into it. The value's buffer is made once instead,
// from the sum the frames declare, so what the decode allocates is the
// value.
func TestDecodeStored_MakesTheBufferOfAZstdValueOfManyFramesOnce(t *testing.T) {
	const frames, frameText = 1023, 128 << 10
	var stored []byte
	for range frames {
		stored = append(stored, zstdFrameDeclaring(frameText, true, zstdRLE(true, frameText, 0))...)
	}
	what := objectRef{objectData, 4096}
	decoder := payloadDecoders[objectCompressedZstd]
	// A first decode makes the pooled decoder's own tables, which the
	// measured decode reuses.
	if _, err := decodeStored(decoder, zstdFrameDeclaring(5, true, zstdRLE(true, 5, 0)), maxValue, what); err != nil {
		t.Fatal(err)
	}
	var value []byte
	var err error
	allocated := allocatedBytes(func() { value, err = decodeStored(decoder, stored, maxValue, what) })
	if err != nil || len(value) != frames*frameText {
		t.Fatalf("decodes to %d bytes, %v; want %d", len(value), err, frames*frameText)
	}
	if limit := uint64(frames*frameText + 8<<20); allocated > limit {
		t.Errorf("the decode of %d stored bytes allocated %d MiB, want at most %d MiB", len(stored), allocated>>20, limit>>20)
	}
	t.Logf("%d stored bytes: %d MiB allocated", len(stored), allocated>>20)
}

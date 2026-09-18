package seekable_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// footerTestText is the text the footer tests compress: 3000 numbered
// lines, cut into frames of 16 KiB with no regard for line breaks, as
// another encoder may cut them.
func footerTestText() ([]byte, [][]byte) {
	var text bytes.Buffer
	for n := 1; n <= 3000; n++ {
		fmt.Fprintf(&text, "LINE %d of the footer test text, padded to a useful length\n", n)
	}
	return text.Bytes(), seekablefile.SplitEvery(text.Bytes(), 16<<10)
}

// footerLayouts are the seek-table footers rx reads, by name.
var footerLayouts = map[string]seekablefile.Footer{
	"spec layout":                seekablefile.SpecFooter,
	"spec layout with checksums": seekablefile.SpecFooterWithChecksums,
	"legacy rx layout":           seekablefile.LegacyRxFooter,
}

// rx reads a seek table in the footer layout of the zstd seekable
// format specification (frame count, descriptor, magic), with or
// without per-frame checksums (descriptor bit 7, 12-byte entries), and
// in the layout rx wrote before (magic, frame count, flags). Each gives
// the frames as the file holds them, and the text read through the
// table is the file's text.
func TestReadSeekTable_ReadsEveryFooterLayout(t *testing.T) {
	t.Parallel()
	text, pieces := footerTestText()
	for name, footer := range footerLayouts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := seekablefile.EncodeWithFooter(t, pieces, footer)
			r := bytes.NewReader(file)
			tbl, err := seekable.ReadSeekTable(r, r.Size())
			if err != nil {
				t.Fatalf("ReadSeekTable: %v", err)
			}
			if tbl.NumFrames != len(pieces) || len(tbl.Frames) != len(pieces) {
				t.Fatalf("NumFrames = %d (%d listed), want %d", tbl.NumFrames, len(tbl.Frames), len(pieces))
			}
			var offset int64
			for i, piece := range pieces {
				frame := tbl.Frames[i]
				if frame.DecompressedOffset != offset || frame.DecompressedSize != int64(len(piece)) {
					t.Fatalf("frame %d: text at %d, %d bytes; want at %d, %d bytes",
						i, frame.DecompressedOffset, frame.DecompressedSize, offset, len(piece))
				}
				offset += int64(len(piece))
			}
			if !seekable.IsSeekableFile("app.log.zst", r, r.Size()) {
				t.Error("IsSeekableFile = false")
			}
			reader := seekable.NewTextReader(r, tbl)
			defer func() { _ = reader.Close() }()
			got, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(got, text) {
				t.Errorf("text through the table: %d bytes, %v; want the %d bytes of the text", len(got), err, len(text))
			}
		})
	}
}

// entryField returns the position in file of field (0 compressed size,
// 4 decompressed size) of seek-table entry i, for a file whose table has
// frames entries of entrySize bytes. In every layout the entries end
// where the 9-byte footer starts.
func entryField(file []byte, frames, entrySize, i, field int) int {
	return len(file) - seekable.FooterSize - (frames-i)*entrySize + field
}

// addToU32 adds delta to the little-endian u32 at file[at:].
func addToU32(file []byte, at int, delta int32) {
	value := binary.LittleEndian.Uint32(file[at:])
	binary.LittleEndian.PutUint32(file[at:], uint32(int64(value)+int64(delta)))
}

// A file that ends with the footer magic, in either layout, whose table
// does not describe the file is not seekable: ReadSeekTable says
// ErrSeekTableMismatch, and rx reads the file as the plain zstd stream
// it still is. That covers a descriptor whose checksum flag disagrees
// with the entries' width, a descriptor with a reserved bit set (the
// specification forbids them), a frame count larger than the file can
// hold, and nine bytes where both layouts find their magic and neither
// table adds up.
func TestReadSeekTable_RefusesAFooterWhoseTableDoesNotDescribeTheFile(t *testing.T) {
	t.Parallel()
	_, pieces := footerTestText()
	frames := len(pieces)
	spec := seekablefile.EncodeWithFooter(t, pieces, seekablefile.SpecFooter)
	withChecksums := seekablefile.EncodeWithFooter(t, pieces, seekablefile.SpecFooterWithChecksums)
	legacy := seekablefile.EncodeWithFooter(t, pieces, seekablefile.LegacyRxFooter)
	descriptor := func(file []byte) *byte { return &file[len(file)-5] }

	cases := map[string]func() []byte{
		"spec: a compressed size one byte off": func() []byte {
			f := bytes.Clone(spec)
			addToU32(f, entryField(f, frames, seekable.EntrySize, 1, 0), 1)
			return f
		},
		"spec with checksums: a decompressed size off": func() []byte {
			f := bytes.Clone(withChecksums)
			addToU32(f, entryField(f, frames, 12, 2, 4), 1)
			return f
		},
		"spec: the checksum flag set on 8-byte entries": func() []byte {
			f := bytes.Clone(spec)
			*descriptor(f) |= 0x80
			return f
		},
		"spec: the checksum flag clear on 12-byte entries": func() []byte {
			f := bytes.Clone(withChecksums)
			*descriptor(f) &^= 0x80
			return f
		},
		"spec: a reserved descriptor bit set": func() []byte {
			f := bytes.Clone(spec)
			*descriptor(f) |= 0x04
			return f
		},
		"spec: more frames than the file holds": func() []byte {
			f := bytes.Clone(spec)
			binary.LittleEndian.PutUint32(f[len(f)-9:], 0xFFFFFFFF)
			return f
		},
		"legacy rx: a compressed size one byte off": func() []byte {
			f := bytes.Clone(legacy)
			addToU32(f, entryField(f, frames, seekable.EntrySize, 1, 0), 1)
			return f
		},
		"the magic in both layouts' places": func() []byte {
			f := bytes.Clone(spec)
			binary.LittleEndian.PutUint32(f[len(f)-9:], seekable.FooterMagic)
			return f
		},
	}
	for name, damaged := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := bytes.NewReader(damaged())
			_, err := seekable.ReadSeekTable(r, r.Size())
			if !errors.Is(err, seekable.ErrSeekTableMismatch) {
				t.Fatalf("ReadSeekTable err = %v, want ErrSeekTableMismatch", err)
			}
			if seekable.IsSeekableFile("app.log.zst", r, r.Size()) {
				t.Error("IsSeekableFile = true for a table that does not describe the file")
			}
		})
	}
}

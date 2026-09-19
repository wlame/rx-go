package seekable

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/counting"
)

// encodeSeekable returns text as rx writes a seekable file, in frames
// of about frameSize bytes.
func encodeSeekable(t *testing.T, text []byte, frameSize int) []byte {
	t.Helper()
	var out bytes.Buffer
	enc := NewEncoder(EncoderConfig{FrameSize: frameSize, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &out); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return out.Bytes()
}

// entryAt returns the position in file of seek-table entry i, in the
// layout rx writes: the specification's footer, frame count first, and
// 8-byte entries that end where the 9-byte footer starts.
func entryAt(file []byte, i int) int {
	frames := int(binary.LittleEndian.Uint32(file[len(file)-FooterSize:]))
	return len(file) - FooterSize - frames*EntrySize + i*EntrySize
}

// addToEntry adds delta to the u32 at offset field (0 compressed size,
// 4 decompressed size) of seek-table entry i.
func addToEntry(file []byte, i, field int, delta int32) {
	at := entryAt(file, i) + field
	value := binary.LittleEndian.Uint32(file[at:])
	binary.LittleEndian.PutUint32(file[at:], uint32(int64(value)+int64(delta)))
}

// A seek table is trusted only when it describes the file it ends: the
// frames it lists end where the table starts, each starts with a zstd
// frame header, and a header that records its content size agrees with
// the entry. Anything else is ErrSeekTableMismatch, and the file is not
// seekable for rx, which reads it as the plain zstd stream it still is.
func TestReadSeekTable_RefusesATableThatDoesNotDescribeTheFile(t *testing.T) {
	t.Parallel()
	text := buildTestPayload(3000)
	valid := encodeSeekable(t, text, 16<<10)
	if tbl, err := ReadSeekTable(bytes.NewReader(valid), int64(len(valid))); err != nil || tbl.NumFrames < 4 {
		t.Fatalf("fixture: ReadSeekTable = %v, %v; want at least 4 frames", tbl, err)
	}
	other := encodeSeekable(t, buildTestPayload(500), 16<<10)

	cases := map[string]func([]byte) []byte{
		"a compressed size one byte off": func(f []byte) []byte {
			addToEntry(f, 1, 0, 1)
			return f
		},
		"two compressed sizes off in opposite directions": func(f []byte) []byte {
			addToEntry(f, 1, 0, 5)
			addToEntry(f, 2, 0, -5)
			return f
		},
		"a decompressed size off": func(f []byte) []byte {
			addToEntry(f, 2, 4, 1)
			return f
		},
		"the table's frame length wrong": func(f []byte) []byte {
			at := entryAt(f, 0) - 4
			binary.LittleEndian.PutUint32(f[at:], binary.LittleEndian.Uint32(f[at:])+8)
			return f
		},
		"the table's frame magic wrong": func(f []byte) []byte {
			f[entryAt(f, 0)-SkippableHeaderSize] ^= 0x01
			return f
		},
		"two seekable files joined": func(f []byte) []byte {
			return append(bytes.Clone(other), f...)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := damage(bytes.Clone(valid))
			r := bytes.NewReader(file)
			_, err := ReadSeekTable(r, r.Size())
			if !errors.Is(err, ErrSeekTableMismatch) {
				t.Fatalf("ReadSeekTable err = %v, want ErrSeekTableMismatch", err)
			}
			if IsSeekableFile("app.log.zst", r, r.Size()) {
				t.Error("IsSeekableFile = true for a table that does not describe the file")
			}
		})
	}
}

// Checking a table reads the table and, per frame, at most one frame
// header: never the frames themselves.
func TestReadSeekTable_ReadsTheTableAndOneHeaderPerFrame(t *testing.T) {
	t.Parallel()
	file := encodeSeekable(t, buildTestPayload(3000), 16<<10)
	r := counting.NewReaderAt(bytes.NewReader(file))
	tbl, err := ReadSeekTable(r, int64(len(file)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}
	table := int64(SkippableHeaderSize + tbl.NumFrames*EntrySize + FooterSize)
	budget := table + int64(tbl.NumFrames*maxFrameHeaderSize)
	if got := r.Load(); got > budget {
		t.Errorf("read %d bytes, want at most %d (table %d + %d frame headers)", got, budget, table, tbl.NumFrames)
	}
}

// A file whose text is empty has a table of no frames, and that table
// describes it.
func TestReadSeekTable_AcceptsAnEmptyTable(t *testing.T) {
	t.Parallel()
	var file bytes.Buffer
	if err := WriteSeekTable(&file, nil); err != nil {
		t.Fatalf("WriteSeekTable: %v", err)
	}
	tbl, err := ReadSeekTable(bytes.NewReader(file.Bytes()), int64(file.Len()))
	if err != nil || tbl.NumFrames != 0 {
		t.Fatalf("ReadSeekTable = %+v, %v; want an empty table", tbl, err)
	}
}

// A TextReader gives a seekable file's text frame after frame, and stops
// at a damaged frame with ErrDamagedFrame naming it, after the text of
// the frames before it.
func TestTextReader_ReadsTheTextAndStopsAtADamagedFrame(t *testing.T) {
	t.Parallel()
	text := buildTestPayload(3000)
	file := encodeSeekable(t, text, 16<<10)
	tbl, err := ReadSeekTable(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}

	whole := NewTextReader(bytes.NewReader(file), tbl)
	got, err := io.ReadAll(whole)
	_ = whole.Close()
	if err != nil || !bytes.Equal(got, text) {
		t.Fatalf("ReadAll = %d bytes, %v; want the %d bytes of the text", len(got), err, len(text))
	}

	damaged := tbl.Frames[2]
	at := damaged.CompressedOffset + damaged.CompressedSize/2
	for i := at; i < at+5; i++ {
		file[i] ^= 0xFF
	}
	partial := NewTextReader(bytes.NewReader(file), tbl)
	defer func() { _ = partial.Close() }()
	got, err = io.ReadAll(partial)
	if !errors.Is(err, ErrDamagedFrame) || !strings.Contains(err.Error(), "frame 2") {
		t.Fatalf("err = %v, want ErrDamagedFrame naming frame 2", err)
	}
	if !bytes.Equal(got, text[:damaged.DecompressedOffset]) {
		t.Errorf("read %d bytes before the damage, want the %d of the frames before it", len(got), damaged.DecompressedOffset)
	}
}

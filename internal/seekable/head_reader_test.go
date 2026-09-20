package seekable

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
)

// headReaderOver returns a HeadReader over file, whose seek table is
// tbl, that allows a window of up to 1 MiB.
func headReaderOver(t *testing.T, file io.ReaderAt, tbl *SeekTable) *HeadReader {
	t.Helper()
	h, err := NewHeadReader(file, tbl, 1<<20)
	if err != nil {
		t.Fatalf("NewHeadReader: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// A HeadReader gives a seekable file's text frame after frame, as a
// TextReader does, and stops at a damaged frame with ErrDamagedFrame
// naming it, after the text of the frames before it.
func TestHeadReader_ReadsTheTextAndStopsAtADamagedFrame(t *testing.T) {
	t.Parallel()
	text := buildTestPayload(3000)
	file := encodeSeekable(t, text, 16<<10)
	tbl, err := ReadSeekTable(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}
	got, err := io.ReadAll(headReaderOver(t, bytes.NewReader(file), tbl))
	if err != nil || !bytes.Equal(got, text) {
		t.Fatalf("ReadAll = %d bytes, %v; want the %d bytes of the text", len(got), err, len(text))
	}

	damaged := tbl.Frames[2]
	at := damaged.CompressedOffset + damaged.CompressedSize/2
	for i := at; i < at+5; i++ {
		file[i] ^= 0xFF
	}
	got, err = io.ReadAll(headReaderOver(t, bytes.NewReader(file), tbl))
	if !errors.Is(err, ErrDamagedFrame) || !strings.Contains(err.Error(), "frame 2") {
		t.Fatalf("err = %v, want ErrDamagedFrame naming frame 2", err)
	}
	if !bytes.Equal(got, text[:damaged.DecompressedOffset]) {
		t.Errorf("read %d bytes before the damage, want the %d of the frames before it", len(got), damaged.DecompressedOffset)
	}
}

// A frame read to its end must give the length the seek table says,
// as DecodeFrame requires: more or fewer bytes would shift every offset
// after it.
func TestHeadReader_AFrameOfAnotherLengthThanTheTableIsDamaged(t *testing.T) {
	t.Parallel()
	text := buildTestPayload(3000)
	file := encodeSeekable(t, text, 16<<10)
	for name, delta := range map[string]int64{"longer than the table says": -1, "shorter than the table says": 1} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tbl, err := ReadSeekTable(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatalf("ReadSeekTable: %v", err)
			}
			tbl.Frames[1].DecompressedSize += delta
			_, err = io.ReadAll(headReaderOver(t, bytes.NewReader(file), tbl))
			if !errors.Is(err, ErrDamagedFrame) || !strings.Contains(err.Error(), "frame 1") {
				t.Errorf("err = %v, want ErrDamagedFrame naming frame 1", err)
			}
		})
	}
}

// failingReaderAt fails every read that reaches from on.
type failingReaderAt struct {
	r    io.ReaderAt
	from int64
}

var errDisk = errors.New("disk read failed")

func (f failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.from {
		return 0, errDisk
	}
	return f.r.ReadAt(p, off)
}

// A frame whose bytes cannot be read is the read's error, not a damaged
// frame: the file may be fine.
func TestHeadReader_AFailedReadIsNotADamagedFrame(t *testing.T) {
	t.Parallel()
	text := buildTestPayload(3000)
	file := encodeSeekable(t, text, 16<<10)
	tbl, err := ReadSeekTable(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}
	failing := failingReaderAt{r: bytes.NewReader(file), from: tbl.Frames[1].CompressedOffset}
	got, err := io.ReadAll(headReaderOver(t, failing, tbl))
	if !errors.Is(err, errDisk) || errors.Is(err, ErrDamagedFrame) || !strings.Contains(err.Error(), "frame 1") {
		t.Fatalf("err = %v, want the read's error naming frame 1", err)
	}
	if !bytes.Equal(got, text[:tbl.Frames[1].DecompressedOffset]) {
		t.Errorf("read %d bytes before the failure, want the %d of frame 0", len(got), tbl.Frames[1].DecompressedOffset)
	}
}

// A frame whose window is above the limit is refused, naming the frame,
// with compression.ErrWindowTooLarge.
func TestHeadReader_RefusesAFrameWhoseWindowIsAboveTheLimit(t *testing.T) {
	t.Parallel()
	text := buildTestPayload(3000)
	file := encodeSeekable(t, text, 64<<10)
	tbl, err := ReadSeekTable(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}
	h, err := NewHeadReader(bytes.NewReader(file), tbl, 16<<10)
	if err != nil {
		t.Fatalf("NewHeadReader: %v", err)
	}
	defer func() { _ = h.Close() }()
	_, err = io.ReadAll(h)
	if !errors.Is(err, compression.ErrWindowTooLarge) || !strings.Contains(err.Error(), "frame 0") {
		t.Errorf("err = %v, want compression.ErrWindowTooLarge naming frame 0", err)
	}
}

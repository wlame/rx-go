package filetext

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// sampleText is a few hundred lines of numbered text, enough for several
// reads of every decoder and for a seekable copy of several frames.
func sampleText() []byte {
	var buf bytes.Buffer
	for n := 1; n <= 3000; n++ {
		fmt.Fprintf(&buf, "LINE %d of the sample text, with some width to it\n", n)
	}
	return buf.Bytes()
}

// storedCopies are the formats the opener reads a stream of, each with
// the compressedcopy encoding that writes it. A format whose encoder the
// host lacks (bzip2) is left out by the test that meets it.
var storedCopies = []struct {
	format   compression.Format
	encoding string
}{
	{compression.FormatGzip, compressedcopy.Gzip},
	{compression.FormatBz2, compressedcopy.Bzip2},
	{compression.FormatXz, compressedcopy.Xz},
	{compression.FormatZstd, compressedcopy.Zstd},
	{compression.FormatSeekableZstd, compressedcopy.SeekableZstd},
}

// writeFile writes data to a new file in the test's directory and
// returns it opened for reading, with its size. The file is closed when
// the test ends, whatever the test did with it.
func writeFile(t *testing.T, data []byte) (*os.File, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stored")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, int64(len(data))
}

// streamDecoderText is what compression.NewReader gives for data stored
// in format: the bytes it reads before its first error, and that error
// (nil at a clean end).
func streamDecoderText(t *testing.T, data []byte, format compression.Format) ([]byte, error) {
	t.Helper()
	dec, err := compression.NewReader(io.NopCloser(bytes.NewReader(data)), format)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dec.Close() }()
	return io.ReadAll(dec)
}

// The text of a file in every format rx read through
// compression.NewReader is, through the opener, the bytes that reader
// gave: a seekable zstd file read linearly included. For an undamaged
// file that is the text itself.
func TestOpen_GivesTheBytesTheStreamDecoderGave(t *testing.T) {
	text := sampleText()
	for _, c := range storedCopies {
		t.Run(string(c.format), func(t *testing.T) {
			stored := compressedcopy.Encode(t, c.encoding, text)
			if stored == nil {
				t.Skipf("no encoder for %s on this host", c.format)
			}
			want, wantErr := streamDecoderText(t, stored, c.format)
			if wantErr != nil || !bytes.Equal(want, text) {
				t.Fatalf("the stream decoder gives %d bytes and %v; the test needs the whole text", len(want), wantErr)
			}
			f, size := writeFile(t, stored)
			got := readAll(t, f, size, c.format)
			if !bytes.Equal(got, want) {
				t.Fatalf("the opener gives %d bytes, the stream decoder %d", len(got), len(want))
			}
		})
	}
}

// A stream cut short gives, through the opener, the bytes the stream
// decoder gave before the cut, and an error as it did.
func TestOpen_AStreamCutShortGivesWhatTheStreamDecoderGave(t *testing.T) {
	text := sampleText()
	for _, c := range storedCopies {
		t.Run(string(c.format), func(t *testing.T) {
			stored := compressedcopy.Encode(t, c.encoding, text)
			if stored == nil {
				t.Skipf("no encoder for %s on this host", c.format)
			}
			cut := stored[:len(stored)*2/3]
			want, wantErr := streamDecoderText(t, cut, c.format)
			if wantErr == nil {
				t.Fatalf("the stream decoder reads the cut %s stream without an error", c.format)
			}
			f, size := writeFile(t, cut)
			opened, err := Open(f, size, c.format)
			if err != nil {
				t.Fatalf("Open: %v, want the error from Read as the stream decoder gave it", err)
			}
			defer func() { _ = opened.Close() }()
			got, err := io.ReadAll(opened)
			if err == nil {
				t.Fatalf("the opener reads the cut stream to its end without an error")
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the opener gives %d bytes before its error, the stream decoder %d", len(got), len(want))
			}
		})
	}
}

// A plain file's text is its first size bytes, so a file that grew after
// its size was taken is read as it was.
func TestOpen_APlainFileIsItsFirstSizeBytes(t *testing.T) {
	text := sampleText()
	f, _ := writeFile(t, text)
	const size = 1000
	if got := readAll(t, f, size, compression.FormatNone); !bytes.Equal(got, text[:size]) {
		t.Fatalf("the plain text is %d bytes, want the first %d of the file", len(got), size)
	}
}

// Close releases the decoder and leaves the file open: the caller still
// reads it and closes it once, without an error.
func TestOpen_CloseLeavesTheFileOpen(t *testing.T) {
	text := sampleText()
	for _, c := range append(storedCopies, struct {
		format   compression.Format
		encoding string
	}{compression.FormatNone, ""}) {
		t.Run(string(c.format)+"_or_plain", func(t *testing.T) {
			stored := text
			if c.encoding != "" {
				stored = compressedcopy.Encode(t, c.encoding, text)
			}
			if stored == nil {
				t.Skipf("no encoder for %s on this host", c.format)
			}
			path := filepath.Join(t.TempDir(), "stored")
			if err := os.WriteFile(path, stored, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path) //nolint:gosec // the test's own file
			if err != nil {
				t.Fatal(err)
			}
			opened, err := Open(f, int64(len(stored)), c.format)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := io.Copy(io.Discard, opened); err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := opened.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := opened.Close(); err != nil {
				t.Errorf("a second Close: %v", err)
			}
			if _, err := f.ReadAt(make([]byte, 1), 0); err != nil {
				t.Errorf("the file after the text's Close: %v, want it still open", err)
			}
			if err := f.Close(); err != nil {
				t.Errorf("the caller's Close of the file: %v", err)
			}
		})
	}
}

// A format the opener does not know is an error from Open, and no text.
func TestOpen_AnUnknownFormatIsAnError(t *testing.T) {
	f, size := writeFile(t, []byte("text\n"))
	if opened, err := Open(f, size, compression.Format("lzo")); err == nil {
		_ = opened.Close()
		t.Fatal("Open of an unknown format gave a text")
	}
}

// A file that is not a journal has no journal report.
func TestOpen_NoJournalReportForOtherFormats(t *testing.T) {
	f, size := writeFile(t, []byte("text\n"))
	opened, err := Open(f, size, compression.FormatNone)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if _, err := io.Copy(io.Discard, opened); err != nil {
		t.Fatal(err)
	}
	if _, ok := opened.JournalReport(); ok {
		t.Fatal("a plain file has a journal report")
	}
}

// Read after Close is an error, not more text.
func TestOpen_ReadAfterCloseIsAnError(t *testing.T) {
	f, size := writeFile(t, []byte("text\n"))
	opened, err := Open(f, size, compression.FormatNone)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := opened.Read(make([]byte, 8)); n != 0 || !errors.Is(err, ErrClosed) {
		t.Fatalf("Read after Close = %d, %v; want 0 and ErrClosed", n, err)
	}
}

// readAll opens the text of f (size bytes, in format) and reads it to
// its end; any error fails the test.
func readAll(t *testing.T, f *os.File, size int64, format compression.Format) []byte {
	t.Helper()
	opened, err := Open(f, size, format)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = opened.Close() }()
	got, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

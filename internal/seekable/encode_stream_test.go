package seekable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

// countingReader counts the bytes its caller has read so far.
type countingReader struct {
	inner io.Reader
	read  int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.read += int64(n)
	return n, err
}

// firstWriteProbe records how many bytes of the source had been read
// when the encoder wrote its first frame.
type firstWriteProbe struct {
	source         *countingReader
	readAtFirstOut int64
	wroteAnything  bool
	out            bytes.Buffer
}

func (w *firstWriteProbe) Write(p []byte) (int, error) {
	if !w.wroteAnything {
		w.wroteAnything = true
		w.readAtFirstOut = w.source.read
	}
	return w.out.Write(p)
}

func TestEncodeStream_OutputDoesNotDependOnHowTheSourceSplitsItsReads(t *testing.T) {
	t.Parallel()
	payload := buildTestPayload(3000)
	for _, workers := range []int{1, 3} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			t.Parallel()
			enc := NewEncoder(EncoderConfig{FrameSize: 2048, Workers: workers})

			var fromFile bytes.Buffer
			wantTable, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &fromFile)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}

			// A decompressor hands out a few bytes at a time; the frames
			// must still end where they end for a file read whole.
			var fromStream bytes.Buffer
			gotTable, err := enc.EncodeStream(context.Background(), iotest.OneByteReader(bytes.NewReader(payload)), &fromStream)
			if err != nil {
				t.Fatalf("EncodeStream: %v", err)
			}

			if !bytes.Equal(fromStream.Bytes(), fromFile.Bytes()) {
				t.Errorf("stream output differs from file output (%d vs %d bytes)", fromStream.Len(), fromFile.Len())
			}
			if gotTable.NumFrames != wantTable.NumFrames {
				t.Errorf("frames: got %d, want %d", gotTable.NumFrames, wantTable.NumFrames)
			}
		})
	}
}

func TestEncodeStream_ReadsOnlyTheFramesInFlightBeforeItWritesTheFirst(t *testing.T) {
	t.Parallel()
	const (
		frameSize = 1024
		workers   = 2
	)
	payload := buildTestPayload(20000) // about 1 MB
	source := &countingReader{inner: bytes.NewReader(payload)}
	probe := &firstWriteProbe{source: source}

	enc := NewEncoder(EncoderConfig{FrameSize: frameSize, Workers: workers})
	if _, err := enc.EncodeStream(context.Background(), source, probe); err != nil {
		t.Fatalf("EncodeStream: %v", err)
	}

	// One batch of frames plus the read-ahead buffer is all the encoder
	// may hold; reading the whole source first would hold a 100 GB log
	// in memory.
	const limit = 64 * 1024
	if probe.readAtFirstOut > limit {
		t.Errorf("read %d bytes before the first write, want at most %d", probe.readAtFirstOut, limit)
	}
	if source.read != int64(len(payload)) {
		t.Errorf("read %d bytes in total, want %d", source.read, len(payload))
	}
}

func TestEncodeStream_ReturnsTheSourceError(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"any read error": errors.New("corrupt input"),
		// A gzip or zstd decoder reports a truncated stream this way; it
		// is an error, not the end of the text.
		"truncated compressed stream": io.ErrUnexpectedEOF,
	}
	for name, broken := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := io.MultiReader(bytes.NewReader(buildTestPayload(500)), iotest.ErrReader(broken))
			enc := NewEncoder(EncoderConfig{FrameSize: 1024, Workers: 2})

			_, err := enc.EncodeStream(context.Background(), source, io.Discard)
			if !errors.Is(err, broken) {
				t.Fatalf("got %v, want the source error", err)
			}
		})
	}
}

func TestEncodeStream_EmptySourceWritesAnEmptySeekTable(t *testing.T) {
	t.Parallel()
	enc := NewEncoder(EncoderConfig{Workers: 2})
	var out bytes.Buffer
	tbl, err := enc.EncodeStream(context.Background(), bytes.NewReader(nil), &out)
	if err != nil {
		t.Fatalf("EncodeStream: %v", err)
	}
	if tbl.NumFrames != 0 {
		t.Errorf("frames: got %d, want 0", tbl.NumFrames)
	}
	parsed, err := ReadSeekTable(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("read seek table: %v", err)
	}
	if parsed.NumFrames != 0 {
		t.Errorf("parsed frames: got %d, want 0", parsed.NumFrames)
	}
}

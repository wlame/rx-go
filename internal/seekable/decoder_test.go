package seekable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/compression"
)

// TestDecoder_ThreeFrameRoundTrip is the regression test for the
// pooled (sync.Pool) per-frame decompression path. It encodes a
// deterministic payload into a seekable-zstd file of at least three
// frames (a small FrameSize and a large payload), decodes every frame
// on its own through DecompressFrameAt, and checks each frame against
// its slice of the payload and the concatenation against the whole.
//
// If decoder state leaked between frames (forgetting that DecodeAll is
// stateless, or reusing a streaming decoder incorrectly), a frame would
// come out different and the test would name it.
func TestDecoder_ThreeFrameRoundTrip(t *testing.T) {
	t.Parallel()

	payload := buildTestPayload(3000)
	f, parsed := encodeToFile(t, payload, EncoderConfig{
		FrameSize: 4 * 1024, // small enough to force multiple frames
		Level:     3,
		Workers:   1,
	})
	if parsed.NumFrames < 3 {
		t.Fatalf("expected at least 3 frames, got %d — adjust FrameSize or payload size", parsed.NumFrames)
	}

	dec := NewDecoder()
	var reconstructed bytes.Buffer
	for i, frame := range parsed.Frames {
		got, err := dec.DecompressFrameAt(f, i, parsed)
		if err != nil {
			t.Fatalf("DecompressFrameAt[%d]: %v", i, err)
		}
		start := frame.DecompressedOffset
		end := start + int64(len(got))
		if end > int64(len(payload)) {
			t.Fatalf("frame %d decoded bytes overrun payload: end=%d payloadLen=%d", i, end, len(payload))
		}
		if !bytes.Equal(got, payload[start:end]) {
			t.Errorf("frame %d: bytes differ at [%d, %d)", i, start, end)
		}
		reconstructed.Write(got)
	}
	if !bytes.Equal(reconstructed.Bytes(), payload) {
		t.Errorf("reassembly mismatch: got %d bytes, want %d", reconstructed.Len(), len(payload))
	}
}

// TestDecoder_ManyFramesConcurrent decodes every frame of one file from
// its own goroutine, five times over, through one shared *os.File. The
// pooled decoders are handed from goroutine to goroutine; if one ever
// kept streaming state between uses, -race or a byte mismatch would
// show it.
//
// Go note: os.File.ReadAt reads at an explicit position (pread), so the
// goroutines can share the file without moving each other's position.
// sync.WaitGroup waits until every goroutine of a round has finished;
// each goroutine writes only its own element of decoded, so the slice
// needs no lock.
func TestDecoder_ManyFramesConcurrent(t *testing.T) {
	t.Parallel()

	payload := buildTestPayload(8000)
	f, tbl := encodeToFile(t, payload, EncoderConfig{
		FrameSize: 2 * 1024,
		Workers:   2,
	})

	dec := NewDecoder()
	for attempt := 0; attempt < 5; attempt++ {
		decoded := make([][]byte, tbl.NumFrames)
		errs := make([]error, tbl.NumFrames)
		var wg sync.WaitGroup
		for i := range tbl.NumFrames {
			wg.Add(1)
			go func() {
				defer wg.Done()
				decoded[i], errs[i] = dec.DecompressFrameAt(f, i, tbl)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("attempt %d: DecompressFrameAt: %v", attempt, err)
		}
		if !bytes.Equal(bytes.Join(decoded, nil), payload) {
			t.Errorf("attempt %d: reassembly mismatch", attempt)
		}
	}
}

// encodeToFile encodes payload as a seekable-zstd file under t.TempDir()
// and returns the open file, closed when the test ends, with its seek
// table.
func encodeToFile(t *testing.T, payload []byte, cfg EncoderConfig) (*os.File, *SeekTable) {
	t.Helper()
	var out bytes.Buffer
	tbl, err := NewEncoder(cfg).Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "frames.zst")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, tbl
}

// DecodeFrame refuses a frame whose bytes do not decompress, and one
// that decompresses to another length than the seek table records,
// with ErrDamagedFrame naming the frame. The second kind would shift
// every offset after it.
func TestDecodeFrame_RefusesDamagedBytesAndAWrongLength(t *testing.T) {
	t.Parallel()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("create encoder: %v", err)
	}
	defer func() { _ = encoder.Close() }()
	text := bytes.Repeat([]byte("LINE 1 some text\n"), 200)
	compressed := encoder.EncodeAll(text, nil)
	damaged := bytes.Clone(compressed)
	for i := len(damaged) / 2; i < len(damaged)/2+5; i++ {
		damaged[i] ^= 0xFF
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("create decoder: %v", err)
	}
	defer decoder.Close()

	cases := map[string]struct {
		bytes []byte
		size  int64
		ok    bool
	}{
		"intact":              {compressed, int64(len(text)), true},
		"damaged bytes":       {damaged, int64(len(text)), false},
		"longer than listed":  {compressed, int64(len(text)) - 1, false},
		"shorter than listed": {compressed, int64(len(text)) + 1, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := DecodeFrame(decoder, tc.bytes, FrameInfo{Index: 7, DecompressedSize: tc.size})
			if tc.ok {
				if err != nil || !bytes.Equal(out, text) {
					t.Fatalf("DecodeFrame = %d bytes, %v; want the text", len(out), err)
				}
				return
			}
			if !errors.Is(err, ErrDamagedFrame) || !strings.Contains(err.Error(), "frame 7") {
				t.Fatalf("DecodeFrame err = %v, want ErrDamagedFrame naming frame 7", err)
			}
		})
	}
}

// A frame too large to hold whole is refused as too large, never as
// damaged: a frame whose header declares a window above
// compression.WindowLimit, and one whose seek-table entry gives it more
// text than that, which is refused before anything is decoded. A
// search goes around a damaged frame and keeps the rest; a refused one
// must stop the whole read.
func TestDecodeFrame_RefusesAFrameTooLargeToHoldAsTooLargeNotDamaged(t *testing.T) {
	t.Parallel()
	// The writer declares the window it is given only for an input
	// longer than its first block.
	text := bytes.Repeat([]byte("LINE 1 some text\n"), 25_000)
	var stream bytes.Buffer
	w, err := zstd.NewWriter(&stream, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(256<<20))
	if err != nil {
		t.Fatalf("create encoder: %v", err)
	}
	if _, err := w.Write(text); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	decoder := compression.AcquireDecoder()
	defer compression.ReleaseDecoder(decoder)

	cases := map[string]FrameInfo{
		"a 256 MiB window": {Index: 3, DecompressedSize: int64(len(text)), CompressedSize: int64(stream.Len())},
		"an entry of more text than the limit": {
			Index: 3, DecompressedSize: compression.WindowLimit + 1, CompressedSize: int64(stream.Len()),
		},
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeFrame(decoder, stream.Bytes(), frame)
			if !errors.Is(err, compression.ErrWindowTooLarge) || errors.Is(err, ErrDamagedFrame) {
				t.Errorf("err = %v; want compression.ErrWindowTooLarge and not ErrDamagedFrame", err)
			}
			if !strings.Contains(fmt.Sprint(err), "frame 3") {
				t.Errorf("err = %v; want it to name frame 3", err)
			}
		})
	}
}

// A seek table that describes its file but gives a frame more text than
// compression.WindowLimit is not used: ReadSeekTable returns
// ErrFrameTooLargeToHold, which is not ErrSeekTableMismatch, and the
// file is read as the zstd stream it also is. The same text in frames
// within the limit keeps its table.
func TestReadSeekTable_DoesNotUseATableWithAFrameAboveTheLimit(t *testing.T) {
	line := []byte("2025-12-10 07:00:00.000 INFO same line again\n")
	text := bytes.Repeat(line, (compression.WindowLimit+1<<20)/len(line))
	encode := func(frameSize int) []byte {
		var out bytes.Buffer
		enc := NewEncoder(EncoderConfig{FrameSize: frameSize, Workers: 1})
		if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &out); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return out.Bytes()
	}

	oneFrame := encode(len(text))
	_, err := ReadSeekTable(bytes.NewReader(oneFrame), int64(len(oneFrame)))
	if !errors.Is(err, ErrFrameTooLargeToHold) || errors.Is(err, ErrSeekTableMismatch) {
		t.Errorf("one %d MiB frame: err = %v; want ErrFrameTooLargeToHold only", len(text)>>20, err)
	}

	framesWithinTheLimit := encode(compression.WindowLimit / 2)
	tbl, err := ReadSeekTable(bytes.NewReader(framesWithinTheLimit), int64(len(framesWithinTheLimit)))
	if err != nil || tbl.NumFrames != 3 {
		t.Errorf("frames of 64 MiB: %v, %v; want a table of 3 frames", tbl, err)
	}
}

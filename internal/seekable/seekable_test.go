package seekable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildTestPayload produces deterministic multi-line text of about `lines`
// lines. Long enough to force multiple chunks for a FrameSize ~ small.
func buildTestPayload(lines int) []byte {
	var b bytes.Buffer
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "%06d This is line number %d with some trailing text\n", i, i)
	}
	return b.Bytes()
}

func TestWriteReadSeekTable_RoundTrip(t *testing.T) {
	t.Parallel()
	frames := []FrameInfo{
		{Index: 0, CompressedOffset: 0, CompressedSize: 100, DecompressedOffset: 0, DecompressedSize: 400},
		{Index: 1, CompressedOffset: 100, CompressedSize: 120, DecompressedOffset: 400, DecompressedSize: 500},
		{Index: 2, CompressedOffset: 220, CompressedSize: 80, DecompressedOffset: 900, DecompressedSize: 300},
	}
	var buf bytes.Buffer
	if err := WriteSeekTable(&buf, frames); err != nil {
		t.Fatalf("WriteSeekTable: %v", err)
	}
	// To call ReadSeekTable we need a full file. Prepend dummy "compressed"
	// bytes so the seek table's offsets are consistent with a plausible
	// layout: each frame's CompressedOffset = cumulative. The tail bytes
	// are what matters for ReadSeekTable (footer + entries + skippable header).
	rawSize := int64(220 + 80) // sum of CompressedSize
	payload := make([]byte, rawSize)
	full := append(payload, buf.Bytes()...)

	tbl, err := ReadSeekTable(bytes.NewReader(full), int64(len(full)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}
	if tbl.NumFrames != len(frames) {
		t.Fatalf("numFrames: got %d, want %d", tbl.NumFrames, len(frames))
	}
	for i, f := range frames {
		if tbl.Frames[i] != f {
			t.Errorf("frame %d: got %+v, want %+v", i, tbl.Frames[i], f)
		}
	}
}

func TestReadSeekTable_RejectsMissingMagic(t *testing.T) {
	t.Parallel()
	bogus := make([]byte, 100)
	// Last 9 bytes are all zero — FooterMagic check will fail.
	_, err := ReadSeekTable(bytes.NewReader(bogus), int64(len(bogus)))
	if err == nil {
		t.Error("expected ErrNotSeekable")
	}
}

func TestReadSeekTable_TooShort(t *testing.T) {
	t.Parallel()
	_, err := ReadSeekTable(bytes.NewReader([]byte{0, 1, 2}), 3)
	if err == nil {
		t.Error("expected error for tiny file")
	}
}

func TestWriteSeekTable_RejectsOversizedFrame(t *testing.T) {
	t.Parallel()
	big := []FrameInfo{
		{Index: 0, CompressedSize: 1 << 33, DecompressedSize: 100},
	}
	var buf bytes.Buffer
	if err := WriteSeekTable(&buf, big); err == nil {
		t.Error("expected error for frame > u32 max")
	}
}

func TestEncodeDecode_RoundTrip_Sequential(t *testing.T) {
	t.Parallel()
	payload := buildTestPayload(2000)
	enc := NewEncoder(EncoderConfig{
		FrameSize: 8 * 1024, // small to force many frames
		Level:     3,
		Workers:   1,
	})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if tbl.NumFrames < 2 {
		t.Errorf("expected multiple frames, got %d", tbl.NumFrames)
	}

	encoded := out.Bytes()

	// IsSeekable requires a file on disk.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.zst")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsSeekable(path) {
		t.Error("IsSeekable returned false for freshly-written file")
	}

	// Round-trip via decoder.
	dec := NewDecoder()
	parsed, err := ReadSeekTable(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}
	if parsed.NumFrames != tbl.NumFrames {
		t.Errorf("parsed frame count %d != encoded %d", parsed.NumFrames, tbl.NumFrames)
	}

	var reconstructed bytes.Buffer
	for i := 0; i < parsed.NumFrames; i++ {
		data, err := dec.DecompressFrame(path, i, parsed)
		if err != nil {
			t.Fatalf("DecompressFrame %d: %v", i, err)
		}
		reconstructed.Write(data)
	}
	if !bytes.Equal(reconstructed.Bytes(), payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", reconstructed.Len(), len(payload))
	}
}

func TestEncodeDecode_RoundTrip_Parallel(t *testing.T) {
	t.Parallel()
	payload := buildTestPayload(5000)
	enc := NewEncoder(EncoderConfig{
		FrameSize: 16 * 1024,
		Level:     3,
		Workers:   4,
	})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if tbl.NumFrames < 2 {
		t.Errorf("expected multiple frames, got %d", tbl.NumFrames)
	}

	encoded := out.Bytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.zst")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	// Decode all frames in parallel.
	dec := NewDecoder()
	parsed, err := ReadSeekTable(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	indices := make([]int, parsed.NumFrames)
	for i := range indices {
		indices[i] = i
	}
	got, err := dec.DecompressFrames(context.Background(), path, indices, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != parsed.NumFrames {
		t.Errorf("got %d frames, want %d", len(got), parsed.NumFrames)
	}

	// Reassemble by order and compare.
	var reconstructed bytes.Buffer
	for i := 0; i < parsed.NumFrames; i++ {
		reconstructed.Write(got[i])
	}
	if !bytes.Equal(reconstructed.Bytes(), payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", reconstructed.Len(), len(payload))
	}
}

func TestEncodeDecode_FramesAreNewlineAligned(t *testing.T) {
	t.Parallel()
	payload := buildTestPayload(500)
	enc := NewEncoder(EncoderConfig{
		FrameSize: 2048, // small enough to force multiple frames
		Workers:   1,
	})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if tbl.NumFrames < 2 {
		t.Skip("test requires multiple frames")
	}

	// Decode frames one by one; each one's decompressed bytes must end
	// with '\n' (except possibly the final frame — we check the
	// tentative invariant that every frame except last ends with \n).
	encoded := out.Bytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.zst")
	os.WriteFile(path, encoded, 0o644)
	dec := NewDecoder()
	for i := 0; i < tbl.NumFrames-1; i++ {
		data, err := dec.DecompressFrame(path, i, tbl)
		if err != nil {
			t.Fatalf("DecompressFrame %d: %v", i, err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("frame %d does not end with newline", i)
		}
	}
}

func TestDecompressRange(t *testing.T) {
	t.Parallel()
	payload := []byte(strings.Repeat("abcdefghij", 2000)) // 20000 bytes, no newlines
	// Force newline-aligned chunks: add a newline every 500 bytes.
	var aligned bytes.Buffer
	for i, b := range payload {
		aligned.WriteByte(b)
		if (i+1)%500 == 0 {
			aligned.WriteByte('\n')
		}
	}
	full := aligned.Bytes()
	enc := NewEncoder(EncoderConfig{FrameSize: 600})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(full), int64(len(full)), &out)
	if err != nil {
		t.Fatal(err)
	}

	encoded := out.Bytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.zst")
	os.WriteFile(path, encoded, 0o644)

	dec := NewDecoder()
	// Pick a slice in the middle.
	start := int64(5000)
	length := int64(200)
	got, err := dec.DecompressRange(context.Background(), path, tbl, start, length)
	if err != nil {
		t.Fatalf("DecompressRange: %v", err)
	}
	want := full[start : start+length]
	if !bytes.Equal(got, want) {
		t.Errorf("range mismatch at [%d, %d):\ngot %q\nwant %q", start, start+length, got, want)
	}
}

func TestEncoder_EmptyInput(t *testing.T) {
	t.Parallel()
	enc := NewEncoder(EncoderConfig{})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(nil), 0, &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if tbl.NumFrames != 0 {
		t.Errorf("expected 0 frames, got %d", tbl.NumFrames)
	}
	// Output must still be a valid seekable file (9-byte footer + 8-byte header).
	encoded := out.Bytes()
	if len(encoded) < FooterSize {
		t.Errorf("expected non-empty output, got %d bytes", len(encoded))
	}
}

func TestIsSeekable_NonZstExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "not-zst.log")
	os.WriteFile(path, []byte("plain text"), 0o644)
	if IsSeekable(path) {
		t.Error("IsSeekable should return false for non-.zst file")
	}
}

func TestIsSeekable_MissingFile(t *testing.T) {
	t.Parallel()
	if IsSeekable("/absolutely/does/not/exist.zst") {
		t.Error("IsSeekable should return false for missing file")
	}
}

func TestDecompressFrame_OutOfRange(t *testing.T) {
	t.Parallel()
	tbl := &SeekTable{NumFrames: 2, Frames: []FrameInfo{{}, {}}}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.zst")
	os.WriteFile(path, []byte{}, 0o644)
	dec := NewDecoder()
	if _, err := dec.DecompressFrame(path, 5, tbl); err == nil {
		t.Error("expected ErrFrameIndexOutOfRange")
	}
	if _, err := dec.DecompressFrame(path, -1, tbl); err == nil {
		t.Error("expected ErrFrameIndexOutOfRange for negative")
	}
}

// errInjectedInitFailure is the synthetic error returned by the test
// factory to simulate a zstd.NewWriter failure partway through worker
// init. See TestEncodeParallel_EncoderClosedOnInitError.
var errInjectedInitFailure = errors.New("injected: zstd init failed")

// closeSpyEncoder wraps a real workerEncoder and records how many times
// Close() was invoked. Used by the leak test to verify that all
// encoders built before an injected failure get their Close() called
// exactly once by encodeParallel's cleanup defer.
type closeSpyEncoder struct {
	inner      workerEncoder
	closeCount int
	mu         sync.Mutex
}

func (s *closeSpyEncoder) EncodeAll(src, dst []byte) []byte {
	return s.inner.EncodeAll(src, dst)
}

func (s *closeSpyEncoder) Close() error {
	s.mu.Lock()
	s.closeCount++
	s.mu.Unlock()
	return s.inner.Close()
}

// TestEncodeParallel_EncoderClosedOnInitError covers Stage 8 Reviewer 1
// High #1: when the zstd factory fails mid-loop during encodeParallel
// worker init, all encoders built BEFORE the failure must be Close()d.
//
// Before the fix, the cleanup defer was installed AFTER the
// construction loop, so a failure at iteration i leaked encoders[0..i-1]
// — each holding a compression window + internal worker goroutine.
//
// Detection strategy: wrap each real encoder in a closeSpyEncoder that
// counts Close() calls. Inject a factory that builds `failAt` real
// spies, then returns an error on the next call. After encodeParallel
// returns, assert each spy saw exactly one Close().
//
// This test MUST fail on HEAD before the fix (defer-after-loop skips
// Close on all spies) and pass after (defer-before-loop closes them
// all via the populated slice prefix).
func TestEncodeParallel_EncoderClosedOnInitError(t *testing.T) {
	origFactory := newZstdEncoderForWorker
	t.Cleanup(func() { newZstdEncoderForWorker = origFactory })

	const workers = 5
	const failAt = 3

	var mu sync.Mutex
	var spies []*closeSpyEncoder
	var callCount int

	newZstdEncoderForWorker = func(level int) (workerEncoder, error) {
		mu.Lock()
		defer mu.Unlock()
		n := callCount
		callCount++
		if n == failAt {
			return nil, errInjectedInitFailure
		}
		real, err := origFactory(level)
		if err != nil {
			return nil, err
		}
		spy := &closeSpyEncoder{inner: real}
		spies = append(spies, spy)
		return spy, nil
	}

	enc := NewEncoder(EncoderConfig{Workers: workers})
	payload := buildTestPayload(200)
	var out bytes.Buffer
	_, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err == nil {
		t.Fatal("expected injected init failure, got nil")
	}
	if !errors.Is(err, errInjectedInitFailure) {
		t.Fatalf("expected wrapped injected failure, got: %v", err)
	}

	// Verify factory call count.
	if callCount != failAt+1 {
		t.Errorf("expected %d factory calls, got %d", failAt+1, callCount)
	}
	if len(spies) != failAt {
		t.Fatalf("expected %d successful constructions, got %d", failAt, len(spies))
	}

	// Core assertion: every encoder that was SUCCESSFULLY built before
	// the factory error must have had its Close() called exactly once.
	// Before the fix: 0 closes (defer was after the failing loop). After
	// the fix: 1 close per spy.
	for i, spy := range spies {
		spy.mu.Lock()
		n := spy.closeCount
		spy.mu.Unlock()
		if n != 1 {
			t.Errorf("spy %d: Close() called %d times, want 1 — leak or double-close",
				i, n)
		}
	}
}

// Property-ish test: random payloads round-trip.
func TestEncodeDecode_RandomPayloads(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 10; i++ {
		size := rng.IntN(50000) + 1000
		// Build random payload with occasional newlines.
		payload := make([]byte, size)
		for j := range payload {
			if rng.IntN(80) == 0 {
				payload[j] = '\n'
			} else {
				payload[j] = byte('a' + rng.IntN(26))
			}
		}
		payload[size-1] = '\n' // ensure terminator

		enc := NewEncoder(EncoderConfig{
			FrameSize: 2048 + rng.IntN(4096),
			Workers:   1 + rng.IntN(3),
		})
		var out bytes.Buffer
		tbl, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
		if err != nil {
			t.Fatalf("run %d encode: %v", i, err)
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "rand.zst")
		os.WriteFile(path, out.Bytes(), 0o644)

		parsed, err := ReadSeekTable(bytes.NewReader(out.Bytes()), int64(out.Len()))
		if err != nil {
			t.Fatalf("run %d parse: %v", i, err)
		}
		if parsed.NumFrames != tbl.NumFrames {
			t.Errorf("run %d: parsed %d frames vs encoded %d", i, parsed.NumFrames, tbl.NumFrames)
		}
		// Decompress everything and compare.
		dec := NewDecoder()
		var got bytes.Buffer
		for f := 0; f < parsed.NumFrames; f++ {
			data, err := dec.DecompressFrame(path, f, parsed)
			if err != nil {
				t.Fatalf("run %d frame %d: %v", i, f, err)
			}
			got.Write(data)
		}
		if !bytes.Equal(got.Bytes(), payload) {
			t.Errorf("run %d: payload mismatch (len got=%d, want=%d)", i, got.Len(), len(payload))
		}
	}
}

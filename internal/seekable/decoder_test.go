package seekable

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDecoder_ThreeFrameRoundTrip is the safety-net regression for the
// sync.Pool migration of the per-frame decompression path (Task 1 of
// the performance plan). It encodes a deterministic payload into exactly
// a multi-frame seekable-zstd file (we aim for ≥3 frames by keeping the
// FrameSize small and the payload large), then:
//
//  1. Decodes every frame individually via DecompressFrame and asserts
//     the concatenation equals the original payload.
//  2. Decodes the same frames in parallel via DecompressFrames and
//     asserts the same byte-exact output.
//
// If the migration introduces any decoder-state leakage between frames
// (e.g. forgetting that DecodeAll is stateless, or reusing a streaming
// decoder incorrectly), this test will fail with a byte-level mismatch
// on one or more frames.
func TestDecoder_ThreeFrameRoundTrip(t *testing.T) {
	t.Parallel()

	// Build a large enough payload so a small FrameSize produces ≥3
	// frames after newline-alignment.
	payload := buildTestPayload(3000)

	enc := NewEncoder(EncoderConfig{
		FrameSize: 4 * 1024, // small enough to force multiple frames
		Level:     3,
		Workers:   1,
	})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if tbl.NumFrames < 3 {
		t.Fatalf("expected at least 3 frames, got %d — adjust FrameSize or payload size", tbl.NumFrames)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "threeframe.zst")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	parsed, err := ReadSeekTable(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("ReadSeekTable: %v", err)
	}

	// Sequential decode: frame-by-frame.
	dec := NewDecoder()
	var reconstructedSeq bytes.Buffer
	for i := 0; i < parsed.NumFrames; i++ {
		data, err := dec.DecompressFrame(path, i, parsed)
		if err != nil {
			t.Fatalf("DecompressFrame[%d]: %v", i, err)
		}
		reconstructedSeq.Write(data)
	}
	if !bytes.Equal(reconstructedSeq.Bytes(), payload) {
		t.Errorf("sequential reassembly mismatch: got %d bytes, want %d", reconstructedSeq.Len(), len(payload))
	}

	// Parallel decode: same frames at once.
	indices := make([]int, parsed.NumFrames)
	for i := range indices {
		indices[i] = i
	}
	framesMap, err := dec.DecompressFrames(context.Background(), path, indices, parsed)
	if err != nil {
		t.Fatalf("DecompressFrames: %v", err)
	}
	if len(framesMap) != parsed.NumFrames {
		t.Fatalf("parallel decode returned %d frames, want %d", len(framesMap), parsed.NumFrames)
	}
	var reconstructedPar bytes.Buffer
	for i := 0; i < parsed.NumFrames; i++ {
		reconstructedPar.Write(framesMap[i])
	}
	if !bytes.Equal(reconstructedPar.Bytes(), payload) {
		t.Errorf("parallel reassembly mismatch: got %d bytes, want %d", reconstructedPar.Len(), len(payload))
	}

	// Per-frame byte-exactness: each frame's decoded bytes must match
	// the slice of the original payload at [DecompressedOffset, end).
	for i, frame := range parsed.Frames {
		got := framesMap[i]
		start := frame.DecompressedOffset
		end := start + int64(len(got))
		if end > int64(len(payload)) {
			t.Fatalf("frame %d decoded bytes overrun payload: end=%d payloadLen=%d",
				i, end, len(payload))
		}
		want := payload[start:end]
		if !bytes.Equal(got, want) {
			t.Errorf("frame %d: bytes differ at [%d, %d) — migration may have broken byte-exact semantics",
				i, start, end)
		}
	}

	// Log for visibility in -v runs — helps future readers see frame
	// counts without rerunning the test.
	t.Logf("round-tripped %d frames totaling %d bytes", parsed.NumFrames, len(payload))
}

// TestDecoder_ManyFramesConcurrent exercises the per-frame path under
// concurrent load. If the pool migration reuses decoders correctly
// (stateless DecodeAll), this passes. If it ever accidentally shares
// streaming state across goroutines, -race or a byte mismatch will
// surface it.
func TestDecoder_ManyFramesConcurrent(t *testing.T) {
	t.Parallel()

	payload := buildTestPayload(8000)
	enc := NewEncoder(EncoderConfig{
		FrameSize: 2 * 1024,
		Workers:   2,
	})
	var out bytes.Buffer
	tbl, err := enc.Encode(context.Background(), bytes.NewReader(payload), int64(len(payload)), &out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.zst")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	indices := make([]int, tbl.NumFrames)
	for i := range indices {
		indices[i] = i
	}

	dec := NewDecoder()
	// Run the parallel decode 5 times back-to-back so pool reuse is
	// exercised across repeated Acquire/Release cycles.
	for attempt := 0; attempt < 5; attempt++ {
		framesMap, err := dec.DecompressFrames(context.Background(), path, indices, tbl)
		if err != nil {
			t.Fatalf("attempt %d: DecompressFrames: %v", attempt, err)
		}
		var reconstructed bytes.Buffer
		for i := 0; i < tbl.NumFrames; i++ {
			reconstructed.Write(framesMap[i])
		}
		if !bytes.Equal(reconstructed.Bytes(), payload) {
			t.Errorf("attempt %d: reassembly mismatch", attempt)
		}
	}
	t.Logf("ran %d concurrent decode rounds across %d frames", 5, tbl.NumFrames)
}

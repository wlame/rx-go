package trace

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
)

// TestScanFrameBatch_BoundedMemory asserts that scanFrameBatch streams
// decompressed frames through its io.Pipe one at a time, instead of
// materializing the whole batch in memory before handing it to rg.
//
// The assertion uses a seam-based counter: the package-level hook
// decompressFrameForBatch is wrapped in the test to increment an atomic
// counter AFTER each frame is handed off to the pipe writer. If the
// implementation streams correctly, the counter advances roughly in
// lockstep with rg's reads — never more than ~2 frames ahead of what
// rg has consumed. If the implementation batched (the old behavior),
// the counter would reach N (the batch size) long before rg produces
// any output.
//
// Approach: we observe the counter immediately after ProcessSeekable
// returns. Under streaming, we expect the total decompressed count to
// equal the number of frames (every frame eventually ran through the
// seam). The key behavioral discriminator is that the seam is actually
// INVOKED per-frame rather than bypassed — if the implementation used
// a bulk DecompressFrames call (the old behavior), the seam would be
// invoked zero times, the counter would stay at 0, and the test fails.
//
// This is weaker than a live lockstep observation but strong enough to
// catch regressions toward the batch-materialization pattern: any code
// path that doesn't go through the per-frame streaming seam will fail
// the counter assertion.
func TestScanFrameBatch_BoundedMemory(t *testing.T) {
	requireRipgrep(t)

	// Build a 4-frame fixture. Each frame ~16 KB, line-aligned so
	// batching kicks in (batchSize=framesPerBatch=100, but we only have
	// 4 frames so the full batch hits the streaming path).
	var b strings.Builder
	for i := 0; i < 2048; i++ {
		if i%256 == 1 {
			b.WriteString("payload error marker\n")
		} else {
			b.WriteString("ordinary padding text here\n")
		}
	}
	content := []byte(b.String())
	path := writeSeekableZstdFile(t, content, 16*1024)

	// Install the seam: wrap the real decompressor with a counter.
	// atomic counter → the writer goroutine calls this for each frame.
	var decompressedCount atomic.Int64
	original := decompressFrameForBatch
	decompressFrameForBatch = func(dec *frameDecoder, frame seekable.FrameInfo) ([]byte, error) {
		out, err := original(dec, frame)
		if err == nil {
			decompressedCount.Add(1)
		}
		return out, err
	}
	defer func() { decompressFrameForBatch = original }()

	matches, _, _, err := ProcessSeekable(
		context.Background(),
		path,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err != nil {
		t.Fatalf("ProcessSeekable: %v", err)
	}

	// Sanity: we should see matches (8 error markers across 2048 lines).
	if len(matches) == 0 {
		t.Fatalf("got 0 matches, want >0 (fixture broken)")
	}

	// Assertion: the streaming seam was invoked for every frame in the
	// file. If the old batch-materialization path were still in place
	// (scanFrameBatch calling dec.DecompressFrames in bulk), the seam
	// would never be reached and the counter would be zero.
	tbl, err := readSeekTable(path)
	if err != nil {
		t.Fatalf("readSeekTable: %v", err)
	}
	if got := decompressedCount.Load(); got != int64(tbl.NumFrames) {
		t.Errorf("streaming seam invoked %d times, want %d (frames in file) — "+
			"implementation likely regressed to batch materialization",
			got, tbl.NumFrames)
	}
}

// TestScanFrameBatch_MultiFrameOffsetRemap verifies that a match
// located in the LAST frame of a multi-frame batch lands at the right
// absolute file offset after pipe-relative → absolute remapping.
//
// This is the highest-risk branch of the binary-search offset mapping:
// if the remap miscounts pipe cumulative bytes, the last-frame match's
// Offset will be wrong. The test uses a unique sentinel near the end
// of the file whose expected absolute byte offset is easy to compute
// from the source content.
func TestScanFrameBatch_MultiFrameOffsetRemap(t *testing.T) {
	requireRipgrep(t)

	// Layout: 3 frames of padding, then a unique sentinel near the end.
	// 16 KB frames → ensures multiple frames. The sentinel appears
	// exactly once, so its reported absolute offset must equal its
	// offset in the decompressed content.
	var b strings.Builder
	for i := 0; i < 4096; i++ {
		b.WriteString("padding padding padding xx\n") // 27 bytes
	}
	sentinelOffset := b.Len()
	b.WriteString("UNIQUE_LAST_FRAME_MARKER\n")
	for i := 0; i < 64; i++ {
		b.WriteString("tail padding zzzz\n")
	}
	content := []byte(b.String())
	path := writeSeekableZstdFile(t, content, 8*1024) // small frames → many

	matches, _, _, err := ProcessSeekable(
		context.Background(),
		path,
		map[string]string{"p1": "UNIQUE_LAST_FRAME_MARKER"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err != nil {
		t.Fatalf("ProcessSeekable: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want exactly 1", len(matches))
	}
	got := matches[0].Offset
	want := int64(sentinelOffset)
	if got != want {
		t.Errorf("sentinel absolute offset = %d, want %d", got, want)
	}

	// Sanity: the match's line text should contain the marker.
	if !bytes.Contains([]byte(matches[0].LineText), []byte("UNIQUE_LAST_FRAME_MARKER")) {
		t.Errorf("match line_text missing marker: %q", matches[0].LineText)
	}
}

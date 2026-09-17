package seekableindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
)

// makeSeekable writes `lines` lines of text, compresses them into a
// seekable .zst with the given frame size, and returns both paths.
func makeSeekable(t *testing.T, lines int, frameSize int) (textPath, zstPath string) {
	t.Helper()
	dir := t.TempDir()
	textPath = filepath.Join(dir, "app.log")

	var body bytes.Buffer
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&body, "log line number %d with padding to make frames\n", n)
	}
	if err := os.WriteFile(textPath, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}

	zstPath = textPath + ".zst"
	src, err := os.Open(textPath)
	if err != nil {
		t.Fatalf("open text: %v", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.Create(zstPath)
	if err != nil {
		t.Fatalf("create zst: %v", err)
	}
	defer func() { _ = dst.Close() }()

	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: frameSize, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), src, int64(body.Len()), dst); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := dst.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return textPath, zstPath
}

// openForBuild opens path and returns the file and its size, the
// arguments Build reads a seekable file through.
func openForBuild(path string) (*os.File, int64, error) {
	f, err := os.Open(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// buildPath runs Build over the file at path.
func buildPath(path string) (*Result, error) {
	f, size, err := openForBuild(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Build(f, size)
}

// copyPath runs BuildAndCopyText over the file at path.
func copyPath(path string, text io.Writer) (*Result, error) {
	f, size, err := openForBuild(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return BuildAndCopyText(f, size, text)
}

// The frame table is what the index exists for: every line in the file
// belongs to exactly one frame, and the frames tile the line range with
// no gap and no overlap.
func TestBuild_FramesTileEveryLine(t *testing.T) {
	_, zstPath := makeSeekable(t, 50000, 64*1024)

	got, err := buildPath(zstPath)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got.LineCount != 50000 {
		t.Errorf("line_count: got %d, want 50000", got.LineCount)
	}
	if got.FrameCount != len(got.Frames) {
		t.Errorf("frame_count %d does not match %d frames", got.FrameCount, len(got.Frames))
	}
	if got.FrameCount < 2 {
		t.Fatalf("fixture produced %d frames; the test needs several", got.FrameCount)
	}

	wantFirst := int64(1)
	for _, frame := range got.Frames {
		if frame.FirstLine != wantFirst {
			t.Errorf("frame %d starts at line %d, want %d", frame.Index, frame.FirstLine, wantFirst)
		}
		if frame.LineCount != frame.LastLine-frame.FirstLine+1 {
			t.Errorf("frame %d: line_count %d does not match %d..%d",
				frame.Index, frame.LineCount, frame.FirstLine, frame.LastLine)
		}
		wantFirst = frame.LastLine + 1
	}
	if last := got.Frames[len(got.Frames)-1]; last.LastLine != got.LineCount {
		t.Errorf("last frame ends at line %d, want %d", last.LastLine, got.LineCount)
	}
}

// Every checkpoint names the frame that holds its line, which is the
// third element rx-python writes and what makes a lookup decompress one
// frame instead of walking the stream.
func TestBuild_CheckpointsNameTheirFrame(t *testing.T) {
	_, zstPath := makeSeekable(t, 50000, 64*1024)

	got, err := buildPath(zstPath)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.LineIndex) == 0 {
		t.Fatal("no checkpoints were recorded")
	}

	for _, entry := range got.LineIndex {
		if entry.FrameIndex == nil {
			t.Fatalf("checkpoint for line %d names no frame", entry.LineNumber)
		}
		frame := got.Frames[*entry.FrameIndex]
		if entry.LineNumber < frame.FirstLine || entry.LineNumber > frame.LastLine {
			t.Errorf("checkpoint line %d is outside frame %d (%d..%d)",
				entry.LineNumber, frame.Index, frame.FirstLine, frame.LastLine)
		}
		if entry.ByteOffset < frame.DecompressedOffset ||
			entry.ByteOffset >= frame.DecompressedOffset+frame.DecompressedSize {
			t.Errorf("checkpoint offset %d is outside frame %d's bytes", entry.ByteOffset, frame.Index)
		}
	}

	// One per frame is the floor; a frame holding more than the interval
	// gets more.
	if len(got.LineIndex) < got.FrameCount {
		t.Errorf("checkpoints: got %d for %d frames", len(got.LineIndex), got.FrameCount)
	}
}

// A frame holding more lines than the interval gets interior
// checkpoints, so a lookup does not scan a whole large frame.
func TestBuild_LargeFrameGetsInteriorCheckpoints(t *testing.T) {
	// One 4 MB frame holds far more than CheckpointLineInterval lines.
	_, zstPath := makeSeekable(t, 30000, 4*1024*1024)

	got, err := buildPath(zstPath)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.FrameCount != 1 {
		t.Skipf("fixture produced %d frames; this test needs one", got.FrameCount)
	}
	if len(got.LineIndex) < 3 {
		t.Errorf("a %d-line single frame produced %d checkpoints, want one per %d lines",
			got.LineCount, len(got.LineIndex), CheckpointLineInterval)
	}
	for i, entry := range got.LineIndex {
		wantLine := int64(1 + i*CheckpointLineInterval)
		if entry.LineNumber != wantLine {
			t.Errorf("checkpoint %d: line %d, want %d", i, entry.LineNumber, wantLine)
		}
	}
}

// A last line with no trailing newline is still a line.
func TestBuild_UnterminatedLastLineCounts(t *testing.T) {
	dir := t.TempDir()
	textPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(textPath, []byte("alpha\nbeta\ngamma"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	zstPath := textPath + ".zst"
	src, _ := os.Open(textPath)
	defer func() { _ = src.Close() }()
	dst, _ := os.Create(zstPath)
	defer func() { _ = dst.Close() }()
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 4096, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), src, 15, dst); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_ = dst.Sync()

	got, err := buildPath(zstPath)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.LineCount != 3 {
		t.Errorf("line_count: got %d, want 3", got.LineCount)
	}
}

// A file that is not seekable, or whose seek table is damaged, is an
// error. An index built from a wrong frame table is worse than none,
// because every later lookup trusts it.
func TestBuild_RefusesAFileWithoutAUsableSeekTable(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "plain.log")
	if err := os.WriteFile(plain, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, zstPath := makeSeekable(t, 100, 4096)
	truncated := filepath.Join(dir, "truncated.zst")
	raw, err := os.ReadFile(zstPath)
	if err != nil {
		t.Fatalf("read zst: %v", err)
	}
	if err := os.WriteFile(truncated, raw[:len(raw)-32], 0o600); err != nil {
		t.Fatalf("write truncated: %v", err)
	}

	for name, path := range map[string]string{
		"plain text":     plain,
		"truncated zstd": truncated,
		"missing":        filepath.Join(dir, "nope.zst"),
	} {
		if _, err := buildPath(path); err == nil {
			t.Errorf("%s: got nil error, want a refusal", name)
		}
	}
}

// The checkpoints serialize as the 3-element arrays rx-python reads.
func TestBuild_CheckpointsSerializeAsThreeElementArrays(t *testing.T) {
	_, zstPath := makeSeekable(t, 5000, 64*1024)

	got, err := buildPath(zstPath)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	encoded, err := json.Marshal(got.LineIndex)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(string(encoded), "[[1,0,0]") {
		t.Errorf("first checkpoint: got %s…, want [[1,0,0]…", string(encoded)[:24])
	}
}

// BuildAndCopyText hands the caller the file's whole text, frame after
// frame, during the one pass that builds the frame table.
func TestBuildAndCopyText_WritesTheDecompressedText(t *testing.T) {
	textPath, zstPath := makeSeekable(t, 3000, 4*1024)
	want, err := os.ReadFile(textPath) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("read text: %v", err)
	}

	var text bytes.Buffer
	got, err := copyPath(zstPath, &text)
	if err != nil {
		t.Fatalf("BuildAndCopyText: %v", err)
	}
	if !bytes.Equal(text.Bytes(), want) {
		t.Errorf("copied %d bytes that differ from the %d-byte text", text.Len(), len(want))
	}
	plain, err := buildPath(zstPath)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.LineCount != plain.LineCount || len(got.Frames) != len(plain.Frames) {
		t.Errorf("copying the text changed the index: %d lines in %d frames, want %d in %d",
			got.LineCount, len(got.Frames), plain.LineCount, len(plain.Frames))
	}
}

// failingWriter refuses every write, as a reader that stopped would.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// A text copy that cannot be written stops the build with that error:
// an index whose analysis was never fed must not come back as complete.
func TestBuildAndCopyText_StopsWhenTheCopyFails(t *testing.T) {
	_, zstPath := makeSeekable(t, 3000, 4*1024)
	refused := errors.New("reader went away")

	if _, err := copyPath(zstPath, failingWriter{err: refused}); !errors.Is(err, refused) {
		t.Errorf("BuildAndCopyText error = %v, want one wrapping %v", err, refused)
	}
}

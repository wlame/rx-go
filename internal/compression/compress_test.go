package compression

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateSeekableZstd_BasicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := "line one\nline two\nline three\nline four\nline five\n"

	outPath := filepath.Join(dir, "output.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	table, err := CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{FrameSize: 20, CompressionLevel: 1},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	// Verify seek table was returned.
	require.NotNil(t, table)
	assert.Greater(t, len(table.Frames), 0)

	// Verify total decompressed size matches.
	assert.Equal(t, int64(len(original)), table.TotalDecompressedSize())

	// Decompress the whole file and verify content matches.
	rc, format, err := NewReader(outPath)
	require.NoError(t, err)
	defer rc.Close()

	// Should be detected as seekable zstd.
	assert.Equal(t, FormatSeekableZstd, format)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

func TestCreateSeekableZstd_SingleFrame(t *testing.T) {
	dir := t.TempDir()
	original := "small content\n"

	outPath := filepath.Join(dir, "single.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	table, err := CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{FrameSize: 1024 * 1024}, // Much larger than content.
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	assert.Equal(t, 1, len(table.Frames))
	assert.Equal(t, int64(len(original)), table.TotalDecompressedSize())

	// Round-trip verification.
	rc, _, err := NewReader(outPath)
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

func TestCreateSeekableZstd_MultipleFrames(t *testing.T) {
	dir := t.TempDir()

	// Generate content large enough for multiple frames with a small frame size.
	var buf bytes.Buffer
	for i := 0; i < 100; i++ {
		buf.WriteString("this is a line of text for testing frame splitting\n")
	}
	original := buf.String()

	outPath := filepath.Join(dir, "multi.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	table, err := CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{FrameSize: 200, CompressionLevel: 1},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	// Should produce multiple frames.
	assert.Greater(t, len(table.Frames), 1, "should produce multiple frames with small frame size")

	// Every frame should be accounted for.
	assert.Equal(t, int64(len(original)), table.TotalDecompressedSize())

	// Read back the seek table from the file to verify it was written correctly.
	f, err := os.Open(outPath)
	require.NoError(t, err)
	defer f.Close()

	readTable, err := ReadSeekTable(f)
	require.NoError(t, err)

	assert.Equal(t, len(table.Frames), len(readTable.Frames))
	for i := range table.Frames {
		assert.Equal(t, table.Frames[i].CompressedSize, readTable.Frames[i].CompressedSize)
		assert.Equal(t, table.Frames[i].DecompressedSize, readTable.Frames[i].DecompressedSize)
		assert.Equal(t, table.Frames[i].CompressedOffset, readTable.Frames[i].CompressedOffset)
		assert.Equal(t, table.Frames[i].DecompressedOffset, readTable.Frames[i].DecompressedOffset)
	}

	// Full round-trip: decompress and verify.
	rc, _, err := NewReader(outPath)
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

func TestCreateSeekableZstd_FramesAreLineAligned(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	for i := 0; i < 50; i++ {
		buf.WriteString("a line of text\n")
	}
	original := buf.String()

	outPath := filepath.Join(dir, "aligned.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	_, err = CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{FrameSize: 50, CompressionLevel: 1},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	// Open and verify line alignment.
	f, err := os.Open(outPath)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)

	aligned, err := IsLineAligned(f, table)
	require.NoError(t, err)
	assert.True(t, aligned, "all frames should end on newline boundaries")

	// Also verify by decompressing each frame individually.
	for i, frame := range table.Frames {
		data, err := DecompressFrame(f, frame)
		require.NoError(t, err)
		if len(data) > 0 {
			assert.Equal(t, byte('\n'), data[len(data)-1],
				"frame %d should end with newline", i)
		}
	}
}

func TestCreateSeekableZstd_EmptyInput(t *testing.T) {
	dir := t.TempDir()

	outPath := filepath.Join(dir, "empty.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	table, err := CreateSeekableZstd(
		strings.NewReader(""),
		outFile,
		CompressOpts{},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	assert.Equal(t, 0, len(table.Frames))
}

func TestCreateSeekableZstd_DefaultOpts(t *testing.T) {
	dir := t.TempDir()

	original := "default opts test\n"
	outPath := filepath.Join(dir, "defaults.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	// Zero-value opts should use defaults.
	table, err := CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	assert.Equal(t, 1, len(table.Frames), "small content should be a single frame")

	// Verify decompresses correctly.
	rc, _, err := NewReader(outPath)
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

func TestCreateSeekableZstd_ContentWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()

	// Content that does NOT end with a newline — the last frame should still work.
	original := "line one\nline two\nno trailing newline"

	outPath := filepath.Join(dir, "notrail.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	table, err := CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{FrameSize: 20, CompressionLevel: 1},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	assert.Equal(t, int64(len(original)), table.TotalDecompressedSize())

	// Verify round-trip.
	rc, _, err := NewReader(outPath)
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

func TestCreateSeekableZstd_FrameDecompressPerFrame(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	for i := 0; i < 20; i++ {
		buf.WriteString("0123456789abcdef\n")
	}
	original := buf.String()

	outPath := filepath.Join(dir, "perframe.zst")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)

	table, err := CreateSeekableZstd(
		strings.NewReader(original),
		outFile,
		CompressOpts{FrameSize: 60, CompressionLevel: 1},
	)
	require.NoError(t, err)
	require.NoError(t, outFile.Close())

	// Decompress frame by frame and verify concatenation matches original.
	f, err := os.Open(outPath)
	require.NoError(t, err)
	defer f.Close()

	var reconstructed bytes.Buffer
	for _, frame := range table.Frames {
		data, err := DecompressFrame(f, frame)
		require.NoError(t, err)
		reconstructed.Write(data)
	}

	assert.Equal(t, original, reconstructed.String())
}

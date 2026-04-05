package compression

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createSeekableZstdTestFile creates a seekable zstd file using CreateSeekableZstd
// and returns its path. Uses a small frame size to produce multiple frames.
func createSeekableZstdTestFile(t *testing.T, dir string, content string, frameSize int) string {
	t.Helper()
	path := filepath.Join(dir, "seekable.zst")

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	opts := CompressOpts{
		FrameSize:        frameSize,
		CompressionLevel: 1,
	}
	_, err = CreateSeekableZstd(bytes.NewReader([]byte(content)), f, opts)
	require.NoError(t, err)

	return path
}

func TestReadSeekTable_BasicFile(t *testing.T) {
	dir := t.TempDir()
	content := "line one\nline two\nline three\nline four\nline five\n"
	// Use a tiny frame size to get multiple frames.
	path := createSeekableZstdTestFile(t, dir, content, 20)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)

	assert.Greater(t, len(table.Frames), 0, "should have at least one frame")
	assert.False(t, table.HasChecksums)

	// Verify offsets are monotonically increasing.
	for i := 1; i < len(table.Frames); i++ {
		assert.Greater(t, table.Frames[i].CompressedOffset, table.Frames[i-1].CompressedOffset,
			"compressed offsets must increase")
		assert.Greater(t, table.Frames[i].DecompressedOffset, table.Frames[i-1].DecompressedOffset,
			"decompressed offsets must increase")
	}

	// Total decompressed size should match the original content length.
	assert.Equal(t, int64(len(content)), table.TotalDecompressedSize())
}

func TestReadSeekTable_SingleFrame(t *testing.T) {
	dir := t.TempDir()
	content := "small\n"
	// Frame size larger than content — should produce exactly one frame.
	path := createSeekableZstdTestFile(t, dir, content, 1024*1024)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)

	assert.Equal(t, 1, len(table.Frames))
	assert.Equal(t, int64(0), table.Frames[0].CompressedOffset)
	assert.Equal(t, int64(0), table.Frames[0].DecompressedOffset)
	assert.Equal(t, uint32(len(content)), table.Frames[0].DecompressedSize)
}

func TestReadSeekTable_NotSeekableZstd(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "plain.zst", []byte("this is not a seekable zstd file"))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	_, err = ReadSeekTable(f)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a seekable zstd")
}

func TestDecompressFrame_AllFrames(t *testing.T) {
	dir := t.TempDir()
	content := "line one\nline two\nline three\nline four\nline five\n"
	path := createSeekableZstdTestFile(t, dir, content, 20)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)

	// Decompress all frames and concatenate — should match original content.
	var reconstructed bytes.Buffer
	for _, frame := range table.Frames {
		data, err := DecompressFrame(f, frame)
		require.NoError(t, err)
		reconstructed.Write(data)
	}

	assert.Equal(t, content, reconstructed.String())
}

func TestDecompressFrame_SingleFrame(t *testing.T) {
	dir := t.TempDir()
	content := "hello world\n"
	path := createSeekableZstdTestFile(t, dir, content, 1024*1024)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)
	require.Equal(t, 1, len(table.Frames))

	data, err := DecompressFrame(f, table.Frames[0])
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func TestIsLineAligned_AlignedFrames(t *testing.T) {
	dir := t.TempDir()
	// CreateSeekableZstd aligns frames to newline boundaries.
	content := "line one\nline two\nline three\nline four\nline five\n"
	path := createSeekableZstdTestFile(t, dir, content, 20)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)

	aligned, err := IsLineAligned(f, table)
	require.NoError(t, err)
	assert.True(t, aligned, "frames created by CreateSeekableZstd should be line-aligned")
}

func TestIsLineAligned_SingleFrame(t *testing.T) {
	dir := t.TempDir()
	content := "one line\n"
	path := createSeekableZstdTestFile(t, dir, content, 1024*1024)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table, err := ReadSeekTable(f)
	require.NoError(t, err)

	aligned, err := IsLineAligned(f, table)
	require.NoError(t, err)
	assert.True(t, aligned, "single-frame file is trivially line-aligned")
}

func TestIsLineAligned_EmptyTable(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "dummy.zst", []byte("x"))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	table := &SeekTable{Frames: nil}
	_, err = IsLineAligned(f, table)
	assert.Error(t, err, "empty seek table should return error")
}

func TestSeekTable_TotalDecompressedSize(t *testing.T) {
	table := &SeekTable{
		Frames: []FrameEntry{
			{DecompressedOffset: 0, DecompressedSize: 100},
			{DecompressedOffset: 100, DecompressedSize: 200},
			{DecompressedOffset: 300, DecompressedSize: 50},
		},
	}
	assert.Equal(t, int64(350), table.TotalDecompressedSize())
}

func TestSeekTable_TotalDecompressedSize_Empty(t *testing.T) {
	table := &SeekTable{}
	assert.Equal(t, int64(0), table.TotalDecompressedSize())
}

func TestDetect_SeekableZstdCreatedByUs(t *testing.T) {
	dir := t.TempDir()
	content := "hello seekable\nworld\n"
	path := createSeekableZstdTestFile(t, dir, content, 1024)

	format, err := Detect(path)
	require.NoError(t, err)
	assert.Equal(t, FormatSeekableZstd, format)
}

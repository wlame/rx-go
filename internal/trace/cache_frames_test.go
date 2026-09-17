package trace

import (
	"slices"
	"testing"
)

// frameOfOffset returns the index of the frame whose decompressed
// bytes hold offset, from the file's seek table.
func frameOfOffset(t *testing.T, path string, offset int64) int {
	t.Helper()
	tbl, err := readSeekTable(pinForTest(t, path))
	if err != nil {
		t.Fatalf("read seek table: %v", err)
	}
	for _, frame := range tbl.Frames {
		if offset >= frame.DecompressedOffset && offset < frame.DecompressedEnd() {
			return frame.Index
		}
	}
	t.Fatalf("offset %d is past the last frame", offset)
	return -1
}

// A seekable-zstd trace cache records which frames hold a match, and
// the frame of every match, as rx-python's cache does. A cache hit
// reports the frames the scan found.
func TestSeekableCacheRecordsTheFramesWithMatches(t *testing.T) {
	largeFileCacheEnv(t)
	path := writeSeekableLog(t)
	// Three lines far apart, so the matches sit in a few frames only.
	patterns := []string{`^line (1|20000|40000) `}

	fresh := traceOnce(t, path, patterns, Options{})
	if len(fresh.Matches) != 3 {
		t.Fatalf("got %d matches, want 3", len(fresh.Matches))
	}
	var want []int
	for _, m := range fresh.Matches {
		if frame := frameOfOffset(t, path, m.Offset); !slices.Contains(want, frame) {
			want = append(want, frame)
		}
	}
	slices.Sort(want)

	data, err := LoadCache(CachePath(path, patterns, nil))
	if err != nil {
		t.Fatalf("the scan wrote no cache: %v", err)
	}
	if !slices.Equal(data.FramesWithMatches, want) {
		t.Errorf("frames_with_matches = %v, want %v", data.FramesWithMatches, want)
	}
	for _, m := range data.Matches {
		wantFrame := frameOfOffset(t, path, m.Offset)
		if m.FrameIndex == nil || *m.FrameIndex != wantFrame {
			t.Errorf("match at %d: frame_index = %v, want %d", m.Offset, m.FrameIndex, wantFrame)
		}
	}

	info, err := GetCompressedCacheInfo(path, patterns, nil)
	if err != nil {
		t.Fatalf("cache lookup: %v", err)
	}
	if !slices.Equal(info.FramesWithMatches, want) {
		t.Errorf("cache hit reports frames %v, the scan %v", info.FramesWithMatches, want)
	}
}

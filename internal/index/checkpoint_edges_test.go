package index

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// lineStartsOf returns the byte offset at which each line of text
// starts, keyed by its 1-based line number. A last line without a final
// newline is a line; the position after a final newline is not.
func lineStartsOf(text []byte) map[int64]int64 {
	starts := map[int64]int64{}
	line, offset := int64(1), int64(0)
	for offset < int64(len(text)) {
		starts[line] = offset
		next := bytes.IndexByte(text[offset:], '\n')
		if next < 0 {
			break
		}
		offset += int64(next) + 1
		line++
	}
	return starts
}

// requireCheckpointsAtLineStarts fails unless every checkpoint of idx
// names a line that exists in text and the byte where that line starts.
func requireCheckpointsAtLineStarts(t *testing.T, idx *rxtypes.UnifiedFileIndex, text []byte) {
	t.Helper()
	starts := lineStartsOf(text)
	for i, entry := range idx.LineIndex {
		want, exists := starts[entry.LineNumber]
		if !exists {
			t.Errorf("checkpoint %d %+v names line %d; the text has %d lines",
				i, entry, entry.LineNumber, len(starts))
			continue
		}
		if entry.ByteOffset != want {
			t.Errorf("checkpoint %d %+v: line %d starts at byte %d",
				i, entry, entry.LineNumber, want)
		}
	}
}

// A checkpoint is the byte where a line starts, so every checkpoint
// names a line the file has. The walk adds one at the start of the line
// after the one that crosses a step boundary; when the crossing line is
// the last, there is no such line, and no checkpoint may name it.
func TestBuild_EveryCheckpointStartsALineOfTheFile(t *testing.T) {
	hundredByteLines := strings.Repeat(strings.Repeat("x", 99)+"\n", 50)
	cases := []struct {
		name string
		text string
		step int64
	}{
		{"final newline, last line ends on a step", hundredByteLines, 500},
		{"no final newline, last line ends on a step", strings.TrimSuffix(hundredByteLines, "\n"), 499},
		{"long last line crosses a step", "a\nb\nc\nLAST " + strings.Repeat("x", 200) + "\n", 100},
		{"long last line without a final newline", "a\nb\nc\nLAST " + strings.Repeat("x", 200), 100},
		{"single line longer than the step", strings.Repeat("y", 1000) + "\n", 100},
		{"file shorter than the step", "one\ntwo\n", 1 << 20},
		{"empty file", "", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := []byte(tc.text)
			dir := t.TempDir()
			plain := filepath.Join(dir, "app.log")
			writePlain(t, plain, text)
			compressed := filepath.Join(dir, "app.log.gz")
			writeGzip(t, compressed, text)

			for _, path := range []string{plain, compressed} {
				idx, err := Build(path, BuildOptions{StepBytes: tc.step})
				if err != nil {
					t.Fatalf("Build(%s): %v", filepath.Base(path), err)
				}
				requireCheckpointsAtLineStarts(t, idx, text)
			}
		})
	}
}

// An empty file has no lines, so its index has no checkpoints, and the
// stored index and every answer built from it carry an empty list
// rather than null.
func TestBuild_EmptyFileHasNoCheckpoints(t *testing.T) {
	p := writeTempFile(t, "")
	idx, err := Build(p, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(idx.LineIndex) != 0 {
		t.Errorf("LineIndex = %+v, want no checkpoints", idx.LineIndex)
	}
	encoded, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"line_index":[]`)) {
		t.Errorf("stored index does not hold an empty line_index list: %s", encoded)
	}
}

// Lookups on an index without checkpoints start at the first byte of
// the file, as they do without an index.
func TestLookups_OnAnIndexWithoutCheckpointsStartAtTheFirstByte(t *testing.T) {
	idx := &rxtypes.UnifiedFileIndex{LineIndex: []rxtypes.LineIndexEntry{}}
	zero := rxtypes.LineIndexEntry{}
	if got := FindNearestCheckpoint(idx, 1); got != zero {
		t.Errorf("FindNearestCheckpoint = %+v, want the zero entry", got)
	}
	if got := FindNearestCheckpointForOffset(idx, 0); got != zero {
		t.Errorf("FindNearestCheckpointForOffset = %+v, want the zero entry", got)
	}
	if got := CheckpointForContext(idx, 0, 3); got != zero {
		t.Errorf("CheckpointForContext = %+v, want the zero entry", got)
	}
}

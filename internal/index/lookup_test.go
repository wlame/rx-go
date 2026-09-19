package index

import (
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

func TestFindNearestCheckpoint(t *testing.T) {
	t.Parallel()
	idx := &rxtypes.UnifiedFileIndex{
		LineIndex: []rxtypes.LineIndexEntry{
			{LineNumber: 1, ByteOffset: 0},
			{LineNumber: 100, ByteOffset: 4096},
			{LineNumber: 500, ByteOffset: 20480},
			{LineNumber: 1000, ByteOffset: 40960},
		},
	}
	cases := []struct {
		target int64
		want   rxtypes.LineIndexEntry
	}{
		{0, rxtypes.LineIndexEntry{LineNumber: 0, ByteOffset: 0}}, // nothing <= 0
		{1, rxtypes.LineIndexEntry{LineNumber: 1, ByteOffset: 0}},
		{50, rxtypes.LineIndexEntry{LineNumber: 1, ByteOffset: 0}},
		{100, rxtypes.LineIndexEntry{LineNumber: 100, ByteOffset: 4096}},
		{101, rxtypes.LineIndexEntry{LineNumber: 100, ByteOffset: 4096}},
		{500, rxtypes.LineIndexEntry{LineNumber: 500, ByteOffset: 20480}},
		{999, rxtypes.LineIndexEntry{LineNumber: 500, ByteOffset: 20480}},
		{1000, rxtypes.LineIndexEntry{LineNumber: 1000, ByteOffset: 40960}},
		{2000, rxtypes.LineIndexEntry{LineNumber: 1000, ByteOffset: 40960}},
	}
	for _, tc := range cases {
		got := FindNearestCheckpoint(idx, tc.target)
		if got != tc.want {
			t.Errorf("FindNearestCheckpoint(%d) = %+v, want %+v", tc.target, got, tc.want)
		}
	}
}

func TestFindNearestCheckpoint_EmptyIndex(t *testing.T) {
	t.Parallel()
	empty := &rxtypes.UnifiedFileIndex{}
	got := FindNearestCheckpoint(empty, 100)
	if got != (rxtypes.LineIndexEntry{}) {
		t.Errorf("expected zero, got %+v", got)
	}
}

func TestFindNearestCheckpointForOffset(t *testing.T) {
	t.Parallel()
	idx := &rxtypes.UnifiedFileIndex{
		LineIndex: []rxtypes.LineIndexEntry{
			{LineNumber: 1, ByteOffset: 0},
			{LineNumber: 100, ByteOffset: 4096},
			{LineNumber: 500, ByteOffset: 20480},
		},
	}
	cases := []struct {
		target int64
		want   rxtypes.LineIndexEntry
	}{
		{0, rxtypes.LineIndexEntry{LineNumber: 1, ByteOffset: 0}},
		{1000, rxtypes.LineIndexEntry{LineNumber: 1, ByteOffset: 0}},
		{4096, rxtypes.LineIndexEntry{LineNumber: 100, ByteOffset: 4096}},
		{20480, rxtypes.LineIndexEntry{LineNumber: 500, ByteOffset: 20480}},
		{100000, rxtypes.LineIndexEntry{LineNumber: 500, ByteOffset: 20480}},
	}
	for _, tc := range cases {
		got := FindNearestCheckpointForOffset(idx, tc.target)
		if got != tc.want {
			t.Errorf("target=%d: got %+v, want %+v", tc.target, got, tc.want)
		}
	}
}

// A pass that must show `context` lines before an offset starts at a
// checkpoint at least that many lines before the checkpoint holding the
// offset, however many checkpoints back that is. The checkpoints here
// are 4 lines apart, the spacing of a log whose lines are about 1 KB
// long under a 4 KB index step.
func TestCheckpointForContext(t *testing.T) {
	t.Parallel()
	idx := &rxtypes.UnifiedFileIndex{
		LineIndex: []rxtypes.LineIndexEntry{
			{LineNumber: 1, ByteOffset: 0},
			{LineNumber: 5, ByteOffset: 4000},
			{LineNumber: 9, ByteOffset: 8000},
			{LineNumber: 13, ByteOffset: 12000},
			{LineNumber: 17, ByteOffset: 16000},
		},
	}
	start := rxtypes.LineIndexEntry{}
	cases := []struct {
		name    string
		offset  int64
		context int
		want    rxtypes.LineIndexEntry
	}{
		{"no context starts at the checkpoint holding the offset", 16500, 0, idx.LineIndex[4]},
		{"offset exactly at a checkpoint", 16000, 1, idx.LineIndex[3]},
		{"offset one byte after a checkpoint", 16001, 4, idx.LineIndex[3]},
		{"context one line more than a gap", 16000, 5, idx.LineIndex[2]},
		{"context spanning three gaps", 16000, 12, idx.LineIndex[1]},
		{"context spanning three gaps and a line", 16000, 13, idx.LineIndex[0]},
		{"context reaching before the first line", 16000, 100, start},
		{"offset before the second checkpoint", 3999, 2, start},
	}
	for _, tc := range cases {
		got := CheckpointForContext(idx, tc.offset, tc.context)
		if got != tc.want {
			t.Errorf("%s: CheckpointForContext(%d, %d) = %+v, want %+v",
				tc.name, tc.offset, tc.context, got, tc.want)
		}
	}
}

func TestCheckpointForContext_NoIndex(t *testing.T) {
	t.Parallel()
	for _, idx := range []*rxtypes.UnifiedFileIndex{nil, {}} {
		if got := CheckpointForContext(idx, 5000, 3); got != (rxtypes.LineIndexEntry{}) {
			t.Errorf("CheckpointForContext(%v) = %+v, want the zero entry", idx, got)
		}
	}
}

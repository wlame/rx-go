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

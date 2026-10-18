package compressfile

import (
	"errors"
	"testing"
)

// The bound both parsers share: a frame holds at least one byte.
func TestCheckFrameSize(t *testing.T) {
	for bytes, wantErr := range map[int64]bool{-1: true, 0: true, 1: false, 4 << 20: false} {
		err := CheckFrameSize(bytes)
		if (err != nil) != wantErr {
			t.Errorf("CheckFrameSize(%d) = %v, want error %v", bytes, err, wantErr)
		}
		if err != nil && !errors.Is(err, ErrFrameSizeBelowMinimum) {
			t.Errorf("CheckFrameSize(%d) = %v, want ErrFrameSizeBelowMinimum", bytes, err)
		}
	}
}

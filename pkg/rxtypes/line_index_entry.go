package rxtypes

import (
	"encoding/json"
	"fmt"
)

// LineIndexEntry is a single checkpoint in a file index: a line number
// paired with the byte offset where that line starts.
//
// WHY a custom type with custom MarshalJSON: Python serializes these as
// 2-element arrays (e.g. [42, 1048576]) rather than objects. Using a
// struct lets us ship the same JSON shape while keeping a typed, named
// representation in Go code. Go's encoding/json has no idiom for
// "serialize a struct as a positional array", so we implement it here.
//
// A seekable-zstd checkpoint carries a third element: the index of the
// frame holding that line. It is what lets a lookup decompress one
// frame instead of walking the stream, and rx-python has always written
// it, so FrameIndex is a pointer — nil for a plain file, where the
// entry must stay two elements because both backends read them
// positionally.
type LineIndexEntry struct {
	LineNumber int64
	ByteOffset int64
	FrameIndex *int
}

// MarshalJSON emits [lineNumber, byteOffset], or
// [lineNumber, byteOffset, frameIndex] when the entry names a frame.
func (e LineIndexEntry) MarshalJSON() ([]byte, error) {
	if e.FrameIndex != nil {
		return json.Marshal([3]int64{e.LineNumber, e.ByteOffset, int64(*e.FrameIndex)})
	}
	return json.Marshal([2]int64{e.LineNumber, e.ByteOffset})
}

// UnmarshalJSON accepts either form. A third element becomes FrameIndex;
// a longer array is a format we do not know, and reading its first two
// elements as if we did would produce a wrong answer silently.
func (e *LineIndexEntry) UnmarshalJSON(data []byte) error {
	var arr []int64
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("LineIndexEntry: expected JSON array, got %s: %w", data, err)
	}
	if len(arr) < 2 || len(arr) > 3 {
		return fmt.Errorf("LineIndexEntry: expected 2 or 3 elements, got %d", len(arr))
	}
	e.LineNumber = arr[0]
	e.ByteOffset = arr[1]
	e.FrameIndex = nil
	if len(arr) == 3 {
		frame := int(arr[2])
		e.FrameIndex = &frame
	}
	return nil
}

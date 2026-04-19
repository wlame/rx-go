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
// For seekable-zstd files Python extends this to a 3-tuple
// [line, decompressed_offset, frame_index]. If we ever need that, add
// a second optional field with a conditional marshaller. For now the
// 2-tuple is what the spec requires.
type LineIndexEntry struct {
	LineNumber int64
	ByteOffset int64
}

// MarshalJSON emits the 2-element array form: [lineNumber, byteOffset].
func (e LineIndexEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int64{e.LineNumber, e.ByteOffset})
}

// UnmarshalJSON accepts either a 2-element array [line, offset] or a
// 3-element array [line, offset, frameIndex] (third element silently
// dropped — we don't model frame index in this type yet).
func (e *LineIndexEntry) UnmarshalJSON(data []byte) error {
	var arr []int64
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("LineIndexEntry: expected JSON array, got %s: %w", data, err)
	}
	if len(arr) < 2 {
		return fmt.Errorf("LineIndexEntry: expected at least 2 elements, got %d", len(arr))
	}
	e.LineNumber = arr[0]
	e.ByteOffset = arr[1]
	return nil
}

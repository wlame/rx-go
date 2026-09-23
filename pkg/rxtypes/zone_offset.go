package rxtypes

import (
	"encoding/json"
	"fmt"
)

// ZoneOffset is one change point of the zone offsets a file's lines
// write: from line Line on, every timestamped line writes
// OffsetMinutes (east of UTC), until the next change point. It is
// written as the pair [line, minutes], the way a line_index checkpoint
// is written as [line, offset].
type ZoneOffset struct {
	Line          int64
	OffsetMinutes int
}

// MarshalJSON emits [line, minutes].
func (z ZoneOffset) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int64{z.Line, int64(z.OffsetMinutes)})
}

// UnmarshalJSON accepts exactly a pair of whole numbers. A longer array
// is a format this build does not know, and reading its first two
// elements as if it did would give a wrong answer silently; a fraction
// is refused by the decoder of int64 itself.
func (z *ZoneOffset) UnmarshalJSON(data []byte) error {
	var pair []int64
	if err := json.Unmarshal(data, &pair); err != nil {
		return fmt.Errorf("ZoneOffset: expected a JSON array of two whole numbers, got %s: %w", data, err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("ZoneOffset: expected 2 elements, got %d", len(pair))
	}
	// The range of an offset is checked where the index is validated;
	// here the minutes need only fit an int, which any int64 does on the
	// 64-bit platforms rx is built for, and which the check below keeps
	// true on a 32-bit one.
	minutes := int(pair[1])
	if int64(minutes) != pair[1] {
		return fmt.Errorf("ZoneOffset: offset %d does not fit an int", pair[1])
	}
	z.Line, z.OffsetMinutes = pair[0], minutes
	return nil
}

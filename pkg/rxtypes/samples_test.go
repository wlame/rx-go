package rxtypes

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSamplesResponse_OffsetsFieldIsInt64 covers Stage 8 Reviewer 3
// High #14: the pre-fix SamplesResponse.Offsets / .Lines used plain
// `int`, which would silently truncate on 32-bit builds and represents
// a latent bug even on 64-bit (where int is 64-bit) because a reader
// of the struct cannot be sure of the precision.
//
// Every other byte-offset field in this package uses int64 (see
// index.go::CompressedOffset, trace.go::Offset / AbsoluteOffset,
// tracecache.go::Offset, line_index_entry.go::ByteOffset). The
// samples response is the outlier. Fix: align with the rest.
//
// The test uses reflection to pin the field type — any future
// regression back to `int` will fail here.
func TestSamplesResponse_OffsetsFieldIsInt64(t *testing.T) {
	r := SamplesResponse{}
	tp := reflect.TypeOf(r)

	offsetsField, ok := tp.FieldByName("Offsets")
	if !ok {
		t.Fatal("SamplesResponse has no Offsets field")
	}
	want := reflect.TypeOf(map[string]int64{})
	if offsetsField.Type != want {
		t.Errorf("Offsets type = %v, want %v (int→int64 for byte-offset precision parity with other rxtypes)",
			offsetsField.Type, want)
	}

	linesField, ok := tp.FieldByName("Lines")
	if !ok {
		t.Fatal("SamplesResponse has no Lines field")
	}
	if linesField.Type != want {
		t.Errorf("Lines type = %v, want %v (int→int64)", linesField.Type, want)
	}
}

// TestSamplesResponse_LargeOffsetRoundTrip verifies that a byte offset
// exceeding math.MaxInt32 survives a JSON round-trip. The pre-fix code
// truncated via `int(byteOffset)` at the handler layer; the post-fix
// code propagates int64 all the way to the wire type. This test pins
// the post-fix shape.
func TestSamplesResponse_LargeOffsetRoundTrip(t *testing.T) {
	// 2^33 exceeds int32 range but is valid int64.
	const bigOffset int64 = 1 << 33

	in := SamplesResponse{
		Offsets: map[string]int64{"2147483648": bigOffset},
		Lines:   map[string]int64{"100": bigOffset},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out SamplesResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Offsets["2147483648"] != bigOffset {
		t.Errorf("Offsets round-trip: got %d, want %d (int32 truncation would produce a small number)",
			out.Offsets["2147483648"], bigOffset)
	}
	if out.Lines["100"] != bigOffset {
		t.Errorf("Lines round-trip: got %d, want %d", out.Lines["100"], bigOffset)
	}
}

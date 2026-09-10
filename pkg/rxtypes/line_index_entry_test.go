package rxtypes

import (
	"encoding/json"
	"testing"
)

func TestLineIndexEntry_MarshalJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   LineIndexEntry
		want string
	}{
		{"zero", LineIndexEntry{}, `[0,0]`},
		{"typical", LineIndexEntry{LineNumber: 42, ByteOffset: 1048576}, `[42,1048576]`},
		{"large", LineIndexEntry{LineNumber: 9999999, ByteOffset: 1 << 40}, `[9999999,1099511627776]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLineIndexEntry_UnmarshalJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    LineIndexEntry
		wantErr bool
	}{
		{"two-tuple", `[42, 1048576]`, LineIndexEntry{LineNumber: 42, ByteOffset: 1048576}, false},
		{"three-tuple for seekable", `[10, 200, 3]`,
			LineIndexEntry{LineNumber: 10, ByteOffset: 200, FrameIndex: intPtr(3)}, false},
		{"zeros", `[0, 0]`, LineIndexEntry{}, false},
		{"too short", `[42]`, LineIndexEntry{}, true},
		{"too long", `[1, 2, 3, 4]`, LineIndexEntry{}, true},
		{"not array", `"foo"`, LineIndexEntry{}, true},
		{"empty array", `[]`, LineIndexEntry{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got LineIndexEntry
			err := json.Unmarshal([]byte(tc.input), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !entriesEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLineIndexEntry_RoundTrip(t *testing.T) {
	t.Parallel()
	original := []LineIndexEntry{
		{LineNumber: 1, ByteOffset: 0},
		{LineNumber: 100, ByteOffset: 4096},
		{LineNumber: 1000, ByteOffset: 65536},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `[[1,0],[100,4096],[1000,65536]]`
	if string(data) != want {
		t.Errorf("marshal output: got %s, want %s", data, want)
	}
	var decoded []LineIndexEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded) != len(original) {
		t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(original))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("[%d]: got %+v, want %+v", i, decoded[i], original[i])
		}
	}
}

// A seekable-zstd checkpoint carries the frame that holds the line, so
// a lookup can decompress one frame instead of walking the file. The
// third element is what makes that possible, and rx-python has always
// written it — rx-go used to drop it on read and never emit it, so an
// index it wrote could not be used as a frame table by either backend.
func TestLineIndexEntry_ThreeElementFormRoundTrips(t *testing.T) {
	frame := 7
	entry := LineIndexEntry{LineNumber: 1362, ByteOffset: 65582, FrameIndex: &frame}

	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "[1362,65582,7]" {
		t.Errorf("encoded: got %s, want [1362,65582,7]", encoded)
	}

	var decoded LineIndexEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.FrameIndex == nil || *decoded.FrameIndex != frame {
		t.Errorf("frame index: got %v, want %d", decoded.FrameIndex, frame)
	}
	if decoded.LineNumber != entry.LineNumber || decoded.ByteOffset != entry.ByteOffset {
		t.Errorf("round trip lost data: %+v", decoded)
	}
}

// A plain-text checkpoint has no frame, and must still be two elements:
// rx-python reads them positionally.
func TestLineIndexEntry_TwoElementFormIsUnchanged(t *testing.T) {
	encoded, err := json.Marshal(LineIndexEntry{LineNumber: 42, ByteOffset: 1048576})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "[42,1048576]" {
		t.Errorf("encoded: got %s, want [42,1048576]", encoded)
	}

	var decoded LineIndexEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.FrameIndex != nil {
		t.Errorf("frame index: got %v, want nil", decoded.FrameIndex)
	}
}

// entriesEqual compares two entries including the optional frame index,
// which a plain == cannot do through the pointer.
func entriesEqual(a, b LineIndexEntry) bool {
	if a.LineNumber != b.LineNumber || a.ByteOffset != b.ByteOffset {
		return false
	}
	switch {
	case a.FrameIndex == nil && b.FrameIndex == nil:
		return true
	case a.FrameIndex == nil || b.FrameIndex == nil:
		return false
	default:
		return *a.FrameIndex == *b.FrameIndex
	}
}

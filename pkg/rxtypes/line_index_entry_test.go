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
		{"two-tuple", `[42, 1048576]`, LineIndexEntry{42, 1048576}, false},
		{"three-tuple for seekable", `[10, 200, 3]`, LineIndexEntry{10, 200}, false},
		{"zeros", `[0, 0]`, LineIndexEntry{0, 0}, false},
		{"too short", `[42]`, LineIndexEntry{}, true},
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
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLineIndexEntry_RoundTrip(t *testing.T) {
	t.Parallel()
	original := []LineIndexEntry{{1, 0}, {100, 4096}, {1000, 65536}}
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

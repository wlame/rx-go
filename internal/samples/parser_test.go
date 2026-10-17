package samples

import (
	"strings"
	"testing"
)

// TestParseLines_SingleValues covers the happy path for plain integers
// (positive and negative singles).
func TestParseLines_SingleValues(t *testing.T) {
	cases := []struct {
		in    string
		want  int64
		isNeg bool
	}{
		{"100", 100, false},
		{"0", 0, false},
		{"-5", -5, true},
		{" 42 ", 42, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLines(tc.in)
			if err != nil {
				t.Fatalf("ParseLines(%q): %v", tc.in, err)
			}
			if len(got) != 1 {
				t.Fatalf("ParseLines(%q): got %d items, want 1", tc.in, len(got))
			}
			if got[0].Start != tc.want {
				t.Errorf("Start: got %d, want %d", got[0].Start, tc.want)
			}
			if got[0].IsRange() {
				t.Errorf("single should not be a range")
			}
		})
	}
}

// TestParseLines_Ranges covers "100-200" style input.
func TestParseLines_Ranges(t *testing.T) {
	got, err := ParseLines("100-200,300-301,0-0")
	if err != nil {
		t.Fatalf("ParseLines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	// Verify each range.
	expected := []struct {
		start int64
		end   int64
	}{{100, 200}, {300, 301}, {0, 0}}
	for i, want := range expected {
		if !got[i].IsRange() {
			t.Errorf("got[%d] should be a range", i)
			continue
		}
		if got[i].Start != want.start || *got[i].End != want.end {
			t.Errorf("got[%d] = %d-%d, want %d-%d",
				i, got[i].Start, *got[i].End, want.start, want.end)
		}
	}
}

// TestParseLines_MultiRangeMixed
// a single call can mix singles and ranges like "200-350,450-600,1000".
func TestParseLines_MultiRangeMixed(t *testing.T) {
	got, err := ParseLines("200-350,450-600,1000")
	if err != nil {
		t.Fatalf("ParseLines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	if !got[0].IsRange() || got[0].Start != 200 || *got[0].End != 350 {
		t.Errorf("item 0 wrong: %+v", got[0])
	}
	if !got[1].IsRange() || got[1].Start != 450 || *got[1].End != 600 {
		t.Errorf("item 1 wrong: %+v", got[1])
	}
	if got[2].IsRange() || got[2].Start != 1000 {
		t.Errorf("item 2 wrong: %+v", got[2])
	}
}

// TestParseLines_Errors covers rejected inputs: a mixed range (one end
// counted from the start, the other from the end), a reversed one, and
// text that is no position. Each error names a line, never an offset.
func TestParseLines_Errors(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"abc",
		"1-2-3",
		"1-abc",
		"-1-5",     // mixed: from the end, then from the start
		"5--10",    // mixed: from the start, then from the end
		"10-5",     // start > end
		"-1--3",    // reversed: -3 is before -1
		"100,,200", // empty middle token
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParseLines(in)
			if err == nil {
				t.Fatalf("ParseLines(%q): expected error, got nil", in)
			}
			if strings.Contains(err.Error(), "offset") {
				t.Errorf("ParseLines(%q): error names an offset: %v", in, err)
			}
		})
	}
}

// A range of lines counted from the end (-100--1: the last hundred
// lines) is accepted in lines mode; both ends are negative and the
// start is before the end, as in a positive range.
func TestParseLines_NegativeRanges(t *testing.T) {
	got, err := ParseLines("-100--1,-3--3,5-9")
	if err != nil {
		t.Fatalf("ParseLines: %v", err)
	}
	want := []struct{ start, end int64 }{{-100, -1}, {-3, -3}, {5, 9}}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i, w := range want {
		if !got[i].IsRange() || got[i].Start != w.start || *got[i].End != w.end {
			t.Errorf("got[%d] = %+v, want %d-%d", i, got[i], w.start, w.end)
		}
	}
}

// The error for a bad line names a line; the error for a bad offset
// names an offset: the two flags of rx samples are inverses, and a user
// who typed --lines must not be told about offsets.
func TestParseLines_ErrorsNameLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "invalid line: abc"},
		{"1-abc", "invalid line range: 1-abc"},
		{"-1--3", "invalid line range: -1--3"},
		{"5--1", "invalid line range: 5--1"},
	}
	for _, tc := range cases {
		_, err := ParseLines(tc.in)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("ParseLines(%q) = %v, want a prefix %q", tc.in, err, tc.want)
		}
	}
}

// Offsets keep their grammar: a single negative offset counts from the
// end, a negative range is refused, and every error names an offset.
func TestParseOffsets_RefusesNegativeRangesAndNamesOffsets(t *testing.T) {
	if got, err := ParseOffsets("100-200,-5"); err != nil || len(got) != 2 || got[1].Start != -5 {
		t.Fatalf("ParseOffsets(100-200,-5) = %+v, %v", got, err)
	}
	cases := []struct{ in, want string }{
		{"abc", "invalid offset: abc"},
		{"-3--1", "invalid offset range: -3--1"},
		{"-1-5", "invalid offset range: -1-5"},
		{"10-5", "invalid offset range: 10-5"},
	}
	for _, tc := range cases {
		_, err := ParseOffsets(tc.in)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("ParseOffsets(%q) = %v, want a prefix %q", tc.in, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "line") {
			t.Errorf("ParseOffsets(%q): error names a line: %v", tc.in, err)
		}
	}
}

// TestOffsetOrRange_Key covers the string key used as the map key in
// SamplesResponse.Samples / .Offsets / .Lines.
func TestOffsetOrRange_Key(t *testing.T) {
	end200 := int64(200)
	cases := []struct {
		v    OffsetOrRange
		want string
	}{
		{OffsetOrRange{Start: 100}, "100"},
		{OffsetOrRange{Start: -5}, "-5"},
		{OffsetOrRange{Start: 100, End: &end200}, "100-200"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.v.Key(); got != tc.want {
				t.Errorf("Key() = %q, want %q", got, tc.want)
			}
		})
	}
}

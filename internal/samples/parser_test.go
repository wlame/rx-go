package samples

import (
	"testing"
)

// TestParseCSV_SingleValues covers the happy path for plain integers
// (positive and negative singles).
func TestParseCSV_SingleValues(t *testing.T) {
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
			got, err := ParseCSV(tc.in)
			if err != nil {
				t.Fatalf("ParseCSV(%q): %v", tc.in, err)
			}
			if len(got) != 1 {
				t.Fatalf("ParseCSV(%q): got %d items, want 1", tc.in, len(got))
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

// TestParseCSV_Ranges covers "100-200" style input.
func TestParseCSV_Ranges(t *testing.T) {
	got, err := ParseCSV("100-200,300-301,0-0")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
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

// TestParseCSV_MultiRangeMixed — Stage 9 Round 2 R1-B4 user design:
// a single call can mix singles and ranges like "200-350,450-600,1000".
func TestParseCSV_MultiRangeMixed(t *testing.T) {
	got, err := ParseCSV("200-350,450-600,1000")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
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

// TestParseCSV_Errors covers rejected inputs.
func TestParseCSV_Errors(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"abc",
		"1-2-3",
		"1-abc",
		"-1-5",     // negative range
		"5--10",    // malformed
		"10-5",     // start > end
		"100,,200", // empty middle token
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParseCSV(in)
			if err == nil {
				t.Errorf("ParseCSV(%q): expected error, got nil", in)
			}
		})
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

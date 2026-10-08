package rxtypes

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// An id's number decides its place, so f10 follows f2 and p11 follows
// p9; an id rx could not have made goes after every id it could.
func TestCompareIDs_OrdersByNumber(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"f2", "f10", -1},
		{"f10", "f2", 1},
		{"p9", "p11", -1},
		{"p1", "p1", 0},
		{"f12", "f12", 0},
		// The letter comes first: two kinds of id never interleave.
		{"c2", "f1", -1},
		// Ids rx never makes: a leading zero, no digits, a number too
		// large for an int, other characters. They follow every id rx
		// makes and are ordered among themselves as text.
		{"f99", "f01", -1},
		{"f99", "f", -1},
		{"f1", "f99999999999999999999", -1},
		{"f99", "f1x", -1},
		{"f1", "", -1},
		{"F1", "f1", 1},
		{"f1x", "f2x", -1},
		{"", "f1x", -1},
	}
	for _, tc := range cases {
		if got := sign(CompareIDs(tc.a, tc.b)); got != tc.want {
			t.Errorf("CompareIDs(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// A sort is only correct when its comparison is a total order. Mixing
// number order with text order can break that (f2 < f10 by number,
// f10 < f1x and f1x < f2 by text), so every triple of a set that mixes
// both kinds is checked.
func TestCompareIDs_IsATotalOrder(t *testing.T) {
	t.Parallel()
	ids := []string{"f1", "f2", "f10", "f11", "f1x", "f01", "f", "", "p3", "p30", "c7", "x", "f99999999999999999999"}
	for _, a := range ids {
		if CompareIDs(a, a) != 0 {
			t.Errorf("CompareIDs(%q, %q) != 0", a, a)
		}
		for _, b := range ids {
			if sign(CompareIDs(a, b)) != -sign(CompareIDs(b, a)) {
				t.Errorf("CompareIDs(%q, %q) and CompareIDs(%q, %q) disagree", a, b, b, a)
			}
			for _, c := range ids {
				if CompareIDs(a, b) < 0 && CompareIDs(b, c) < 0 && CompareIDs(a, c) >= 0 {
					t.Errorf("%q < %q < %q but not %q < %q", a, b, c, a, c)
				}
			}
		}
	}
}

func TestCompareIDs_SortsTwelveFileIDs(t *testing.T) {
	t.Parallel()
	got := []string{"f1", "f10", "f11", "f12", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9"}
	slices.SortFunc(got, CompareIDs)
	want := []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12"}
	if !slices.Equal(got, want) {
		t.Errorf("sorted %v, want %v", got, want)
	}
}

// sign reduces a comparison result to -1, 0 or 1.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Reading the number of an id allocates nothing, whatever the id holds:
// an id far longer than any number, and one whose 19 digits are past
// the largest int, are refused without the error value strconv makes.
// A sort of many matches compares ids on every step.
func TestIDNumber_AllocatesNothingForAnyID(t *testing.T) {
	ids := []string{
		"f12",
		"f9223372036854775807",          // the largest int64: well formed
		"f9223372036854775808",          // 19 digits, one past it
		"f99999999999999999999",         // 20 digits
		"f" + strings.Repeat("7", 4096), // far longer than any number
	}
	for _, id := range ids {
		if allocs := testing.AllocsPerRun(100, func() { _, _ = idNumber(id) }); allocs != 0 {
			t.Errorf("idNumber(%.24q…) allocates %v times per call, want 0", id, allocs)
		}
	}
}

// A number past the int range is not well formed, and the largest int
// is; the check on the length does not move that border.
func TestIDNumber_TheIntRangeIsTheBorder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id         string
		want       int
		wellFormed bool
	}{
		{"f" + strconv.Itoa(math.MaxInt), math.MaxInt, true},
		{"f" + strconv.FormatUint(uint64(math.MaxInt)+1, 10), 0, false},
		{"f99999999999999999999", 0, false},
		{"f" + strings.Repeat("1", 40), 0, false},
		{"f1", 1, true},
	}
	for _, tc := range cases {
		got, ok := idNumber(tc.id)
		if got != tc.want || ok != tc.wellFormed {
			t.Errorf("idNumber(%q) = %d, %v; want %d, %v", tc.id, got, ok, tc.want, tc.wellFormed)
		}
	}
}

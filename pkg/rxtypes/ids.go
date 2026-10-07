package rxtypes

import (
	"cmp"
	"strconv"
	"strings"
)

// CompareIDs orders two of the ids an answer uses to name its files and
// patterns ("f1", "f2", …, "p1", "p2", …) by the number in them, so
// "f2" comes before "f10" and "p9" before "p11". It returns a negative
// number when a goes first, a positive one when b does, and 0 when they
// are the same id, the way strings.Compare does, so it can be handed
// straight to slices.SortFunc.
//
// rx numbers files in the order the paths were given or walked, and
// patterns in the order they were given. Ordering the ids as text
// would put "f10" before "f2"; ordering them by number keeps that
// order, which is the order a person sees the matches in.
//
// An id is well formed when it is one lowercase ASCII letter followed
// by a decimal number without a leading zero that fits in an int. Ids
// of different letters are ordered by the letter first, so two kinds of
// id never interleave.
//
// rx makes every id it compares, so an id that is not well formed is a
// programming error. It still gets a place: after every well-formed id,
// in text order among the ones like it. Ordering it by text against a
// well-formed id too would not be a total order (f2 < f10 by number,
// f10 < f1x and f1x < f2 by text), and a sort needs a total order to
// give a defined result.
func CompareIDs(a, b string) int {
	// Equal ids are the common case in a sort of matches (many matches
	// in one file), and need no parsing.
	if a == b {
		return 0
	}
	aNumber, aWellFormed := idNumber(a)
	bNumber, bWellFormed := idNumber(b)
	switch {
	case aWellFormed && bWellFormed:
		if byLetter := cmp.Compare(a[0], b[0]); byLetter != 0 {
			return byLetter
		}
		// Two well-formed ids of one letter and one number are the same
		// text, so the number alone decides here.
		return cmp.Compare(aNumber, bNumber)
	case aWellFormed:
		return -1
	case bWellFormed:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// idNumber returns the number in a well-formed id: 10 for "f10", 3 for
// "p3". The second result is false, and the number 0, for any other
// string: an empty one, a first byte that is not a lowercase ASCII
// letter, no digits after it, a leading zero, any other character, or a
// number too large for an int.
//
// It reads the string in place and allocates nothing, so a sort of a
// million matches can call it on every comparison.
func idNumber(id string) (int, bool) {
	if len(id) < 2 || id[0] < 'a' || id[0] > 'z' {
		return 0, false
	}
	digits := id[1:]
	// A leading zero would give "f01" and "f1" the same number while
	// they are different ids; rx never writes one.
	if digits[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	// Every byte is a digit, so the only way Atoi can fail now is a
	// number past the int range; such an id is not well formed.
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

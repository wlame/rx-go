package sigscan

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/signatures"
)

// Measures are signatures written as Go code with parameters
// (signatures.MeasureSpec). Each one is run by the machinery its
// findings resemble, so it gets the same chunk edges, the same cap and
// the same times as the other kinds:
//
//   - entropy and base64-run find a value on one line, as a regex
//     signature does (valueMatcher, addValueFinding);
//   - binary and unprefixed find runs of qualifying lines, as a run
//     signature does (runBinary, runUnprefixed in run.go);
//   - json-block is a block, opened and closed by a bracket count
//     instead of regexes (jsonblock.go);
//   - long-line decides each line against the lengths around it, with
//     a merge of its own across chunk edges (longline.go);
//   - time-gap and time-backwards compare each stamped line with the
//     stamped line before it (timegaps.go), and density counts the
//     lines in time buckets (density.go); the merge pairs and adds up
//     what the chunks report at their edges.
//
// In a file without a timestamp format the time measures find nothing,
// and density keeps no series.

// The version of each measure's code. A version enters the key of every
// cached finding of a signature that uses the measure, so a change that
// can change any answer of a measure bumps its version.
const (
	longLineVersion      = 1
	binaryVersion        = 1
	entropyVersion       = 2
	jsonBlockVersion     = 1
	unprefixedVersion    = 1
	base64RunVersion     = 1
	densityVersion       = 1
	timeGapVersion       = 1
	timeBackwardsVersion = 1
)

// measureVersions maps each measure name to its version.
var measureVersions = map[string]int{
	"long-line":      longLineVersion,
	"binary":         binaryVersion,
	"entropy":        entropyVersion,
	"json-block":     jsonBlockVersion,
	"unprefixed":     unprefixedVersion,
	"base64-run":     base64RunVersion,
	"density":        densityVersion,
	"time-gap":       timeGapVersion,
	"time-backwards": timeBackwardsVersion,
}

// MeasureVersion returns the version of the named measure's code, which
// enters the key of every cached finding of a signature that uses it;
// 0 for a name that is no measure.
func MeasureVersion(name string) int {
	return measureVersions[name]
}

// compileMeasure sets up the matcher of a measure signature.
func compileMeasure(cs *compiledSig, sig *signatures.Signature) error {
	if sig.Measure == nil {
		return fmt.Errorf("signature %s: a measure without a definition", sig.ID)
	}
	switch p := sig.Measure.Params.(type) {
	case signatures.EntropyParams, signatures.Base64RunParams:
		cs.value = newValueMatcher(p)
	case signatures.BinaryParams:
		cs.run = &runProgram{mode: runBinary, min: 1}
	case signatures.UnprefixedParams:
		cs.run = &runProgram{mode: runUnprefixed, min: int64(p.Min)}
		cs.minShare = p.MinShare
	case signatures.JSONBlockParams:
		cs.block = compileJSONBlock(p)
	case signatures.LongLineParams:
		cs.long = &longLineSpec{window: int64(p.Window), factor: int64(p.Factor), minBytes: int64(p.MinBytes)}
	case signatures.TimeBackwardsParams:
		cs.back = &backSpec{tolerance: p.Tolerance.Milliseconds()}
	case signatures.TimeGapParams:
		cs.gap = &gapSpec{minGap: p.MinGap.Milliseconds(), factor: p.Factor}
	case signatures.DensityParams:
		cs.density = &densitySpec{grid: densityGrid{explicit: p.Bucket.Milliseconds()}, factor: p.Factor, z: p.Z,
			minCount: int64(p.MinCount)}
	default:
		return fmt.Errorf("signature %s: the measure %s cannot be scanned by this build", sig.ID, sig.Measure.Name)
	}
	return nil
}

// valueMatcher finds the values a value measure looks for on a line
// view: high-entropy tokens, or base64 runs. Both are byte scans, never
// a regex, and linear in the view.
type valueMatcher interface {
	// next returns the first value of view that starts at or after
	// from, and false when there is none.
	next(view []byte, from int) (span, bool)
}

// newValueMatcher returns the matcher of entropy or base64-run
// parameters, nil for any other.
func newValueMatcher(p signatures.MeasureParams) valueMatcher {
	switch p := p.(type) {
	case signatures.EntropyParams:
		return entropyMatcher{minLength: p.MinLength, ratio: p.Ratio, hexRatio: p.HexRatio}
	case signatures.Base64RunParams:
		return base64Matcher{minLength: p.MinLength}
	}
	return nil
}

// appendValues appends every value of view to dst, in order, at most
// limit + 1 of them (more than limit means the line has more).
func appendValues(dst []span, m valueMatcher, view []byte, limit int) []span {
	for from, n := 0, 0; n <= limit; n++ {
		v, ok := m.next(view, from)
		if !ok {
			break
		}
		dst = append(dst, v)
		from = v[1]
	}
	return dst
}

// byteSet is a set of byte values, for byte scans.
type byteSet [256]bool

// newByteSet returns the set of the bytes of each range "a-z" in
// ranges and of every other byte listed.
func newByteSet(ranges ...string) *byteSet {
	var s byteSet
	for _, r := range ranges {
		if len(r) == 3 && r[1] == '-' {
			for c := int(r[0]); c <= int(r[2]); c++ {
				s[c] = true
			}
			continue
		}
		for i := 0; i < len(r); i++ {
			s[r[i]] = true
		}
	}
	return &s
}

// The byte classes of the value measures.
var (
	// tokenBytes are the bytes of an entropy token: [A-Za-z0-9+/=_-].
	tokenBytes = newByteSet("A-Z", "a-z", "0-9", "+/=_-")
	// base64Bytes are the base64 alphabet without its padding.
	base64Bytes = newByteSet("A-Z", "a-z", "0-9", "+/")
	// hexBytes are hex digits, either case.
	hexBytes = newByteSet("0-9", "a-f", "A-F")
	// digitBytes are decimal digits.
	digitBytes = newByteSet("0-9")
)

// runOf returns where the run of bytes of set that starts at i ends.
func runOf(view []byte, i int, set *byteSet) int {
	for i < len(view) && set[view[i]] {
		i++
	}
	return i
}

// entropyMatcher finds tokens of high entropy: maximal runs of
// [A-Za-z0-9+/=_-] of at least minLength bytes whose Shannon entropy,
// in bits per byte, reaches a share of the entropy a random token of
// the same length has on average (randomEntropy). A token of hex digits
// only is held to hexRatio of a random hex token's entropy, and is
// never found when hexRatio is 0: a hash or a request id is as random
// as a hex key, so no statistic tells them apart. Any other token is
// held to ratio of a random base64 token's entropy and needs an
// upper-case letter, a lower-case letter and a digit, which words,
// identifiers and most paths lack. A UUID and a token of digits only
// are skipped: they are ids and numbers, not secrets.
//
// The share is taken of the average entropy at the token's length,
// not of the most a token of that length could have (log2 of its
// length or of the alphabet's size): a random 64-character token
// reaches only about 86% of that maximum on average, against 93% at 24
// characters, so one share of the maximum cannot find most random keys
// at both lengths.
type entropyMatcher struct {
	minLength       int
	ratio, hexRatio float64
}

func (m entropyMatcher) next(view []byte, from int) (span, bool) {
	for i := from; i < len(view); {
		if !tokenBytes[view[i]] {
			i++
			continue
		}
		end := runOf(view, i, tokenBytes)
		if end-i >= m.minLength && m.qualifies(view[i:end]) {
			return span{i, end}, true
		}
		i = end
	}
	return span{}, false
}

// qualifies reports whether a token is a finding of the measure.
func (m entropyMatcher) qualifies(token []byte) bool {
	if isUUID(token) || onlyBytesOf(token, digitBytes) {
		return false
	}
	if onlyBytesOf(token, hexBytes) {
		return m.hexRatio > 0 && shannonEntropy(token) >= m.hexRatio*randomEntropy(hexSymbols, len(token))
	}
	return holdsLetterCasesAndDigit(token) && shannonEntropy(token) >= m.ratio*randomEntropy(base64Symbols, len(token))
}

// holdsLetterCasesAndDigit reports whether b holds an upper-case
// letter, a lower-case letter and a digit.
func holdsLetterCasesAndDigit(b []byte) bool {
	var upper, lower, digit bool
	for _, c := range b {
		switch {
		case 'A' <= c && c <= 'Z':
			upper = true
		case 'a' <= c && c <= 'z':
			lower = true
		case '0' <= c && c <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// onlyBytesOf reports whether every byte of b is in set.
func onlyBytesOf(b []byte, set *byteSet) bool {
	return runOf(b, 0, set) == len(b)
}

// isUUID reports whether b is a UUID: 32 hex digits in groups of 8, 4,
// 4, 4 and 12, joined by dashes.
func isUUID(b []byte) bool {
	if len(b) != 36 {
		return false
	}
	for i, c := range b {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !hexBytes[c] {
				return false
			}
		}
	}
	return true
}

// base64Matcher finds base64 runs: a maximal run of the base64 alphabet
// (A-Z a-z 0-9 + /) with up to two '=' of padding after it, at least
// minLength bytes long with its padding.
type base64Matcher struct {
	minLength int
}

func (m base64Matcher) next(view []byte, from int) (span, bool) {
	for i := from; i < len(view); {
		if !base64Bytes[view[i]] {
			i++
			continue
		}
		end := runOf(view, i, base64Bytes)
		for pad := 0; pad < 2 && end < len(view) && view[end] == '='; pad++ {
			end++
		}
		if end-i >= m.minLength {
			return span{i, end}, true
		}
		i = end
	}
	return span{}, false
}

// holdsBinary reports whether a line view holds a NUL byte or bytes that
// are not UTF-8: a line the binary measure counts. cut says the view is
// shorter than its line; a character the cut splits is then not
// counted, since the rest of its bytes are in the line.
func holdsBinary(view []byte, cut bool) bool {
	if bytes.IndexByte(view, 0) >= 0 {
		return true
	}
	if utf8.Valid(view) {
		return false
	}
	return !cut || !utf8.Valid(withoutCutCharacter(view))
}

// withoutCutCharacter returns view without its last character when the
// view's end cuts that character short: its first bytes are a valid
// start of a character that needs more bytes than the view has.
func withoutCutCharacter(view []byte) []byte {
	for k := 1; k < utf8.UTFMax && k <= len(view); k++ {
		start := len(view) - k
		if utf8.RuneStart(view[start]) {
			if utf8.FullRune(view[start:]) {
				return view
			}
			return view[:start]
		}
	}
	return view
}

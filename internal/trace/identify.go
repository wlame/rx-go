package trace

import (
	"regexp"
	"regexp/syntax"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// IdentifyMatchingPatterns is the 2-phase pattern-identification step.
//
// Context — why do we need this at all?
// ripgrep accepts multiple `-e PAT` patterns but its --json output
// doesn't report WHICH pattern matched a given line. When we give rg
// three regexps {p1: "foo", p2: "bar", p3: "baz"} and a line matches
// both "foo" and "bar", we need to know so the API response lists
// both. So: for each matched line, we re-run each pattern against the
// line text and the submatch set, and collect the patterns that
// reproduce the match.
//
// Algorithm (matches rx-python/src/rx/trace_worker.py::identify_matching_patterns):
//
//   - If submatches is empty (cache-reconstruction path), validate each
//     pattern against the line text directly; return all that match.
//   - Otherwise, compute the set of matched-text strings from submatches,
//     then for each pattern check if ANY of that pattern's findall
//     results intersect the submatch set. Patterns that do are kept.
//
// Flag handling: the pattern is re-run the way ripgrep ran it, so -i, -w,
// -x and -F from rgExtraArgs shape the Go regexp (see compileLikeRipgrep).
//
// INVARIANT: a line ripgrep reported is never dropped here. Go's regexp
// cannot always reproduce what rg matched — a PCRE2 look-around under
// -P, or Rust-only regex syntax, does not compile in Go — so when no
// pattern reproduces the line, the line is credited to the patterns Go
// could not check, and failing that to every pattern. A match labeled
// with too many patterns is a smaller error than a match that vanishes.
//
// patternOrder carries the canonical pattern-ID order ("p1", "p2", ...).
// We iterate in this order so the returned slice is deterministic
// (Go map iteration is randomized).
//
// With submatches the result is never empty. Without them (the line is
// being checked rather than reported by rg) it is empty when no pattern
// matches, and callers may treat that as a stale-cache hit.
func IdentifyMatchingPatterns(
	lineText string,
	submatches []rxtypes.Submatch,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
) []string {
	flags := matchFlagsFrom(rgExtraArgs)

	if len(submatches) == 0 {
		return identifyByFullLineMatch(lineText, patternIDs, patternOrder, flags)
	}

	// Build the set of matched text strings for intersection tests.
	matchedTexts := make(map[string]struct{}, len(submatches))
	for _, sm := range submatches {
		matchedTexts[sm.Text] = struct{}{}
	}

	reproduced := make([]string, 0, len(patternOrder))
	var unverifiable []string
	for _, pid := range patternOrder {
		patStr, ok := patternIDs[pid]
		if !ok {
			continue
		}
		re, err := compileLikeRipgrep(patStr, flags)
		if err != nil {
			// rg accepted this pattern (it reported the line), Go cannot
			// parse it, so whether it matched is unknown, not "no".
			unverifiable = append(unverifiable, pid)
			continue
		}
		// findall against the line; any overlap with matchedTexts wins.
		for _, loc := range re.FindAllStringIndex(lineText, -1) {
			if _, inSet := matchedTexts[lineText[loc[0]:loc[1]]]; inSet {
				reproduced = append(reproduced, pid)
				break
			}
		}
	}

	switch {
	case len(reproduced) > 0:
		return reproduced
	case len(unverifiable) > 0:
		return unverifiable
	default:
		return knownPatternIDs(patternIDs, patternOrder)
	}
}

// identifyByFullLineMatch is the submatch-less fallback path.
// Each pattern is tested against the whole line; every pattern that
// finds at least one match is returned. When none does, the patterns Go
// cannot compile are returned, since they cannot be ruled out; the
// result is empty only when every pattern compiled and none matched.
func identifyByFullLineMatch(
	lineText string,
	patternIDs map[string]string,
	patternOrder []string,
	flags matchFlags,
) []string {
	matched := make([]string, 0, len(patternOrder))
	var unverifiable []string
	for _, pid := range patternOrder {
		patStr, ok := patternIDs[pid]
		if !ok {
			continue
		}
		re, err := compileLikeRipgrep(patStr, flags)
		if err != nil {
			unverifiable = append(unverifiable, pid)
			continue
		}
		if re.MatchString(lineText) {
			matched = append(matched, pid)
		}
	}
	if len(matched) == 0 && len(unverifiable) > 0 {
		return unverifiable
	}
	return matched
}

// knownPatternIDs returns the IDs in patternOrder that patternIDs knows,
// in that order.
func knownPatternIDs(patternIDs map[string]string, patternOrder []string) []string {
	out := make([]string, 0, len(patternOrder))
	for _, pid := range patternOrder {
		if _, ok := patternIDs[pid]; ok {
			out = append(out, pid)
		}
	}
	return out
}

// matchFlags is the set of ripgrep options that change which text a
// pattern matches. It is a bit set: each constant below is one bit, and
// a request's flags are OR-ed together.
type matchFlags uint8

const (
	matchIgnoreCase  matchFlags = 1 << iota // -i: case-insensitive
	matchWholeWord                          // -w: match bounded by word boundaries
	matchWholeLine                          // -x: match is the whole line
	matchFixedString                        // -F: pattern is literal text
)

// has reports whether every bit of flag is set in f.
func (f matchFlags) has(flag matchFlags) bool { return f&flag == flag }

// ripgrepFlagMeaning maps each ripgrep spelling to the bit it sets.
//
// A flag missing from this table changes how ripgrep runs a pattern but
// not the text a match covers — -P picks the PCRE2 engine, for one — so
// re-running the pattern in Go needs nothing from it. Callers pass one
// flag per element ("-i", "-w"), never a bundle like "-iw".
var ripgrepFlagMeaning = map[string]matchFlags{
	"-i":              matchIgnoreCase,
	"--ignore-case":   matchIgnoreCase,
	"-w":              matchWholeWord,
	"--word-regexp":   matchWholeWord,
	"-x":              matchWholeLine,
	"--line-regexp":   matchWholeLine,
	"-F":              matchFixedString,
	"--fixed-strings": matchFixedString,
}

// matchFlagsFrom reads the matching flags out of ripgrep arguments.
// Arguments the table does not know contribute nothing.
func matchFlagsFrom(rgArgs []string) matchFlags {
	var flags matchFlags
	for _, arg := range rgArgs {
		flags |= ripgrepFlagMeaning[arg]
	}
	return flags
}

// compileLikeRipgrep compiles pattern into a Go regexp that matches what
// ripgrep matches under flags.
//
//   - -F quotes the pattern, so `foo(` and `a.b` are literal text.
//   - -x anchors it to the whole line. ripgrep lets -x override -w, and
//     so does this.
//   - -w wraps it in `\b`, which agrees with ripgrep for any match that
//     starts and ends on a word character. ripgrep's own rule also
//     accepts a match whose edge is not a word character; such a match
//     fails here and is caught by the caller's never-drop fallback.
//   - -i adds `(?i)` unless the pattern already sets case folding
//     itself. Under -F an inline flag is literal text, so -i always
//     applies.
//
// Go's regexp is RE2 syntax and ripgrep's default engine is Rust's
// `regex` crate, which agree on almost everything; a PCRE2 pattern under
// -P often fails to compile here, and the caller treats that as "cannot
// tell", never as "did not match".
func compileLikeRipgrep(pattern string, flags matchFlags) (*regexp.Regexp, error) {
	// Decided on the pattern as the caller wrote it, before quoting or
	// wrapping hides its inline flags.
	foldCase := flags.has(matchIgnoreCase) &&
		(flags.has(matchFixedString) || !hasInlineFlag(pattern, syntax.FoldCase))

	if flags.has(matchFixedString) {
		pattern = regexp.QuoteMeta(pattern)
	}
	switch {
	case flags.has(matchWholeLine):
		pattern = `^(?:` + pattern + `)$`
	case flags.has(matchWholeWord):
		pattern = `\b(?:` + pattern + `)\b`
	}
	if foldCase {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// hasInlineFlag parses the pattern just far enough to see whether it
// already opts into the given syntax flag. Returns false on parse
// errors (the regex is broken either way; the caller surfaces that).
func hasInlineFlag(pattern string, flag syntax.Flags) bool {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return false
	}
	return parsed.Flags&flag != 0
}
